#!/bin/sh
# Deploy a released binary onto this host, then prove it works — and undo it if
# it does not (ADR 0003, Operate).
#
#   ./deploy.sh                    # latest release
#   ./deploy.sh v0.1.0             # specific tag
#   DEPLOY_DIR=~/bin ./deploy.sh   # install dir (default /usr/local/bin)
#   HEALTHZ=http://127.0.0.1:8080/healthz ./deploy.sh   # smoke target
#   ASSET_URL=file:///path/to/asset.tar.gz ./deploy.sh  # skip release lookup
#
# The previous binary is kept as <bin>.prev precisely so a failed smoke can put
# it back. A deploy that cannot verify itself is a deploy that will be verified
# by your users.
set -eu
REPO=${REPO:-jamesponwith/flywheel-template}
BIN=${BIN:-flywheel-template}
DIR=${DEPLOY_DIR:-/usr/local/bin}
VER=${1:-latest}
HEALTHZ=${HEALTHZ:-}
SMOKE_TIMEOUT=${SMOKE_TIMEOUT:-30}
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)
case "$ARCH" in x86_64) ARCH=amd64 ;; aarch64 | arm64) ARCH=arm64 ;; esac

URL=${ASSET_URL:-}
if [ -z "$URL" ]; then
	if [ "$VER" = latest ]; then
		API="https://api.github.com/repos/$REPO/releases/latest"
	else
		API="https://api.github.com/repos/$REPO/releases/tags/$VER"
	fi
	URL=$(curl -fsSL "$API" | grep -o "https://[^\"]*_${OS}_${ARCH}\.tar\.gz" | head -1)
	[ -n "$URL" ] || { echo "no ${OS}_${ARCH} asset for $VER" >&2; exit 1; }
fi

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
curl -fsSL "$URL" | tar -xz -C "$TMP"

# Keep the outgoing binary so rollback is a copy, not another download — the
# network is exactly what you cannot count on while rolling back.
[ -f "$DIR/$BIN" ] && cp -f "$DIR/$BIN" "$DIR/$BIN.prev" || true
install "$TMP/$BIN" "$DIR/$BIN"
echo "installed $URL -> $DIR/$BIN"

restart() {
	if command -v systemctl >/dev/null 2>&1 && systemctl is-enabled --quiet "$BIN" 2>/dev/null; then
		sudo systemctl restart "$BIN"
		echo "restarted systemd unit $BIN"
	fi
}
restart

# No smoke target configured: deploy as before, but say so. Silence here would
# read as "verified" when nothing was.
if [ -z "$HEALTHZ" ]; then
	echo "warning: HEALTHZ unset — deployed without a smoke test, no rollback available" >&2
	exit 0
fi

echo "smoking $HEALTHZ (timeout ${SMOKE_TIMEOUT}s)"
i=0
while [ "$i" -lt "$SMOKE_TIMEOUT" ]; do
	if curl -fsS --max-time 2 "$HEALTHZ" 2>/dev/null | grep -q '"status":"ok"'; then
		echo "smoke passed"
		exit 0
	fi
	i=$((i + 1))
	sleep 1
done

echo "smoke FAILED after ${SMOKE_TIMEOUT}s — rolling back" >&2
if [ -f "$DIR/$BIN.prev" ]; then
	install "$DIR/$BIN.prev" "$DIR/$BIN"
	restart
	echo "rolled back to the previous binary" >&2
else
	echo "no previous binary to roll back to; the unit is running the bad build" >&2
fi
# Non-zero so the caller — a human, a workflow, or an agent — treats this as
# the incident it is. tools/watch will file it on its next pass regardless.
exit 1

#!/bin/bash
# Build a macOS installer package (.pkg) for Tart Oven.
#
#   ./packaging/build-pkg.sh
#
# Optional signing (Developer ID Installer cert, for distribution outside Jamf):
#   SIGN_IDENTITY="Developer ID Installer: Your Name (TEAMID)" ./packaging/build-pkg.sh
#
# The resulting TartOven-<version>.pkg installs:
#   /Library/Application Support/Tart Oven/tart-oven   (the binary)
#   /Library/LaunchAgents/com.tartoven.agent.plist     (auto-start agent)
# and its postinstall loads the agent and opens http://127.0.0.1:8080.
set -euo pipefail

cd "$(dirname "$0")/.."   # repo root
REPO="$(pwd)"

PKG_ID="com.tartoven.pkg"
LABEL="com.tartoven.agent"
APPDIR="Library/Application Support/Tart Oven"

VERSION=$(sed -n 's/^const version = "\(.*\)"/\1/p' main.go)
[ -n "$VERSION" ] || { echo "could not read version from main.go"; exit 1; }
OUT="TartOven-${VERSION}.pkg"

echo "==> Building tart-oven ${VERSION} (arm64)…"
BUILD="$(mktemp -d)"
GOOS=darwin GOARCH=arm64 go build -o "$BUILD/tart-oven" .

echo "==> Assembling payload…"
ROOT="$(mktemp -d)/root"
install -d "$ROOT/$APPDIR"
install -m 755 "$BUILD/tart-oven" "$ROOT/$APPDIR/tart-oven"
install -d "$ROOT/Library/LaunchAgents"
install -m 644 packaging/com.tartoven.agent.plist "$ROOT/Library/LaunchAgents/$LABEL.plist"

# Strip extended attributes so the payload doesn't carry ._AppleDouble clutter
# (the repo lives on synced storage that adds xattrs).
xattr -rc "$ROOT" 2>/dev/null || true

chmod +x packaging/scripts/postinstall

echo "==> Running pkgbuild…"
SIGN_ARGS=()
if [ -n "${SIGN_IDENTITY:-}" ]; then
    SIGN_ARGS=(--sign "$SIGN_IDENTITY")
    echo "    signing with: $SIGN_IDENTITY"
fi

pkgbuild \
    --root "$ROOT" \
    --identifier "$PKG_ID" \
    --version "$VERSION" \
    --scripts "$REPO/packaging/scripts" \
    --install-location "/" \
    ${SIGN_ARGS[@]+"${SIGN_ARGS[@]}"} \
    "$OUT"

echo "==> Done: $REPO/$OUT"
echo "    Install:   sudo installer -pkg \"$OUT\" -target /"
echo "    Or double-click it (sign/notarize first for Gatekeeper outside Jamf)."

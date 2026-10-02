#!/bin/bash
# Build a macOS installer package (.pkg) for Tart Oven.
#
#   ./packaging/build-pkg.sh
#
# Signing is auto-detected from Developer ID certs in Keychain, or forced with
# SIGN_PKG=true / APP_SIGN_IDENTITY+PKG_SIGN_IDENTITY. Whenever signing ends up
# enabled — by any of those paths — notarization is asked about (or forced
# non-interactively with NOTARIZE=true). Interactively, you'll be asked:
#   - Whether to sign (skipped if certs are auto-detected or SIGN_PKG is set)
#   - Whether to notarize
#
# Notarization credentials (Apple ID + team ID + app-specific password) are
# stored once in the keychain under the "TartOvenNotarize" notarytool profile
# (see `xcrun notarytool store-credentials`) — same pattern as Armada's
# release.sh. First run prompts for them (or reads TEAM_ID, APPLE_EMAIL,
# APP_PASSWORD); every run after that reuses the stored profile silently.
# Nothing is ever printed or written to disk in plain text.
#
# The resulting TartOven-<version>.pkg installs:
#   /Library/Application Support/Tart Oven/tart-oven   (the binary)
#   /Library/LaunchAgents/com.tartoven.agent.plist     (auto-start agent)
# and its postinstall loads the agent and opens http://127.0.0.1:9000.
set -euo pipefail

# Do not copy resource forks or extended attributes while assembling archives.
export COPYFILE_DISABLE=1

cd "$(dirname "$0")/.."   # repo root
REPO="$(pwd)"

PKG_ID="com.tartoven.pkg"
LABEL="com.tartoven.agent"
APPDIR="Library/Application Support/Tart Oven"

VERSION=$(sed -n 's/^const version = "\(.*\)"/\1/p' main.go)
[ -n "$VERSION" ] || { echo "could not read version from main.go"; exit 1; }

OUT_DIR="${OUT_DIR:-$HOME/Downloads}"
mkdir -p "$OUT_DIR"
OUT="${OUT_DIR}/TartOven-${VERSION}.pkg"

# Auto-detect Developer ID identities from Keychain
DETECTED_APP_ID=$(security find-identity -v -p codesigning 2>/dev/null | grep "Developer ID Application:" | head -n 1 | sed -E 's/.*"Developer ID Application: ([^"]+)".*/Developer ID Application: \1/' || true)
DETECTED_PKG_ID=$(security find-identity -v 2>/dev/null | grep "Developer ID Installer:" | head -n 1 | sed -E 's/.*"Developer ID Installer: ([^"]+)".*/Developer ID Installer: \1/' || true)

APP_SIGN_IDENTITY="${APP_SIGN_IDENTITY:-$DETECTED_APP_ID}"
PKG_SIGN_IDENTITY="${PKG_SIGN_IDENTITY:-$DETECTED_PKG_ID}"
DO_SIGN=false
DO_NOTARIZE=false

# Check environment or prompt
if [ -n "${SIGN_PKG:-}" ] && [ "$SIGN_PKG" = "true" ]; then
    if [ -n "$APP_SIGN_IDENTITY" ] && [ -n "$PKG_SIGN_IDENTITY" ]; then
        DO_SIGN=true
    fi
elif [ -n "${APP_SIGN_IDENTITY}" ] && [ -n "${PKG_SIGN_IDENTITY}" ]; then
    DO_SIGN=true
elif [ -t 0 ]; then
    echo ""
    read -p "Do you want to sign the PKG with Developer ID? [Y/n] " SIGN_ANSWER
    SIGN_ANSWER=${SIGN_ANSWER:-y}
    if [[ "$SIGN_ANSWER" =~ ^[Yy]$ ]]; then
        if [ -n "$DETECTED_APP_ID" ] && [ -n "$DETECTED_PKG_ID" ]; then
            echo "  Using detected identities:"
            echo "    App: $DETECTED_APP_ID"
            echo "    Pkg: $DETECTED_PKG_ID"
            APP_SIGN_IDENTITY="$DETECTED_APP_ID"
            PKG_SIGN_IDENTITY="$DETECTED_PKG_ID"
            DO_SIGN=true
        else
            echo "Enter your Apple Developer signing details:"
            read -p "  Full name (as in certificate): " DEV_NAME
            read -p "  Team ID: " TEAM_ID
            APP_SIGN_IDENTITY="Developer ID Application: $DEV_NAME ($TEAM_ID)"
            PKG_SIGN_IDENTITY="Developer ID Installer: $DEV_NAME ($TEAM_ID)"
            DO_SIGN=true
        fi
    fi
fi

if [ "$DO_SIGN" = true ]; then
    echo "==> Signing enabled:"
    echo "    App binary: $APP_SIGN_IDENTITY"
    echo "    Installer:  $PKG_SIGN_IDENTITY"
fi

# Notarization credentials, resolved independently of *how* signing was
# enabled (env var, an auto-detected Keychain identity, or the prompt above).
# Auto-detection alone used to short-circuit past the old notarization prompt
# entirely on any Mac that already had Developer ID certs installed, since
# that prompt only lived inside the interactive-signing branch above.
NOTARY_PROFILE="TartOvenNotarize"

if [ "$DO_SIGN" = true ]; then
    if [ -n "${NOTARIZE:-}" ] && [ "$NOTARIZE" = "true" ]; then
        DO_NOTARIZE=true
    elif [ -t 0 ]; then
        echo ""
        read -p "Do you also want to submit for Apple Notarization? [y/N] " NOTARIZE_ANSWER
        NOTARIZE_ANSWER=${NOTARIZE_ANSWER:-n}
        if [[ "$NOTARIZE_ANSWER" =~ ^[Yy]$ ]]; then
            DO_NOTARIZE=true
        fi
    fi

    if [ "$DO_NOTARIZE" = true ]; then
        # One-time setup: store Apple ID + team ID + app-specific password in
        # the keychain under $NOTARY_PROFILE. Every later run finds the
        # profile already there and skips straight to notarizing with it.
        if ! xcrun notarytool history --keychain-profile "$NOTARY_PROFILE" >/dev/null 2>&1; then
            TEAM_ID="${TEAM_ID:-}"
            APPLE_EMAIL="${APPLE_EMAIL:-}"
            APP_PASSWORD="${APP_PASSWORD:-}"
            if [ -z "$TEAM_ID" ] || [ -z "$APPLE_EMAIL" ] || [ -z "$APP_PASSWORD" ]; then
                if [ ! -t 0 ]; then
                    echo "No stored notarization credentials (keychain profile \"$NOTARY_PROFILE\") and TEAM_ID/APPLE_EMAIL/APP_PASSWORD aren't all set for non-interactive setup." >&2
                    exit 1
                fi
                echo "No stored notarization credentials found (keychain profile \"$NOTARY_PROFILE\")."
                echo "This is a one-time setup — after this, future runs won't ask again."
                echo ""
                [ -z "$TEAM_ID" ] && read -p "  Team ID: " TEAM_ID
                [ -z "$APPLE_EMAIL" ] && read -p "  Apple ID email: " APPLE_EMAIL
                if [ -z "$APP_PASSWORD" ]; then
                    echo "  App-specific password (generate one at https://appleid.apple.com/account/manage):"
                    read -s -p "  Password: " APP_PASSWORD
                    echo ""
                fi
            fi
            xcrun notarytool store-credentials "$NOTARY_PROFILE" \
                --apple-id "$APPLE_EMAIL" \
                --team-id "$TEAM_ID" \
                --password "$APP_PASSWORD"
            unset APP_PASSWORD
            echo "Credentials stored in the keychain under profile \"$NOTARY_PROFILE\"."
            echo ""
        fi
        echo "==> Notarization enabled (keychain profile: $NOTARY_PROFILE)"
    fi
fi

echo ""
echo "==> Building tart-oven ${VERSION} (arm64)…"
BUILD="$(mktemp -d)"
GOOS=darwin GOARCH=arm64 CGO_ENABLED=1 go build -trimpath -buildvcs=false -o "$BUILD/tart-oven" .

# Sign the binary with hardened runtime if signing is enabled
if [ "$DO_SIGN" = true ]; then
    echo "==> Signing binary with: $APP_SIGN_IDENTITY"
    codesign --force --options runtime --timestamp --sign "$APP_SIGN_IDENTITY" "$BUILD/tart-oven"
    echo "    Verifying signature…"
    codesign --verify --verbose "$BUILD/tart-oven"
fi

echo "==> Assembling payload…"
ROOT="$(mktemp -d)/root"
install -d "$ROOT/$APPDIR"
install -m 755 "$BUILD/tart-oven" "$ROOT/$APPDIR/tart-oven"
install -d "$ROOT/Library/LaunchAgents"
install -m 644 packaging/com.tartoven.agent.plist "$ROOT/Library/LaunchAgents/$LABEL.plist"

# Strip extended attributes so the payload doesn't carry ._AppleDouble clutter
# (the repo lives on synced storage that adds xattrs).
xattr -rc "$ROOT" 2>/dev/null || true

# Some managed/synced environments attach protected attributes that xattr cannot
# remove. Re-stage through an xattr-free UDF view so pkgbuild cannot serialize
# those attributes as ._ AppleDouble payload files.
PKG_ROOT="$ROOT"
PAYLOAD_MOUNT=""
cleanup_payload_mount() {
    if [ -n "$PAYLOAD_MOUNT" ]; then
        hdiutil detach -quiet "$PAYLOAD_MOUNT" 2>/dev/null || true
    fi
}
trap cleanup_payload_mount EXIT
if [ -n "$(xattr -lr "$ROOT" 2>/dev/null)" ]; then
    echo "==> Re-staging payload without extended attributes…"
    PAYLOAD_IMAGE="$BUILD/payload.iso"
    PAYLOAD_MOUNT="$BUILD/payload-root"
    mkdir -p "$PAYLOAD_MOUNT"
    hdiutil makehybrid -quiet -o "$PAYLOAD_IMAGE" "$ROOT" -udf -udf-volume-name TARTOVEN_PAYLOAD
    hdiutil attach -quiet -nobrowse -mountpoint "$PAYLOAD_MOUNT" "$PAYLOAD_IMAGE"
    PKG_ROOT="$PAYLOAD_MOUNT"
fi

chmod +x packaging/scripts/postinstall

echo "==> Running pkgbuild…"
PKG_SIGN_ARGS=()
if [ "$DO_SIGN" = true ]; then
    PKG_SIGN_ARGS=(--sign "$PKG_SIGN_IDENTITY")
    echo "    signing PKG with: $PKG_SIGN_IDENTITY"
fi

pkgbuild \
    --root "$PKG_ROOT" \
    --identifier "$PKG_ID" \
    --version "$VERSION" \
    --scripts "$REPO/packaging/scripts" \
    --install-location "/" \
    ${PKG_SIGN_ARGS[@]+"${PKG_SIGN_ARGS[@]}"} \
    "$OUT"

PAYLOAD_FILES=$(pkgutil --payload-files "$OUT")
if printf '%s\n' "$PAYLOAD_FILES" | grep -Eq '(^|/)\._'; then
    echo "error: package payload contains AppleDouble entries:" >&2
    printf '%s\n' "$PAYLOAD_FILES" | grep -E '(^|/)\._' >&2
    exit 1
fi
for REQUIRED_FILE in \
    "./Library/Application Support/Tart Oven/tart-oven" \
    "./Library/LaunchAgents/$LABEL.plist"; do
    if ! printf '%s\n' "$PAYLOAD_FILES" | grep -Fqx "$REQUIRED_FILE"; then
        echo "error: package payload is missing $REQUIRED_FILE" >&2
        exit 1
    fi
done

if [ -n "$PAYLOAD_MOUNT" ]; then
    hdiutil detach -quiet "$PAYLOAD_MOUNT"
    PAYLOAD_MOUNT=""
fi

echo "==> PKG built: $OUT"

# Notarize and staple if notarization is enabled
if [ "$DO_NOTARIZE" = true ]; then
    echo ""
    echo "==> Submitting for notarization…"
    xcrun notarytool submit "$OUT" --keychain-profile "$NOTARY_PROFILE" --wait

    echo ""
    echo "==> Stapling notarization ticket…"
    xcrun stapler staple "$OUT"
fi

echo ""
echo "==> Done: $OUT"
echo "    Install: sudo installer -pkg \"$OUT\" -target /"

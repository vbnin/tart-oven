#!/bin/bash
# Build a macOS installer package for the Tart guest agent.
#
#   ./packaging/build-agent-pkg.sh
#
# Downloads the official openai/tart-guest-agent release, verifies its checksum,
# re-signs it with a Developer ID Application identity (when available), wraps it
# in a PKG installer (signed with Developer ID Installer when available), and
# optionally notarizes the result.
#
# Signing and notarization follow the same pattern as build-pkg.sh: auto-detect
# identities from Keychain, honour SIGN_PKG/APP_SIGN_IDENTITY/PKG_SIGN_IDENTITY,
# and use the TartOvenNotarize keychain profile for notarization. Always
# rebuilds and overwrites packaging/build/; build-pkg.sh reuses that output.
#
# The resulting tart-guest-agent-<version>.pkg installs:
#   /usr/local/bin/tart-guest-agent
#   /Library/LaunchDaemons/org.cirruslabs.tart-guest-daemon.plist
#   /Library/LaunchAgents/org.cirruslabs.tart-guest-agent.plist
# and loads both launchd jobs.
set -euo pipefail

export COPYFILE_DISABLE=1

cd "$(dirname "$0")/.."   # repo root
REPO="$(pwd)"

AGENT_VERSION="${AGENT_VERSION:-0.15.0}"
PKG_ID="com.tartoven.guest-agent"
BUILD_DIR="$REPO/packaging/build"
mkdir -p "$BUILD_DIR"

OUT_PKG="$BUILD_DIR/tart-guest-agent-${AGENT_VERSION}.pkg"
OUT_LICENSE="$BUILD_DIR/tart-guest-agent-LICENSE.txt"

# Auto-detect Developer ID identities
# Picks the newest valid certificate of each kind and returns its SHA-1 hash, not
# its name — a renewed cert has the same name as the one it replaces, which makes
# codesign/pkgbuild fail with "ambiguous". See lib-signing.sh.
source "$REPO/packaging/lib-signing.sh"
pick_developer_id_identity Application; DETECTED_APP_ID="$PICKED_IDENTITY"
pick_developer_id_identity Installer;   DETECTED_PKG_ID="$PICKED_IDENTITY"

APP_SIGN_IDENTITY="${APP_SIGN_IDENTITY:-$DETECTED_APP_ID}"
PKG_SIGN_IDENTITY="${PKG_SIGN_IDENTITY:-$DETECTED_PKG_ID}"
DO_SIGN=false
DO_NOTARIZE=false

if [ -n "${SIGN_PKG:-}" ]; then
    if [ "$SIGN_PKG" = "true" ] && [ -n "$APP_SIGN_IDENTITY" ] && [ -n "$PKG_SIGN_IDENTITY" ]; then
        DO_SIGN=true
    fi
elif [ -n "$APP_SIGN_IDENTITY" ] && [ -n "$PKG_SIGN_IDENTITY" ]; then
    DO_SIGN=true
elif [ -t 0 ]; then
    echo ""
    read -p "Do you want to sign the PKG with Developer ID? [Y/n] " SIGN_ANSWER
    SIGN_ANSWER=${SIGN_ANSWER:-y}
    if [[ "$SIGN_ANSWER" =~ ^[Yy]$ ]]; then
        if [ -n "$DETECTED_APP_ID" ] && [ -n "$DETECTED_PKG_ID" ]; then
            echo "  Using detected identities:"
            echo "    App: $(describe_signing_identity "$DETECTED_APP_ID")"
            echo "    Pkg: $(describe_signing_identity "$DETECTED_PKG_ID")"
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
    echo "    App binary: $(describe_signing_identity "$APP_SIGN_IDENTITY")"
    echo "    Installer:  $(describe_signing_identity "$PKG_SIGN_IDENTITY")"
fi

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
echo "==> Downloading tart-guest-agent ${AGENT_VERSION}…"
TEMP_DIR="$(mktemp -d)"
PAYLOAD_MOUNT=""
cleanup() {
    if [ -n "$PAYLOAD_MOUNT" ]; then
        hdiutil detach -quiet "$PAYLOAD_MOUNT" 2>/dev/null || true
    fi
    rm -rf "$TEMP_DIR"
}
trap cleanup EXIT

TARBALL_URL="https://github.com/openai/tart-guest-agent/releases/download/v${AGENT_VERSION}/tart-guest-agent-darwin-all.tar.gz"
CHECKSUM_URL="https://github.com/openai/tart-guest-agent/releases/download/v${AGENT_VERSION}/tart-guest-agent_${AGENT_VERSION}_checksums.txt"

curl -fsSL -o "$TEMP_DIR/tart-guest-agent.tar.gz" "$TARBALL_URL"
curl -fsSL -o "$TEMP_DIR/checksums.txt" "$CHECKSUM_URL"

echo "==> Verifying checksum…"
cd "$TEMP_DIR"
EXPECTED_SHA=$(grep "tart-guest-agent-darwin-all.tar.gz" checksums.txt | awk '{print $1}')
ACTUAL_SHA=$(shasum -a 256 tart-guest-agent.tar.gz | awk '{print $1}')
if [ "$EXPECTED_SHA" != "$ACTUAL_SHA" ]; then
    echo "error: checksum mismatch" >&2
    echo "  expected: $EXPECTED_SHA" >&2
    echo "    actual: $ACTUAL_SHA" >&2
    exit 1
fi
echo "    OK: $ACTUAL_SHA"

echo "==> Extracting…"
tar xzf tart-guest-agent.tar.gz
if [ ! -f tart-guest-agent ]; then
    echo "error: tarball did not contain tart-guest-agent binary" >&2
    exit 1
fi
if [ ! -f LICENSE ]; then
    echo "warning: tarball did not contain LICENSE; continuing without it" >&2
fi

# Copy LICENSE to build output
if [ -f LICENSE ]; then
    cp LICENSE "$OUT_LICENSE"
fi

# Re-sign the binary if signing is enabled
if [ "$DO_SIGN" = true ]; then
    echo "==> Re-signing binary with: $(describe_signing_identity "$APP_SIGN_IDENTITY")"
    codesign --force --options runtime --timestamp --sign "$APP_SIGN_IDENTITY" tart-guest-agent
    echo "    Verifying signature…"
    codesign --verify --verbose tart-guest-agent
fi

echo "==> Assembling payload…"
PAYLOAD_ROOT="$TEMP_DIR/payload-root"
mkdir -p "$PAYLOAD_ROOT/usr/local/bin"
install -m 755 tart-guest-agent "$PAYLOAD_ROOT/usr/local/bin/tart-guest-agent"

SCRIPTS_DIR="$TEMP_DIR/scripts"
mkdir -p "$SCRIPTS_DIR"

# Generate postinstall script
cat > "$SCRIPTS_DIR/postinstall" << 'POSTINSTALL_EOF'
#!/bin/sh
set -e

DAEMON_LABEL="org.cirruslabs.tart-guest-daemon"
AGENT_LABEL="org.cirruslabs.tart-guest-agent"

DAEMON_PLIST="/Library/LaunchDaemons/${DAEMON_LABEL}.plist"
AGENT_PLIST="/Library/LaunchAgents/${AGENT_LABEL}.plist"

# Write daemon plist
cat > "$DAEMON_PLIST" << 'DAEMON_PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
    <dict>
        <key>Label</key>
        <string>org.cirruslabs.tart-guest-daemon</string>
        <key>ProgramArguments</key>
        <array>
            <string>/usr/local/bin/tart-guest-agent</string>
            <string>--run-daemon</string>
        </array>
        <key>EnvironmentVariables</key>
        <dict>
            <key>PATH</key>
            <string>/bin:/usr/bin:/usr/sbin:/usr/local/bin:/opt/homebrew/bin</string>
        </dict>
        <key>WorkingDirectory</key>
        <string>/var/empty</string>
        <key>RunAtLoad</key>
        <true/>
        <key>KeepAlive</key>
        <true/>
        <key>StandardOutPath</key>
        <string>/tmp/tart-guest-daemon.log</string>
        <key>StandardErrorPath</key>
        <string>/tmp/tart-guest-daemon.log</string>
    </dict>
</plist>
DAEMON_PLIST

# Write agent plist
cat > "$AGENT_PLIST" << 'AGENT_PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
    <dict>
        <key>Label</key>
        <string>org.cirruslabs.tart-guest-agent</string>
        <key>ProgramArguments</key>
        <array>
            <string>/usr/local/bin/tart-guest-agent</string>
            <string>--run-agent</string>
        </array>
        <key>EnvironmentVariables</key>
        <dict>
            <key>PATH</key>
            <string>/bin:/usr/bin:/usr/sbin:/usr/local/bin:/opt/homebrew/bin</string>
            <key>TERM</key>
            <string>xterm-256color</string>
        </dict>
        <key>RunAtLoad</key>
        <true/>
        <key>KeepAlive</key>
        <true/>
        <key>StandardOutPath</key>
        <string>/tmp/tart-guest-agent.log</string>
        <key>StandardErrorPath</key>
        <string>/tmp/tart-guest-agent.log</string>
    </dict>
</plist>
AGENT_PLIST

chown root:wheel "$DAEMON_PLIST" "$AGENT_PLIST"
chmod 0644 "$DAEMON_PLIST" "$AGENT_PLIST"

# (Re)load daemon
launchctl bootout system/"$DAEMON_LABEL" 2>/dev/null || true
launchctl bootstrap system "$DAEMON_PLIST" || true

# (Re)load agent for currently logged-in user if any
CONSOLE_USER=$(stat -f %Su /dev/console 2>/dev/null || echo "")
if [ -n "$CONSOLE_USER" ] && [ "$CONSOLE_USER" != "root" ] && [ "$CONSOLE_USER" != "loginwindow" ] && [ "$CONSOLE_USER" != "_mbsetupuser" ]; then
    CONSOLE_UID=$(id -u "$CONSOLE_USER")
    launchctl bootout gui/"$CONSOLE_UID"/"$AGENT_LABEL" 2>/dev/null || true
    launchctl bootstrap gui/"$CONSOLE_UID" "$AGENT_PLIST" || true
fi

exit 0
POSTINSTALL_EOF

chmod 755 "$SCRIPTS_DIR/postinstall"

# Strip xattrs
xattr -rc "$PAYLOAD_ROOT" 2>/dev/null || true
xattr -rc "$SCRIPTS_DIR" 2>/dev/null || true

# Re-stage through UDF if protected xattrs remain
PKG_ROOT="$PAYLOAD_ROOT"
if [ -n "$(xattr -lr "$PAYLOAD_ROOT" 2>/dev/null)" ]; then
    echo "==> Re-staging payload without extended attributes…"
    PAYLOAD_IMAGE="$TEMP_DIR/payload.iso"
    PAYLOAD_MOUNT="$TEMP_DIR/payload-mount"
    mkdir -p "$PAYLOAD_MOUNT"
    hdiutil makehybrid -quiet -o "$PAYLOAD_IMAGE" "$PAYLOAD_ROOT" -udf -udf-volume-name AGENT_PAYLOAD
    hdiutil attach -quiet -nobrowse -mountpoint "$PAYLOAD_MOUNT" "$PAYLOAD_IMAGE"
    PKG_ROOT="$PAYLOAD_MOUNT"
fi

echo "==> Running pkgbuild…"
PKG_SIGN_ARGS=()
if [ "$DO_SIGN" = true ]; then
    PKG_SIGN_ARGS=(--sign "$PKG_SIGN_IDENTITY")
    echo "    signing PKG with: $(describe_signing_identity "$PKG_SIGN_IDENTITY")"
else
    echo ""
    echo "WARNING: building an UNSIGNED package. 'sudo installer' (and Install Agent)"
    echo "         work fine; double-clicking it in a guest's Finder may show an"
    echo "         unidentified-developer warning if the file is quarantined."
    echo ""
fi

pkgbuild \
    --root "$PKG_ROOT" \
    --identifier "$PKG_ID" \
    --version "$AGENT_VERSION" \
    --scripts "$SCRIPTS_DIR" \
    --install-location "/" \
    ${PKG_SIGN_ARGS[@]+"${PKG_SIGN_ARGS[@]}"} \
    "$OUT_PKG"

PAYLOAD_FILES=$(pkgutil --payload-files "$OUT_PKG")
if printf '%s\n' "$PAYLOAD_FILES" | grep -Eq '(^|/)\._'; then
    echo "error: package payload contains AppleDouble entries:" >&2
    printf '%s\n' "$PAYLOAD_FILES" | grep -E '(^|/)\._' >&2
    exit 1
fi
if ! printf '%s\n' "$PAYLOAD_FILES" | grep -Fqx "./usr/local/bin/tart-guest-agent"; then
    echo "error: package payload is missing ./usr/local/bin/tart-guest-agent" >&2
    exit 1
fi

if [ -n "$PAYLOAD_MOUNT" ]; then
    hdiutil detach -quiet "$PAYLOAD_MOUNT"
    PAYLOAD_MOUNT=""
fi

echo "==> PKG built: $OUT_PKG"

# Notarize and staple if enabled
if [ "$DO_NOTARIZE" = true ]; then
    echo ""
    echo "==> Submitting for notarization…"
    xcrun notarytool submit "$OUT_PKG" --keychain-profile "$NOTARY_PROFILE" --wait

    echo ""
    echo "==> Stapling notarization ticket…"
    xcrun stapler staple "$OUT_PKG"
fi

echo ""
echo "==> Done: $OUT_PKG"
if [ -f "$OUT_LICENSE" ]; then
    echo "    License: $OUT_LICENSE"
fi
echo "    Install inside a guest: sudo installer -pkg \"<path>\" -target /"

#!/bin/bash
# Build "Tart Oven.app", a double-clickable launcher for people who stopped the
# server and don't use Terminal.
#
#   ./packaging/build-launcher-app.sh <output dir> [version]
#
# Double-clicking the app runs `tart-oven open`: start the server if it isn't
# running, wait until it answers, then open the dashboard in the default
# browser. The address (host, port, http or https) is read from the user's
# ~/.tart-oven/state.json by tart-oven itself. The app has no Dock icon, window
# or Terminal; it only shows an alert when the server cannot be started.
#
# Environment:
#   LAUNCHER_BIN         tart-oven binary the app runs (default: the installed one)
#   APP_SIGN_IDENTITY    codesign identity; unset leaves the app unsigned
#   REQUIRE_RUNTIME=true sign with the hardened runtime and a timestamp (release builds)
set -euo pipefail

cd "$(dirname "$0")/.."   # repo root
OUT_DIR="${1:?usage: build-launcher-app.sh <output dir> [version]}"
VERSION="${2:-$(sed -n 's/^const Version = "\(.*\)"/\1/p' tartoven.go)}"
[ -n "$VERSION" ] || { echo "could not read version from tartoven.go" >&2; exit 1; }
BIN="${LAUNCHER_BIN:-/Library/Application Support/Tart Oven/tart-oven}"
ICON_SRC="assets/icons/tart-oven-icon-512px.png"
[ -f "$ICON_SRC" ] || { echo "missing $ICON_SRC" >&2; exit 1; }

APP="$OUT_DIR/Tart Oven.app"
rm -rf "$APP"
install -d "$APP/Contents/MacOS" "$APP/Contents/Resources"

# Bundle versions are numeric; "2.1.0-dev2" becomes "2.1.0".
NUM_VERSION="${VERSION%%-*}"
cat > "$APP/Contents/Info.plist" <<PLIST_EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>CFBundleIdentifier</key>
    <string>com.tartoven.launcher</string>
    <key>CFBundleName</key>
    <string>Tart Oven</string>
    <key>CFBundleDisplayName</key>
    <string>Tart Oven</string>
    <key>CFBundleExecutable</key>
    <string>tart-oven-launcher</string>
    <key>CFBundleIconFile</key>
    <string>TartOven</string>
    <key>CFBundlePackageType</key>
    <string>APPL</string>
    <key>CFBundleShortVersionString</key>
    <string>${NUM_VERSION}</string>
    <key>CFBundleVersion</key>
    <string>${NUM_VERSION}</string>
    <key>LSMinimumSystemVersion</key>
    <string>13.0</string>
    <!-- No Dock icon or menu bar: the app only launches the server and the browser. -->
    <key>LSUIElement</key>
    <true/>
</dict>
</plist>
PLIST_EOF

# The launcher. Output of `tart-oven open` is kept so a failure can be shown;
# on success nothing at all appears. PATH is the plain system one when Finder
# starts an app, which is all osascript needs.
cat > "$APP/Contents/MacOS/tart-oven-launcher" <<LAUNCHER_EOF
#!/bin/sh
BIN='${BIN}'
if [ ! -x "\$BIN" ]; then
    osascript -e 'display alert "Tart Oven is not installed" message "Install Tart Oven from its installer package, then open this app again." as critical' >/dev/null 2>&1
    exit 1
fi
if out=\$("\$BIN" open 2>&1); then
    exit 0
fi
msg=\$(printf '%s\n' "\$out" | tail -n 4)
osascript \\
    -e 'on run argv' \\
    -e 'display alert "Tart Oven could not start" message (item 1 of argv) as critical' \\
    -e 'end run' "\$msg" >/dev/null 2>&1
exit 1
LAUNCHER_EOF
chmod 755 "$APP/Contents/MacOS/tart-oven-launcher"

# Icon: the 512px PNG at every size an .icns wants (up to 512 at 1x / 256 at 2x).
ICONSET="$(mktemp -d)/TartOven.iconset"
mkdir -p "$ICONSET"
for size in 16 32 64 128 256 512; do
    sips -z "$size" "$size" "$ICON_SRC" --out "$ICONSET/tmp_$size.png" >/dev/null
done
cp "$ICONSET/tmp_16.png"  "$ICONSET/icon_16x16.png"
cp "$ICONSET/tmp_32.png"  "$ICONSET/icon_16x16@2x.png"
cp "$ICONSET/tmp_32.png"  "$ICONSET/icon_32x32.png"
cp "$ICONSET/tmp_64.png"  "$ICONSET/icon_32x32@2x.png"
cp "$ICONSET/tmp_128.png" "$ICONSET/icon_128x128.png"
cp "$ICONSET/tmp_256.png" "$ICONSET/icon_128x128@2x.png"
cp "$ICONSET/tmp_256.png" "$ICONSET/icon_256x256.png"
cp "$ICONSET/tmp_512.png" "$ICONSET/icon_256x256@2x.png"
cp "$ICONSET/tmp_512.png" "$ICONSET/icon_512x512.png"
rm -f "$ICONSET"/tmp_*.png
iconutil -c icns "$ICONSET" -o "$APP/Contents/Resources/TartOven.icns"
rm -rf "$(dirname "$ICONSET")"

plutil -lint "$APP/Contents/Info.plist" >/dev/null

if [ -n "${APP_SIGN_IDENTITY:-}" ]; then
    SIGN_ARGS=(--force --sign "$APP_SIGN_IDENTITY")
    [ "${REQUIRE_RUNTIME:-false}" = true ] && SIGN_ARGS+=(--options runtime --timestamp)
    codesign "${SIGN_ARGS[@]}" "$APP"
fi

echo "$APP"

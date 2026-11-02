#!/bin/sh
# Postinstall of Tart Oven's build of the Tart guest agent package (copied into
# the PKG by build-agent-pkg.sh).
#
# Tart Oven runs the agent's exec/IP service (--run-rpc) from the root
# LaunchDaemon, so `tart exec` works from boot, before anyone logs in. The
# per-user LaunchAgent keeps only the clipboard helper (--run-vdagent). Upstream's
# own layout does it the other way round (the daemon only resizes the disk and
# exec needs a logged-in user), so the user agent of an earlier install is shut
# down first: both would try to serve exec on the same port.
#
# Installer passes: $1 package, $2 install location, $3 target volume ("/" when
# installing onto the running system). On any other volume the files are written
# but launchd is left alone.
set -e

DAEMON_LABEL="org.cirruslabs.tart-guest-daemon"
AGENT_LABEL="org.cirruslabs.tart-guest-agent"

ROOT="${3:-/}"
[ "$ROOT" = "/" ] && ROOT=""
DAEMON_PLIST="$ROOT/Library/LaunchDaemons/${DAEMON_LABEL}.plist"
AGENT_PLIST="$ROOT/Library/LaunchAgents/${AGENT_LABEL}.plist"

mkdir -p "$ROOT/Library/LaunchDaemons" "$ROOT/Library/LaunchAgents"

cat > "$DAEMON_PLIST" << 'DAEMON_EOF'
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
            <string>--run-rpc</string>
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
DAEMON_EOF

cat > "$AGENT_PLIST" << 'AGENT_EOF'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
    <dict>
        <key>Label</key>
        <string>org.cirruslabs.tart-guest-agent</string>
        <key>ProgramArguments</key>
        <array>
            <string>/usr/local/bin/tart-guest-agent</string>
            <string>--run-vdagent</string>
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
AGENT_EOF

if [ "$(id -u)" = 0 ]; then
    chown root:wheel "$DAEMON_PLIST" "$AGENT_PLIST"
fi
chmod 0644 "$DAEMON_PLIST" "$AGENT_PLIST"

# Installing onto another volume: nothing to load.
[ -z "$ROOT" ] || exit 0

CONSOLE_USER=$(stat -f %Su /dev/console 2>/dev/null || echo "")
CONSOLE_UID=""
if [ -n "$CONSOLE_USER" ] && [ "$CONSOLE_USER" != "root" ] && [ "$CONSOLE_USER" != "loginwindow" ] && [ "$CONSOLE_USER" != "_mbsetupuser" ]; then
    CONSOLE_UID=$(id -u "$CONSOLE_USER" 2>/dev/null || echo "")
fi

# 1. Stop the previous per-user agent (it served exec when run as --run-agent).
if [ -n "$CONSOLE_UID" ]; then
    launchctl bootout gui/"$CONSOLE_UID"/"$AGENT_LABEL" 2>/dev/null || true
fi
pkill -f 'tart-guest-agent --run-agent' 2>/dev/null || true

# 2. (Re)load the daemon, which now serves exec.
launchctl bootout system/"$DAEMON_LABEL" 2>/dev/null || true
launchctl bootstrap system "$DAEMON_PLIST" || true

# 3. (Re)load the clipboard helper for the logged-in user, if any.
if [ -n "$CONSOLE_UID" ]; then
    launchctl bootstrap gui/"$CONSOLE_UID" "$AGENT_PLIST" || true
fi

exit 0

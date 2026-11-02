#!/bin/bash
# Primes a golden-image VM so Auto Enroll VM / "Auto enroll at boot" works on
# every VM cloned from it afterward. Run this ONCE per golden image, from your
# Mac (not inside the VM) — it needs Screen Sharing open to that VM so you can
# see and approve the prompts it triggers.
#
# What it does NOT do, on purpose:
#   - Does not touch TCC.db directly. Modern macOS's own TCC prompt is the only
#     supported way to grant Automation, and `profiles install` is blocked on
#     the CLI for every profile type (PPPC included) — there's no way around
#     the guest needing your click here.
#   - Does not set the autologin password. It's checked, but if it's missing
#     you set it once via System Settings — storing a guest password in a
#     plist from a script is not something worth doing for a one-time toggle.
#
# Usage: ./prep-golden-image.sh <vm-name|vm-ip> [guest-user]
#   A VM name runs the commands through the Tart guest agent (`tart exec`, no SSH
#   needed); an IPv4 address runs them over SSH. Use whichever the VM has.

set -euo pipefail
TARGET="${1:?usage: prep-golden-image.sh <vm-name|vm-ip> [guest-user]}"
USER_NAME="${2:-admin}"

if [[ "$TARGET" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  TRANSPORT="SSH"
  guest() {
    ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null "${USER_NAME}@${TARGET}" "$1"
  }
else
  TRANSPORT="the Tart guest agent"
  # Same hand-off Tart Oven uses: the agent's exec service runs as root, so run
  # the command as the guest user (inside their GUI session when they are logged
  # in at the console, which System Events scripting needs).
  RUN_AS='u=$1; c=$2
if [ "$(/usr/bin/id -u)" = 0 ] && [ -n "$u" ] && [ "$u" != root ] && uid=$(/usr/bin/id -u "$u" 2>/dev/null); then
  if [ "$(/usr/bin/stat -f %Su /dev/console 2>/dev/null)" = "$u" ]; then
    exec /bin/launchctl asuser "$uid" /usr/bin/sudo -n -u "$u" -H /bin/sh -c "$c"
  fi
  exec /usr/bin/sudo -n -u "$u" -H /bin/sh -c "$c"
fi
exec /bin/sh -c "$c"'
  guest() {
    tart exec "$TARGET" /bin/sh -c "$RUN_AS" tart-oven "$USER_NAME" "$1"
  }
fi
echo "Running commands in the guest through $TRANSPORT."

echo "== Checking autologin =="
AUTOLOGIN_USER=$(guest "defaults read /Library/Preferences/com.apple.loginwindow autoLoginUser 2>/dev/null" || true)
if [ "$AUTOLOGIN_USER" = "$USER_NAME" ]; then
  echo "autologin already set for $USER_NAME — good."
else
  cat <<EOF
autologin is NOT set for $USER_NAME (current value: "${AUTOLOGIN_USER:-none}").
Enable it now on the VM's screen: System Settings > Users & Groups > Login
Options > Automatic login > $USER_NAME, enter the password once.
Press Enter here once that's done (or if it's already fine and this check is wrong).
EOF
  read -r _
fi

echo
echo "== Triggering the Automation prompt (control System Events) =="
echo "Watch the VM's screen now — a prompt asking to allow control of \"System Events\" should appear."
echo "Click Allow when you see it. This will hang until you do (or ~20s if it's already granted)."
guest "osascript -e 'tell application \"System Events\" to get name of every window of process \"System Settings\"'" > /dev/null 2>&1 || true

cat <<EOF

== Now the manual part TCC doesn't let a script do ==
On the VM's screen, open System Settings > Privacy & Security > Accessibility
and make sure the entry for the process running these commands is listed and
toggled ON ("sshd-keygen-wrapper" over SSH, the Tart guest agent otherwise). It won't prompt
for this one automatically — that's Apple's behavior, not a bug here.
Press Enter once it's toggled on.
EOF
read -r _

echo
echo "== Verifying both grants headlessly (no prompt should appear, no hang) =="
if guest "osascript -e 'tell application \"System Events\" to get name of every window of process \"System Settings\"'" > /dev/null 2>&1; then
  echo "OK — Automation + Accessibility both confirmed working over $TRANSPORT."
  echo "This golden image is ready. Every VM cloned from it inherits these grants."
else
  echo "Still failing. Re-check the Accessibility toggle, or re-run this script — the Automation"
  echo "prompt only shows once per grant decision, so a stale 'Don't Allow' needs resetting on"
  echo "the VM itself before it will prompt again: tccutil reset AppleEvents"
fi

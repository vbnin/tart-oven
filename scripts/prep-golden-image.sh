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
# Usage: ./prep-golden-image.sh <vm-ip> [ssh-user]

set -euo pipefail
IP="${1:?usage: prep-golden-image.sh <vm-ip> [ssh-user]}"
USER_NAME="${2:-admin}"
SSH="ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null ${USER_NAME}@${IP}"

echo "== Checking autologin =="
AUTOLOGIN_USER=$($SSH "defaults read /Library/Preferences/com.apple.loginwindow autoLoginUser 2>/dev/null" || true)
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
$SSH "osascript -e 'tell application \"System Events\" to get name of every window of process \"System Settings\"'" > /dev/null 2>&1 || true

cat <<EOF

== Now the manual part TCC doesn't let a script do ==
On the VM's screen, open System Settings > Privacy & Security > Accessibility
and make sure "sshd-keygen-wrapper" is listed and toggled ON. It won't prompt
for this one automatically — that's Apple's behavior, not a bug here.
Press Enter once it's toggled on.
EOF
read -r _

echo
echo "== Verifying both grants headlessly (no prompt should appear, no hang) =="
if $SSH "osascript -e 'tell application \"System Events\" to get name of every window of process \"System Settings\"'" > /dev/null 2>&1; then
  echo "OK — Automation + Accessibility both confirmed working for sshd-keygen-wrapper."
  echo "This golden image is ready. Every VM cloned from it inherits these grants."
else
  echo "Still failing. Re-check the Accessibility toggle, or re-run this script — the Automation"
  echo "prompt only shows once per grant decision, so a stale 'Don't Allow' needs resetting on"
  echo "the VM itself before it will prompt again: $SSH \"tccutil reset AppleEvents\""
fi

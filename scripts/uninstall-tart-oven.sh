#!/bin/bash
#
# uninstall-tart-oven.sh
#
# Resets or removes Tart Oven. A menu asks how far to go:
#   1) Reset settings          - back to a fresh install; the app and your VMs stay
#   2) Delete Tart Oven        - the app and its settings; Tart and your VMs stay
#   3) Delete Tart Oven + Tart - also removes Tart itself; your VMs stay on disk
#   4) Delete everything       - also deletes every VM and the image cache
#
# How to run it
#   Terminal:  sudo ./uninstall-tart-oven.sh        (shows the menu)
#              sudo ./uninstall-tart-oven.sh 2      (picks an option, no questions asked)
#   It acts on the Mac's logged-in user (or the user who ran sudo).
#
# Not removed: Tart installed with Homebrew (run "brew uninstall tart"), and a
# guest agent installed inside a VM (it goes with the VM).
#
# This script is not part of the Tart Oven installer package.

if [ "$(id -u)" -ne 0 ]; then
    echo "This script must run as root. Try: sudo $0"
    exit 1
fi

# ---- Who and where ----------------------------------------------------------
USER_NAME="${SUDO_USER:-$(stat -f%Su /dev/console)}"
case "$USER_NAME" in
    ""|root|loginwindow) echo "No logged-in user found. Run this with sudo from the user's Terminal."; exit 1 ;;
esac
USER_ID=$(id -u "$USER_NAME")
USER_HOME=$(dscl . -read "/Users/$USER_NAME" NFSHomeDirectory | awk '{print $2}')

LABEL="com.tartoven.agent"
PLIST="/Library/LaunchAgents/$LABEL.plist"
APP_DIR="/Library/Application Support/Tart Oven"
SETTINGS_DIR="$USER_HOME/.tart-oven"
TART_APP="/Applications/tart.app"

# Where the VMs and the shared folder are, as saved in Tart Oven's settings.
setting() { plutil -extract "config.$1" raw -o - "$SETTINGS_DIR/state.json" 2>/dev/null; }
STORAGE=$(setting vmStoragePath);  STORAGE="${STORAGE:-/Users/Shared/Tart}"
SHARED=$(setting sharedDir);       SHARED="${SHARED:-$STORAGE/Resources}"

# ---- Helpers ----------------------------------------------------------------
remove() {
    if [ -e "$1" ] || [ -L "$1" ]; then
        rm -rf "$1" && echo "Removed: $1"
    fi
}

stop_tart_oven() {
    launchctl bootout "gui/$USER_ID" "$PLIST" 2>/dev/null
    pkill -x tart-oven 2>/dev/null
}

stop_running_vms() {
    pkill -f "tart.app/Contents/MacOS/tart run" 2>/dev/null && { echo "Stopped running VMs."; sleep 3; }
}

# Refuses folders that are obviously not a Tart storage folder.
check_storage_path() {
    case "$STORAGE" in
        /*) ;;
        *) echo "Storage path '$STORAGE' is not an absolute path. Not deleting."; exit 1 ;;
    esac
    case "$STORAGE" in
        /|/Users|/Users/Shared|/Volumes|/Applications|/Library|/System|/private|/usr|/opt|"$USER_HOME")
            echo "Storage path '$STORAGE' looks too broad. Not deleting."; exit 1 ;;
    esac
}

# ---- Menu -------------------------------------------------------------------
choice="${1:-}"
if [ -z "$choice" ]; then
    cat <<EOF
Tart Oven uninstall (user: $USER_NAME)

  1) Reset settings           Keeps the app, Tart and your VMs. Clears the saved
                              settings, tokens, HTTPS certificate and sign-in.
  2) Delete Tart Oven         Keeps Tart and your VMs.
  3) Delete Tart Oven + Tart  Keeps your VMs on disk ($STORAGE).
  4) Delete everything        Also deletes ALL VMs and downloaded images.
  q) Quit

EOF
    read -r -p "Choose [1-4, q]: " choice
fi
case "$choice" in 1|2|3|4) ;; *) echo "Nothing done."; exit 0 ;; esac

# Ask before doing anything (skipped when the option came from the command line).
if [ -z "${1:-}" ]; then
    if [ "$choice" = 4 ]; then
        check_storage_path
        echo "This permanently deletes every VM in $STORAGE and in $USER_HOME/.tart."
        read -r -p "Type DELETE to continue: " answer
        [ "$answer" = "DELETE" ] || { echo "Nothing done."; exit 0; }
    else
        read -r -p "Continue? [y/N] " answer
        case "$answer" in y|Y) ;; *) echo "Nothing done."; exit 0 ;; esac
    fi
fi
[ "$choice" = 4 ] && check_storage_path

# ---- Do it ------------------------------------------------------------------
stop_tart_oven

if [ "$choice" = 1 ]; then
    remove "$SETTINGS_DIR"
    remove /Users/Shared/tart-oven.out.log
    remove /Users/Shared/tart-oven.err.log
    launchctl bootstrap "gui/$USER_ID" "$PLIST" 2>/dev/null && echo "Tart Oven restarted with fresh settings."
    exit 0
fi

# 2, 3, 4: remove Tart Oven.
remove "$PLIST"
remove "$APP_DIR"
remove "/Applications/Tart Oven.app"
[ "$(readlink /usr/local/bin/tart-oven 2>/dev/null)" = "$APP_DIR/tart-oven" ] && remove /usr/local/bin/tart-oven
remove "$SETTINGS_DIR"
remove /Users/Shared/tart-oven.out.log
remove /Users/Shared/tart-oven.err.log
remove "$SHARED/tart-guest-agent"
pkgutil --forget com.tartoven.pkg >/dev/null 2>&1 && echo "Forgot installer receipt: com.tartoven.pkg"

# 3, 4: remove Tart.
if [ "$choice" -ge 3 ]; then
    stop_running_vms
    remove "$TART_APP"
    # Tart's own folder holds VMs only if TART_HOME was never changed; keep it unless it is empty.
    if [ "$choice" = 4 ]; then
        remove "$USER_HOME/.tart"
    else
        remove "$USER_HOME/.tart/tmp"
        rmdir "$USER_HOME/.tart" 2>/dev/null
    fi
fi

# 4: remove every VM.
[ "$choice" = 4 ] && remove "$STORAGE"

echo "Done."
exit 0

package main

import (
	"context"
	cryptorand "crypto/rand"
	"fmt"
	"net"
	"strings"
	"time"
)

// copyMDMProfileToVM generates a fresh (unexpired) enrollment profile and copies
// it to name's Desktop, mirroring handleMDMProfile's target-selection and
// transfer logic so both the manual "Copy MDM profile" panel and the
// auto-enroll flow push the same kind of profile the same way. Kept separate
// from handleMDMProfile (rather than factored out of it) to avoid touching the
// heavily-tested HTTP handler for this first version.
func (m *Manager) copyMDMProfileToVM(ctx context.Context, name, profileID, sshUserOverride, sshPasswordOverride string) (path, payloadUUID string, err error) {
	m.mu.Lock()
	cfg := m.cfg
	vm := m.vms[name]
	var vmCopy VM
	if vm != nil {
		vmCopy = *vm
	}
	m.mu.Unlock()
	if vm == nil {
		return "", "", fmt.Errorf("VM %q not found", name)
	}

	var targetProfile JamfProfile
	profileFound := false
	if profileID != "" {
		for _, p := range cfg.JamfProfiles {
			if p.ID == profileID {
				targetProfile = p
				profileFound = true
				break
			}
		}
		if !profileFound {
			return "", "", fmt.Errorf("selected Jamf server profile no longer exists")
		}
	}
	if !profileFound && len(cfg.JamfProfiles) > 0 {
		targetProfile = cfg.JamfProfiles[0]
		profileFound = true
	}
	if !profileFound {
		targetProfile = JamfProfile{
			Name:           "Default Server",
			BaseURL:        cfg.JamfBaseURL,
			InvitationCode: cfg.JamfInvitationCode,
		}
	}

	username, password := effectiveSSHCredentials(cfg, &vmCopy)
	if strings.TrimSpace(sshUserOverride) != "" {
		username = strings.TrimSpace(sshUserOverride)
	}
	if sshPasswordOverride != "" {
		password = sshPasswordOverride
	}
	sshTimeout := cfg.SSHTimeoutSec
	if sshTimeout < 1 {
		sshTimeout = 30
	}
	baseURL, baseURLErr := normalizeJamfBaseURL(targetProfile.BaseURL)
	if baseURLErr != nil || baseURL == "" || strings.TrimSpace(targetProfile.InvitationCode) == "" ||
		strings.TrimSpace(username) == "" || strings.TrimSpace(password) == "" {
		return "", "", fmt.Errorf("MDM profile configuration is incomplete (Jamf server URL, invitation code, or SSH credentials missing)")
	}
	if vmCopy.State != "running" {
		return "", "", fmt.Errorf("VM is not running")
	}

	ip := strings.TrimSpace(vmCopy.IP)
	if ip == "" {
		if m.mdmResolveIP == nil {
			return "", "", fmt.Errorf("IP resolver unavailable")
		}
		ip, err = m.mdmResolveIP(ctx, name, cfg.VMStoragePath)
		if err != nil || strings.TrimSpace(ip) == "" {
			return "", "", fmt.Errorf("could not resolve VM IP: %w", err)
		}
		ip = strings.TrimSpace(ip)
	}

	profile, uuid, err := generateMDMProfile(mdmProfileInput{
		BaseURL:        baseURL,
		InvitationCode: targetProfile.InvitationCode,
	}, cryptorand.Reader)
	if err != nil {
		return "", "", fmt.Errorf("generate profile: %w", err)
	}
	if m.mdmCopier == nil {
		return "", "", fmt.Errorf("profile copy service unavailable")
	}

	target := mdmTransferTarget{
		Address:  net.JoinHostPort(ip, "22"),
		Username: username,
		Password: password,
		Timeout:  time.Duration(sshTimeout) * time.Second,
	}
	if err := m.mdmCopier.CopyAndVerify(ctx, target, profile, uuid); err != nil {
		return "", "", fmt.Errorf("copy profile: %w", err)
	}
	return mdmProfileDisplayPath, uuid, nil
}

// autoEnrollScript drives a guest end-to-end from wherever it currently is —
// possibly mid Setup-Assistant (macOS re-runs the full first-boot OOBE whenever
// --random-serial gives it a hardware identity it has never seen, even on a VM
// that already booted once before) — through to installing the
// ~/Desktop/mdm_enroll.mobileconfig profile already placed there by
// copyMDMProfileToVM, and on to a confirmed MDM enrollment.
//
// It is one long UI-scripting recipe rather than a handful of clean AppleEvents
// because most of the controls involved (System Settings' Device Management
// list, the Setup Assistant FileVault sheet) are SwiftUI views that don't expose
// accessible names and, in one case, don't even respond to synthetic AXPress —
// only to a real Tab-then-Return keyboard focus change. Every quirk below (the
// unlabeled-button ordinal matching, the "+" add-profile flow instead of the
// unresponsive list row, the SecurityAgent panel living outside System
// Settings' own window) was found and confirmed by hand against a real VM
// before being folded in here; see the design discussion for the dead ends
// (VNC-level input, direct TCC.db writes) that were deliberately not taken.
//
// Runs as a single SSH exec (mirrors sshExec's one-shot model): the script
// writes its own AppleScript helper files under /tmp/ae on the guest via
// heredocs, so no separate file transfer is needed. The guest's sudo/SecurityAgent
// password is read from stdin (fed the same way sshExecContext already feeds a
// sudo password) rather than embedded in the script text, so it never appears in
// argv or in the "ssh sudo exec" log line.
//
// Exits 0 and prints RESULT=ENROLLED on success; a non-zero exit with
// RESULT=NOT_ENROLLED or RESULT=OOBE_TIMEOUT reports the failure mode.
const autoEnrollScript = `#!/bin/bash
set -u
IFS= read -r VMPASS
LOG=/tmp/auto_enroll.log
echo "=== auto-enroll run $(date) ===" > "$LOG"

mkdir -p /tmp/ae
cat > /tmp/ae/dumpproc.scpt << 'EOF'
on run argv
  set procName to item 1 of argv
  tell application "System Events"
    tell process procName
      set out to ""
      set allEls to entire contents of window 1
      repeat with el in allEls
        set r to ""
        set n to ""
        try
          set r to (role of el) as string
        end try
        try
          set n to (name of el) as string
        end try
        if n is not "" and n is not "missing value" then
          set out to out & r & ": " & n & linefeed
        end if
      end repeat
      return out
    end tell
  end tell
end run
EOF

cat > /tmp/ae/clickcontains.scpt << 'EOF'
on run argv
  set targetSubstr to item 1 of argv
  set procName to item 2 of argv
  tell application "System Events"
    tell process procName
      set allEls to entire contents of window 1
      repeat with el in allEls
        set n to ""
        try
          set n to (name of el) as string
        end try
        if n contains targetSubstr then
          click el
          return "clicked: " & n
        end if
      end repeat
      return "not found"
    end tell
  end tell
end run
EOF

cat > /tmp/ae/clickexact.scpt << 'EOF'
on run argv
  set targetName to item 1 of argv
  set procName to item 2 of argv
  tell application "System Events"
    tell process procName
      set allEls to entire contents of window 1
      repeat with el in allEls
        set r to ""
        set n to ""
        try
          set r to (role of el) as string
        end try
        try
          set n to (name of el) as string
        end try
        if r is "AXButton" and n is targetName then
          click el
          return "clicked exact: " & n
        end if
      end repeat
      return "not found"
    end tell
  end tell
end run
EOF

# Clicks the Nth (1-indexed) unlabeled AXButton (name missing, description
# "button" — the generic SwiftUI buttons System Settings doesn't expose a
# title for). idx: positive = from start, negative = from end (-1 = last
# = the sheet's primary/rightmost action, e.g. Continue or Install).
cat > /tmp/ae/clickgeneric.scpt << 'EOF'
on run argv
  set idx to (item 1 of argv) as integer
  set procName to item 2 of argv
  tell application "System Events"
    tell process procName
      set allEls to entire contents of window 1
      set candidates to {}
      repeat with el in allEls
        set r to ""
        set d to ""
        set n to "x"
        try
          set r to (role of el) as string
        end try
        try
          set d to (description of el) as string
        end try
        try
          set n to (name of el) as string
        end try
        if r is "AXButton" and d is "button" and (n is "missing value" or n is "") then
          set end of candidates to el
        end if
      end repeat
      set c to count of candidates
      if c is 0 then return "no candidates"
      if idx > 0 then
        set target to idx
      else
        set target to c + idx + 1
      end if
      if target < 1 or target > c then return "index out of range (n=" & c & ")"
      click (item target of candidates)
      return "clicked generic #" & target & " of " & c
    end tell
  end tell
end run
EOF

# Clicks the "+" (add profile) button under General > Device Management.
# macOS 26/27's redesigned System Settings sidebar renders its ~30 nav rows
# as anonymous AXButtons (no name, description "button") — the same shape
# clickgeneric.scpt matches on — so an unscoped "click generic #1" now hits a
# sidebar row instead of "+". Addressing the sidebar/detail split by AXGroup
# class (e.g. "group 1 of splitter group 1 of ...") turned out unreliable —
# it intermittently reports empty groups depending on window/render state —
# so this filters by on-screen position instead: the sidebar's anonymous
# buttons sit within ~220px of the window's left edge; "+"/"-" sit well to
# the right of that, in the detail pane. Falls back to the old unscoped
# whole-window search if position data isn't available for some reason.
cat > /tmp/ae/clickaddprofile.scpt << 'EOF'
on run argv
  set procName to item 1 of argv
  tell application "System Events"
    tell process procName
      set candidates to {}
      -- The window's position/geometry can be momentarily unavailable right
      -- after System Settings launches or navigates (its AX tree is still
      -- settling), which would otherwise make the position filter below spuriously
      -- see zero candidates on a "cold" call. Give it a few short retries before
      -- falling back to the unscoped (less reliable, ordinal-only) search.
      repeat with attempt from 1 to 4
        try
          -- Splitting the property-get from the indexing matters here:
          -- "item 1 of (position of window 1)" throws "can't make ... into
          -- type specifier" (-1700) because System Events hands back a live
          -- property reference, not a plain list, until it's captured into a
          -- variable first. Combining them in one expression silently aborts
          -- this whole try block on every call, which is what was actually
          -- causing the permanent fallback to the unscoped ordinal search.
          set winPos to position of window 1
          set winX to item 1 of winPos
          set allEls to entire contents of window 1
          repeat with el in allEls
            set r to ""
            set d to ""
            set n to "x"
            try
              set r to (role of el) as string
            end try
            try
              set d to (description of el) as string
            end try
            try
              set n to (name of el) as string
            end try
            if r is "AXButton" and d is "button" and (n is "missing value" or n is "") then
              try
                set elPos to position of el
                set elX to item 1 of elPos
                if elX > (winX + 220) then
                  set end of candidates to el
                end if
              end try
            end if
          end repeat
        end try
        if (count of candidates) > 0 then exit repeat
        delay 1
      end repeat
      if (count of candidates) is 0 then
        set allEls to entire contents of window 1
        repeat with el in allEls
          set r to ""
          set d to ""
          set n to "x"
          try
            set r to (role of el) as string
          end try
          try
            set d to (description of el) as string
          end try
          try
            set n to (name of el) as string
          end try
          if r is "AXButton" and d is "button" and (n is "missing value" or n is "") then
            set end of candidates to el
          end if
        end repeat
      end if
      set c to count of candidates
      if c is 0 then return "no candidates"
      click (item 1 of candidates)
      return "clicked add-profile button (" & c & " candidates in scope)"
    end tell
  end tell
end run
EOF

# Last-resort fallback for a pane with exactly one enabled button, whatever
# it's called (e.g. a single "Continue" on an informational pane we don't
# otherwise recognize, such as "Software Update Complete").
cat > /tmp/ae/clickonlybutton.scpt << 'EOF'
on run argv
  set procName to item 1 of argv
  tell application "System Events"
    tell process procName
      set allEls to entire contents of window 1
      set candidates to {}
      repeat with el in allEls
        set r to ""
        try
          set r to (role of el) as string
        end try
        if r is "AXButton" then
          set isEnabled to true
          try
            set isEnabled to (enabled of el)
          end try
          if isEnabled then set end of candidates to el
        end if
      end repeat
      set c to count of candidates
      if c is 1 then
        click (item 1 of candidates)
        return "clicked only button"
      end if
      return "not exactly one enabled button (n=" & c & ")"
    end tell
  end tell
end run
EOF

dumpproc() { osascript /tmp/ae/dumpproc.scpt "$1" 2>&1; }
frontproc() { osascript -e 'tell application "System Events" to name of first process whose frontmost is true' 2>&1; }
log() { echo "[$(date +%T)] $*" | tee -a "$LOG"; }

# Tries a fixed list of common "move forward" / "decline this optional step"
# button labels in order, for a pane this script doesn't have specific
# handling for (new informational panes appear across macOS versions —
# "Software Update Complete" and the like — and most just need one of these
# clicked). Falls back to clickonlybutton.scpt if none of the labels match.
try_generic_continue() {
  for LABEL in "Continue" "Agree" "Not Now" "Later" "Skip" "Done" "Get Started" "OK" "Allow"; do
    RES=$(osascript /tmp/ae/clickexact.scpt "$LABEL" "Setup Assistant" 2>&1)
    case "$RES" in
      clicked*)
        log "generic fallback clicked \"$LABEL\""
        return 0
        ;;
    esac
  done
  RES=$(osascript /tmp/ae/clickonlybutton.scpt "Setup Assistant" 2>&1)
  case "$RES" in
    clicked*)
      log "generic fallback: $RES"
      return 0
      ;;
  esac
  log "generic fallback found nothing to click ($RES)"
  return 1
}

# ---------- Phase 1: OOBE / Setup Assistant, if the guest is mid-first-boot ----------
# macOS only re-runs this at all when --random-serial gives the guest a
# hardware identity it has never seen (and doesn't on macOS 27+, per Apple's
# own change there) — Phase 1 exits on its very first check when Setup
# Assistant isn't running, so it costs nothing on a guest that's already
# past OOBE.
log "Phase 1: checking for Setup Assistant OOBE"
OOBE_DONE=0
for i in $(seq 1 75); do
  FRONT=$(frontproc)
  if [ "$FRONT" != "Setup Assistant" ]; then
    log "Setup Assistant not frontmost (front=$FRONT) - OOBE done or not present"
    OOBE_DONE=1
    break
  fi

  WINCOUNT=$(osascript -e 'tell application "System Events" to count windows of process "Setup Assistant"' 2>&1)
  if [ "$WINCOUNT" = "2" ]; then
    log "Two windows detected - clicking Get Started inside window 2"
    osascript -e 'tell application "System Events"
      tell process "Setup Assistant"
        set w2 to item 2 of windows
        set allEls to entire contents of w2
        repeat with el in allEls
          set n to ""
          try
            set n to (name of el) as string
          end try
          if n contains "Get Started" then
            click el
            exit repeat
          end if
        end repeat
      end tell
    end tell' >>"$LOG" 2>&1
    sleep 2
    continue
  fi

  PANE=$(dumpproc "Setup Assistant")
  if echo "$PANE" | grep -q "How Do You Connect"; then
    osascript /tmp/ae/clickexact.scpt "Continue" "Setup Assistant" >>"$LOG" 2>&1
  elif echo "$PANE" | grep -q "Are you sure you want to skip signing in"; then
    osascript /tmp/ae/clickexact.scpt "Skip" "Setup Assistant" >>"$LOG" 2>&1
  elif echo "$PANE" | grep -qE "Sign In to Your Apple Account|Sign In with Your Apple ID"; then
    osascript /tmp/ae/clickcontains.scpt "Sign-In Options" "Setup Assistant" >>"$LOG" 2>&1
    sleep 1
    osascript /tmp/ae/clickcontains.scpt "Sign in Later in Settings" "Setup Assistant" >>"$LOG" 2>&1
  elif echo "$PANE" | grep -q "Are you sure you want to continue without FileVault"; then
    osascript /tmp/ae/clickexact.scpt "Continue" "Setup Assistant" >>"$LOG" 2>&1
  elif echo "$PANE" | grep -q "Your Mac is Ready for FileVault"; then
    osascript /tmp/ae/clickexact.scpt "Not Now" "Setup Assistant" >>"$LOG" 2>&1
  else
    log "Unrecognized pane: $(echo "$PANE" | head -1 | cut -c1-80) - trying generic fallback"
    try_generic_continue
  fi
  sleep 2
done

if [ "$OOBE_DONE" != "1" ]; then
  log "FATAL: OOBE did not complete within timeout, aborting"
  echo "RESULT=OOBE_TIMEOUT"
  exit 2
fi

# ---------- Phase 2: Device Management - install the profile via the "+" flow ----------
log "Phase 2: navigating to Device Management"
open "x-apple.systempreferences:com.apple.preferences.configurationprofiles"
sleep 2
osascript -e 'tell application "System Settings" to activate' >/dev/null 2>&1
sleep 1

log "Clicking + (add profile)"
osascript /tmp/ae/clickaddprofile.scpt "System Settings" | tee -a "$LOG"
sleep 2

log "Go to Folder -> mdm_enroll.mobileconfig"
osascript -e 'tell application "System Events" to keystroke "g" using {command down, shift down}' >>"$LOG" 2>&1
sleep 1
osascript -e "tell application \"System Events\" to keystroke \"$HOME/Desktop/mdm_enroll.mobileconfig\"" >>"$LOG" 2>&1
sleep 1
osascript -e 'tell application "System Events" to key code 36' >>"$LOG" 2>&1
sleep 2

log "Clicking Open"
osascript /tmp/ae/clickexact.scpt "Open" "System Settings" | tee -a "$LOG"
sleep 2

log "Clicking Continue on bootstrap sheet"
osascript /tmp/ae/clickgeneric.scpt -1 "System Settings" | tee -a "$LOG"
sleep 2

log "Clicking Install on unsigned-profile warning"
osascript /tmp/ae/clickexact.scpt "Install" "System Settings" | tee -a "$LOG"
sleep 3

log "Waiting for the real MDM profile sheet (bootstrap fetches it from the server)"
for i in $(seq 1 20); do
  PANE=$(dumpproc "System Settings")
  if echo "$PANE" | grep -q "Rights"; then
    log "Real MDM profile sheet detected"
    break
  fi
  sleep 1
done

log "Clicking Install on the real MDM profile"
osascript /tmp/ae/clickgeneric.scpt -1 "System Settings" | tee -a "$LOG"
sleep 2

log "Waiting for the SecurityAgent password prompt"
GOT_PROMPT=0
for i in $(seq 1 20); do
  PROCS=$(osascript -e 'tell application "System Events" to name of every process' 2>&1)
  if echo "$PROCS" | grep -q "SecurityAgent"; then
    GOT_PROMPT=1
    log "SecurityAgent prompt appeared"
    break
  fi
  sleep 1
done

if [ "$GOT_PROMPT" = "1" ]; then
  sleep 1
  ESCPASS=$(printf '%s' "$VMPASS" | sed 's/\\/\\\\/g; s/"/\\"/g')
  osascript -e "tell application \"System Events\" to keystroke \"$ESCPASS\"" >>"$LOG" 2>&1
  sleep 1
  # "Enroll" is a named button on some macOS versions, but on macOS 27 this
  # dialog's buttons (Cancel/Enroll) are both anonymous AXButtons like the
  # Device Management "+" button above — fall back to clicking the first one
  # when the exact-name lookup finds nothing. Confirmed empirically: unlike
  # the sheet buttons elsewhere in this script (where the last/rightmost
  # anonymous button is always the primary action), this dialog's first
  # anonymous button is Enroll and the last is Cancel — clicking "last" here
  # silently cancels enrollment instead of completing it.
  RES=$(osascript /tmp/ae/clickexact.scpt "Enroll" "SecurityAgent" 2>&1)
  echo "$RES" | tee -a "$LOG"
  case "$RES" in
    clicked*) ;;
    *) osascript /tmp/ae/clickgeneric.scpt 1 "SecurityAgent" | tee -a "$LOG" ;;
  esac
else
  log "No password prompt appeared - profile may have failed to install earlier"
fi

sleep 3

# ---------- Phase 3: poll for enrollment ----------
log "Phase 3: polling profiles status -type enrollment (up to 60s)"
for i in $(seq 1 20); do
  STATUS=$(profiles status -type enrollment 2>&1)
  if echo "$STATUS" | grep -q "MDM enrollment: Yes"; then
    log "ENROLLED: $STATUS"
    echo "RESULT=ENROLLED"
    exit 0
  fi
  sleep 3
done

log "Timed out. Final status: $(profiles status -type enrollment 2>&1)"
dumpproc "System Settings" | grep -i "fail\|expired" | tee -a "$LOG"
echo "RESULT=NOT_ENROLLED"
exit 1
`

// runAutoEnrollTask does the full auto-enroll flow for name as its own task
// (kind "enroll"), visible and cancellable from Activity like create/clone:
// make sure the VM is running and reachable, push a fresh (unexpired)
// enrollment profile to its Desktop, run autoEnrollScript over SSH, and
// refresh the MDM column regardless of outcome. Safe to call whether the VM
// is currently stopped or already running.
func (m *Manager) runAutoEnrollTask(name, trigger string) {
	t := m.newTask("enroll", name)
	m.broadcast()
	err := m.autoEnroll(t, name)
	m.finishTask(t, err)
	m.broadcast()
	if err != nil {
		m.logln("auto-enroll %s (%s): %v", name, trigger, err)
	} else {
		m.logln("auto-enroll %s (%s): enrolled", name, trigger)
	}
}

func (m *Manager) autoEnroll(t *Task, name string) error {
	m.mu.Lock()
	vm := m.vms[name]
	state := ""
	if vm != nil {
		state = vm.State
	}
	m.mu.Unlock()
	if vm == nil {
		return fmt.Errorf("VM %q not found", name)
	}

	if state != "running" {
		m.appendTaskOutput(t, "VM is stopped — starting it first\n")
		m.doRun(name, "auto-enroll")
		if !m.waitForState(t.ctx, name, "running", 3*time.Minute) {
			return fmt.Errorf("VM did not reach running state")
		}
	}

	m.appendTaskOutput(t, "Waiting for SSH to be reachable\n")
	if !m.waitForSSHReady(t.ctx, name, 2*time.Minute) {
		return fmt.Errorf("SSH never became reachable")
	}

	// Reuse whatever profile is already on the Desktop rather than always
	// overwriting it: copyMDMProfileToVM with no profileID falls back to
	// whichever Jamf Server Profile is first in Config.JamfProfiles, which
	// silently discards a profile the user deployed by hand for a specific
	// server (e.g. from the golden VM this was cloned from) in favor of
	// whatever happens to be first in the list.
	existsRes := m.sshExecContext(t.ctx, name, `test -f "$HOME/Desktop/mdm_enroll.mobileconfig" && echo yes || echo no`, "")
	if existsRes.Error == "" && existsRes.ExitCode == 0 && strings.TrimSpace(existsRes.Stdout) == "yes" {
		m.appendTaskOutput(t, "Reusing the enrollment profile already on the guest Desktop\n")
	} else {
		m.appendTaskOutput(t, "No enrollment profile found on the guest Desktop — pushing a fresh one\n")
		if _, _, err := m.copyMDMProfileToVM(t.ctx, name, "", "", ""); err != nil {
			return fmt.Errorf("could not push enrollment profile: %v", err)
		}
	}

	m.mu.Lock()
	cfg := m.cfg
	vmCopy := *vm
	m.mu.Unlock()
	_, password := effectiveSSHCredentials(cfg, &vmCopy)

	m.appendTaskOutput(t, "Running the enrollment script (OOBE skip if needed, then Install/Enroll)\n")
	res := m.sshExecContext(t.ctx, name, autoEnrollScript, password)
	m.appendTaskOutput(t, res.Stdout)
	if res.Stderr != "" {
		m.appendTaskOutput(t, res.Stderr)
	}

	m.refreshMDMStatus(name)
	m.broadcast()

	if strings.Contains(res.Stdout, "RESULT=ENROLLED") {
		// Belt-and-suspenders alongside refreshMDMStatus's own guest probe
		// above: set this directly from the script's own success signal so
		// doRun's "skip if vm.MDMEnrolled" guard (the thing that stops this
		// from re-running on every later boot) doesn't depend on a second,
		// separate probe succeeding right after the first one already did.
		m.mu.Lock()
		if vm := m.vms[name]; vm != nil {
			vm.MDMEnrolled = true
		}
		m.save()
		m.mu.Unlock()
		m.broadcast()
		return nil
	}
	if res.Error != "" {
		return fmt.Errorf("enrollment script failed: %s", res.Error)
	}
	return fmt.Errorf("enrollment did not complete — see the task log")
}

// waitForState polls until the VM reaches want or the deadline/ctx expires.
func (m *Manager) waitForState(ctx context.Context, name, want string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return false
		}
		m.mu.Lock()
		vm := m.vms[name]
		state := ""
		if vm != nil {
			state = vm.State
		}
		m.mu.Unlock()
		if state == want {
			return true
		}
		if state == "stopped" && want == "running" {
			// boot failed and doRun already reset it to stopped
			return false
		}
		time.Sleep(2 * time.Second)
	}
	return false
}

// waitForSSHReady polls a trivial guest command until it succeeds, so the
// enrollment script never races a VM that has an IP but hasn't finished booting.
func (m *Manager) waitForSSHReady(ctx context.Context, name string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return false
		}
		res := m.sshExecContext(ctx, name, "echo ok", "")
		if res.Error == "" && res.ExitCode == 0 {
			return true
		}
		time.Sleep(3 * time.Second)
	}
	return false
}

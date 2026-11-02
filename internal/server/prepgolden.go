package server

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// enrollReadiness is the compatibility snapshot shown next to the Target VM
// dropdown in "Enable Auto-Enrollment Capabilities on Base VM": each field is
// "unknown" | a positive state | a negative state, never inferred from
// silence — a probe that errors or times out reports unknown, not "disabled"/
// "not_granted", since those would misleadingly assert a VM is incompatible
// when the real answer is just "couldn't check right now".
type enrollReadiness struct {
	Autologin     string `json:"autologin"`     // enabled | disabled | unknown
	Accessibility string `json:"accessibility"` // granted | not_granted | unknown
	Profile       string `json:"profile"`       // present | absent | unknown
}

// checkEnrollReadiness runs three quick, read-only guest probes, each with
// its own bounded timeout.
//
// They go through execInGuest, so they use the guest agent when it answers and
// SSH otherwise, and they report on the same transport prepGoldenImageScript and
// autoEnrollScript run over: the Automation and Accessibility grants belong to
// the process that runs osascript (SSH's "sshd-keygen-wrapper", or the guest
// agent), so a probe over a different transport would answer for the wrong one.
//
// A guest without the agent makes `tart exec` wait out its own ~30s connection
// timeout before execInGuest falls back to SSH, and execViaAgent reports our
// shorter probe deadline as "the agent handled this call", which would leave
// every probe at "unknown". So the agent is probed once up front (without our
// deadline) when it has not been checked since boot; after that execInGuest
// knows which transport to use and answers within the probe timeout.
func (m *Manager) checkEnrollReadiness(parent context.Context, name string) enrollReadiness {
	r := enrollReadiness{Autologin: "unknown", Accessibility: "unknown", Profile: "unknown"}

	m.mu.Lock()
	vm := m.vms[name]
	running := vm != nil && vm.State == "running"
	m.mu.Unlock()
	if !running {
		return r
	}

	// Bounds the one scenario that genuinely needs a ceiling: Automation
	// never having been decided for this VM, which blocks the Accessibility
	// probe until a human answers the prompt it triggers — fine for
	// prepGoldenImageScript (someone is watching Screen Sharing when that
	// script runs), wrong for a dropdown-selection-triggered check that could
	// otherwise hang indefinitely if nobody's looking at the VM's screen
	// right now. The other two probes never block on anything, so this same
	// bound is just a generous ceiling for them, not a real constraint.
	const probeTimeout = 15 * time.Second

	m.guestAgentAvailable(name) // settles which transport execInGuest will use

	autologinCtx, cancel := context.WithTimeout(parent, probeTimeout)
	res := m.execInGuest(autologinCtx, name, "defaults read /Library/Preferences/com.apple.loginwindow autoLoginUser 2>/dev/null", "")
	cancel()
	if res.Error == "" {
		if res.ExitCode == 0 && strings.TrimSpace(res.Stdout) != "" {
			r.Autologin = "enabled"
		} else {
			r.Autologin = "disabled"
		}
	}

	accessibilityCtx, cancel := context.WithTimeout(parent, probeTimeout)
	res = m.execInGuest(accessibilityCtx, name, `osascript -e 'tell application "System Events" to get name of every window of process "System Settings"'`, "")
	cancel()
	if res.Error == "" {
		if res.ExitCode == 0 {
			r.Accessibility = "granted"
		} else {
			r.Accessibility = "not_granted"
		}
	}

	profileCtx, cancel := context.WithTimeout(parent, probeTimeout)
	res = m.execInGuest(profileCtx, name, `test -f "$HOME/Desktop/mdm_enroll.mobileconfig" && echo yes || echo no`, "")
	cancel()
	if res.Error == "" && res.ExitCode == 0 {
		switch strings.TrimSpace(res.Stdout) {
		case "yes":
			r.Profile = "present"
		case "no":
			r.Profile = "absent"
		}
	}

	return r
}

// prepGoldenImageScript primes a base VM's TCC state so autoEnrollScript can
// later drive it without any human interaction. It cannot fully script this —
// see the two dead ends noted in autoenroll.go's doc comment (TCC.db writes,
// PPPC via `profiles install`) — so it does the one thing that *is*
// scriptable (opening the right System Settings pane and triggering the
// Automation prompt on cue) and then polls, giving the operator, watching
// over Screen Sharing, time to click Allow and flip the Accessibility toggle.
//
// The polling loop doubles as the verification step: the same probe command
// blocks on the pending Automation decision the first time (so it naturally
// waits out however long the operator takes to click Allow), then fails fast
// with -1728 until Accessibility is also enabled, then succeeds — one loop
// handles both grants with no special-casing.
const prepGoldenImageScript = `#!/bin/bash
set -u
LOG=/tmp/prep_golden_image.log
echo "=== prep golden image $(date) ===" > "$LOG"
log() { echo "[$(date +%T)] $*" | tee -a "$LOG"; }

AUTOLOGIN=$(defaults read /Library/Preferences/com.apple.loginwindow autoLoginUser 2>/dev/null || echo "")
if [ -z "$AUTOLOGIN" ]; then
  log "WARNING: autoLoginUser is not set. Enable autologin via System Settings > Users & Groups > Login Options first — without it there's no GUI session for the enrollment script to drive."
else
  log "autologin is set for $AUTOLOGIN"
fi

open "x-apple.systempreferences:com.apple.preference.security?Privacy_Accessibility" 2>/dev/null

log "Waiting for the Automation prompt (click Allow on the VM's screen) and for the process running this script (sshd-keygen-wrapper over SSH, the Tart guest agent otherwise) to be enabled in Privacy & Security > Accessibility"
for i in $(seq 1 90); do
  if osascript -e 'tell application "System Events" to get name of every window of process "System Settings"' >/dev/null 2>&1; then
    log "Both grants confirmed — this VM is ready to be a golden image"
    (osascript -e 'display dialog "Auto-enrollment grants confirmed — this VM is ready to be a golden image." with title "Tart Oven" buttons {"OK"} default button 1' >/dev/null 2>&1 &)
    echo "RESULT=READY"
    exit 0
  fi
  sleep 5
done

log "Timed out after ~7.5 minutes waiting for both grants to be confirmed"
echo "RESULT=NOT_READY"
exit 1
`

// runPrepGoldenImageTask is the "Run Script" action in the "Enable
// Auto-Enrollment Capabilities on Base VM" panel: it needs the VM already
// running (unlike auto-enroll, it never boots one — priming only makes sense
// on a base VM you're actively watching over Screen Sharing), and it never
// touches the guest's password since nothing in this script types one. It runs
// over the guest agent or SSH, whichever the VM has (see execInGuest).
func (m *Manager) runPrepGoldenImageTask(name string) {
	t := m.newTask("prep-golden", name)
	m.broadcast()

	m.mu.Lock()
	vm := m.vms[name]
	state := ""
	if vm != nil {
		state = vm.State
	}
	m.mu.Unlock()

	var err error
	if vm == nil {
		err = fmt.Errorf("VM %q not found", name)
	} else if state != "running" {
		err = fmt.Errorf("VM %q is not running", name)
	} else {
		res := m.execInGuest(t.ctx, name, prepGoldenImageScript, "")
		m.appendTaskOutput(t, res.Stdout)
		if res.Stderr != "" {
			m.appendTaskOutput(t, res.Stderr)
		}
		switch {
		case strings.Contains(res.Stdout, "RESULT=READY"):
			err = nil
		case res.Error != "":
			err = fmt.Errorf("prep script failed: %s", res.Error)
		default:
			err = fmt.Errorf("timed out waiting for the Automation and Accessibility grants — see the task log")
		}
	}

	m.finishTask(t, err)
	m.broadcast()
	if err != nil {
		m.logln("prep golden image %s: %v", name, err)
	} else {
		m.logln("prep golden image %s: ready", name)
	}
}

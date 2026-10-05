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
// These call sshExecContext directly rather than the agent-preferring
// execInGuest, for two independent reasons:
//
//  1. Correctness: the whole point of these checks is whether the
//     "sshd-keygen-wrapper" TCC identity (SSH's exec helper — what
//     autoEnrollScript and prepGoldenImageScript actually run as) has its
//     grants. The guest agent, if installed, execs as a different process
//     entirely, so checking through it would answer a question about the
//     wrong identity.
//  2. Reliability: execInGuest tries the agent first, and on a VM without it
//     installed (the normal case here — these are un-agented base images),
//     tart exec's own connection attempt doesn't fail fast; it can run past
//     any reasonable per-probe timeout. When that happens execViaAgent's
//     `if ctxErr := ctx.Err(); ctxErr != nil { ...; return res, true }`
//     reports OUR timeout as "the agent handled this call, with an error"
//     instead of "fall back to SSH" — so every probe silently comes back
//     "unknown" no matter how long the timeout is, since the SSH fallback
//     never runs at all. Going straight to SSH sidesteps this entirely.
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

	autologinCtx, cancel := context.WithTimeout(parent, probeTimeout)
	res := m.sshExecContext(autologinCtx, name, "defaults read /Library/Preferences/com.apple.loginwindow autoLoginUser 2>/dev/null", "")
	cancel()
	if res.Error == "" {
		if res.ExitCode == 0 && strings.TrimSpace(res.Stdout) != "" {
			r.Autologin = "enabled"
		} else {
			r.Autologin = "disabled"
		}
	}

	accessibilityCtx, cancel := context.WithTimeout(parent, probeTimeout)
	res = m.sshExecContext(accessibilityCtx, name, `osascript -e 'tell application "System Events" to get name of every window of process "System Settings"'`, "")
	cancel()
	if res.Error == "" {
		if res.ExitCode == 0 {
			r.Accessibility = "granted"
		} else {
			r.Accessibility = "not_granted"
		}
	}

	profileCtx, cancel := context.WithTimeout(parent, probeTimeout)
	res = m.sshExecContext(profileCtx, name, `test -f "$HOME/Desktop/mdm_enroll.mobileconfig" && echo yes || echo no`, "")
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

log "Waiting for the Automation prompt (click Allow on the VM's screen) and for sshd-keygen-wrapper to be enabled in Privacy & Security > Accessibility"
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
// touches the guest's password since nothing in this script types one.
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
		res := m.sshExecContext(t.ctx, name, prepGoldenImageScript, "")
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

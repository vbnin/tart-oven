package server

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// guestPATHExport gives every guest command the same PATH whichever transport
// runs it: non-interactive SSH gets /usr/bin:/bin:/usr/sbin:/sbin (no
// /usr/local/bin, where jamf lives), and the guest agent's launchd PATH has no
// /sbin (where shutdown lives). macOS sudo has no secure_path, so it inherits it.
const guestPATHExport = `export PATH="/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin:$PATH"; `

var sudoPattern = regexp.MustCompile(`(^|[\s;&|(])sudo(\s)`)

// rewriteSudoForStdin makes every sudo in the command read its password from stdin
// instead of a TTY, so a non-interactive session can run it. Shared by the guest
// agent and SSH paths so both behave identically.
func rewriteSudoForStdin(command string) string {
	return sudoPattern.ReplaceAllString(command, `${1}sudo -S -p ''${2}`)
}

// exitCodeError carries a guest command's exit status.
type exitCodeError struct{ code int }

func (e *exitCodeError) Error() string { return fmt.Sprintf("exit status %d", e.code) }
func (e *exitCodeError) ExitCode() int { return e.code }

// agentExecUnavailable reports whether a `tart exec` failure means the guest agent
// is not usable, as opposed to the guest command itself failing. A guest command
// that exits non-zero is a successful agent call and must NOT fall back to SSH —
// otherwise a legitimately failing command would be silently re-run over another
// transport.
func agentExecUnavailable(err error, stderr string) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(stderr)
	for _, marker := range []string{
		"guest agent",
		"connection refused",
		"is only available on macos",
		"failed to connect",
		"vm is not running",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// guestRunAsScript is the shell program `tart exec` runs in the guest. With the
// agent package Tart Oven ships, the exec service is a root LaunchDaemon, so it
// is available before anyone logs in, but commands would run as root. The script
// hands the command to the VM's account instead ($1, the SSH user), so tools
// that refuse root (Homebrew) and per-user settings behave as they do over SSH.
//   - When that account is the one logged in at the console, the command enters
//     its GUI session (launchctl asuser), as it would under a per-user agent;
//     that is what System Events scripting needs.
//   - With nobody logged in, plain sudo is enough.
//   - With a per-user agent (not root), or an account that doesn't exist yet
//     (Setup Assistant not finished), the command just runs as it is.
//
// $2 is the command. Both travel as arguments, so no quoting is involved.
const guestRunAsScript = `u=$1; c=$2
if [ "$(/usr/bin/id -u)" = 0 ] && [ -n "$u" ] && [ "$u" != root ] && uid=$(/usr/bin/id -u "$u" 2>/dev/null); then
  if [ "$(/usr/bin/stat -f %Su /dev/console 2>/dev/null)" = "$u" ]; then
    exec /bin/launchctl asuser "$uid" /usr/bin/sudo -n -u "$u" -H /bin/sh -c "$c"
  fi
  exec /usr/bin/sudo -n -u "$u" -H /bin/sh -c "$c"
fi
exec /bin/sh -c "$c"`

// agentExecArgs builds the `tart exec` argument list for running command in the
// guest as user. tart only forwards stdin with -i.
func agentExecArgs(name, user, command string, stdin bool) []string {
	args := []string{"exec"}
	if stdin {
		args = append(args, "-i")
	}
	return append(args, name, "/bin/sh", "-c", guestRunAsScript, "tart-oven", user, command)
}

// execViaAgent runs command inside the guest through the Tart guest agent. The
// second return value reports whether the agent handled the call at all; when it is
// false the caller should fall back to SSH.
func (m *Manager) execViaAgent(ctx context.Context, name, command, sudoPassword string) (execResult, bool) {
	m.mu.Lock()
	home := m.cfg.VMStoragePath
	user, _ := effectiveSSHCredentials(m.cfg, m.vms[name])
	m.mu.Unlock()
	if strings.TrimSpace(user) == "" {
		user = "admin"
	}

	remote := command
	if sudoPassword != "" {
		remote = rewriteSudoForStdin(command)
	}

	cmd := m.tartCmdCtx(ctx, home, agentExecArgs(name, user, guestPATHExport+remote, sudoPassword != "")...)
	if sudoPassword != "" {
		cmd.Stdin = strings.NewReader(sudoPassword + "\n")
	}
	stdout, stderr := newCapBuffer(ctx), newCapBuffer(ctx)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()

	res := execResult{Stdout: stdout.String(), Stderr: stderr.String(), Truncated: stdout.truncated || stderr.truncated}
	if err == nil {
		return res, true
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		res.Error = ctxErr.Error()
		return res, true
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if agentExecUnavailable(err, res.Stderr) {
			return execResult{}, false
		}
		// The command ran and returned a status: a successful agent call.
		res.ExitCode = exitErr.ExitCode()
		return res, true
	}
	// tart itself could not be launched.
	return execResult{}, false
}

// execInGuest is the single way to run a command inside a guest. It prefers the
// Tart guest agent (no SSH, no key, no guest network) and falls back to SSH for
// images that do not ship the agent.
//
// When the guest has no agent, `tart exec` still spends ~30s on its own internal
// gRPC connection timeout before giving up — tolerable once, but it made every
// single guest command (even "whoami") pay that tax, since it was retried on
// every call. agentProbedSinceBoot caches "already tried and it's not there" for
// the life of the current boot, so only the first command after a (re)start pays
// it; installGuestAgent and a fresh boot both clear the cache to force a retry.
func (m *Manager) execInGuest(ctx context.Context, name, command, sudoPassword string) execResult {
	m.mu.Lock()
	knownUnavailable := m.agentProbedSinceBoot[name]
	m.mu.Unlock()

	if !knownUnavailable {
		if res, handled := m.execViaAgent(ctx, name, command, sudoPassword); handled {
			m.setAgentOK(name, true)
			return res
		}
		m.setAgentOK(name, false)
		m.mu.Lock()
		m.agentProbedSinceBoot[name] = true
		m.mu.Unlock()
	}

	m.mu.Lock()
	fallback := m.cfg.SSHFallbackEnabled
	m.mu.Unlock()
	if !fallback {
		return execResult{Error: "the Tart guest agent did not respond and the SSH fallback is turned off — " +
			"install the agent on this VM, or re-enable the SSH fallback in Configuration"}
	}
	return m.sshExecContext(ctx, name, command, sudoPassword)
}

// setAgentOK records whether the guest agent answered, for display only.
func (m *Manager) setAgentOK(name string, ok bool) {
	m.mu.Lock()
	if vm := m.vms[name]; vm != nil {
		vm.AgentOK = ok
		vm.AgentCheckedAt = time.Now()
	}
	m.mu.Unlock()
}

// probeGuestChannels checks the guest agent and SSH independently and records
// each result, so the SSH status reflects SSH itself rather than whichever
// transport answered. Each probe gets its own deadline: a guest without the
// agent makes `tart exec` wait out a ~30s gRPC timeout, and execViaAgent
// reports an expired context as "handled", so a shared or shorter deadline
// would both mark a missing agent as OK and starve the SSH probe.
//
// The agent is always probed (it can appear mid-boot once a user logs in, or
// after a manual install), and the result resets execInGuest's per-boot cache
// either way.
func (m *Manager) probeGuestChannels(name string) (agentOK, sshOK bool) {
	agentOK = m.probeAgent(name)
	sshOK = m.probeSSH(name)
	m.mu.Lock()
	m.save()
	m.mu.Unlock()
	m.broadcast()
	return agentOK, sshOK
}

// refreshVMInfo is "Refresh info": it re-checks a running VM's IP, its Agent
// and SSH status and its MDM enrollment, then runs the status command to
// refresh the collected info. It returns the status command's result.
func (m *Manager) refreshVMInfo(name string) execResult {
	m.refreshVMIP(name)
	m.probeGuestChannels(name)

	// The status command only fills the Info column; connectivity was probed above.
	m.mu.Lock()
	cmd := m.cfg.StatusCommand
	m.mu.Unlock()
	res := m.sshExec(name, cmd, "")
	_, info := sshOutcome(res)
	m.mu.Lock()
	if vm := m.vms[name]; vm != nil {
		vm.Info = info
		vm.InfoAt = time.Now()
	}
	m.mu.Unlock()
	m.refreshMDMStatus(name)
	m.broadcast()
	return res
}

// refreshVMIP re-resolves a running VM's IP, which can change after a DHCP
// renewal. A failed lookup keeps the IP already known.
func (m *Manager) refreshVMIP(name string) {
	m.mu.Lock()
	vm := m.vms[name]
	running := vm != nil && vm.State == "running"
	m.mu.Unlock()
	if !running {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ip, err := m.resolveVMIPRobust(ctx, m.storage(), name, 3)
	if err != nil || ip == "" {
		return
	}
	m.mu.Lock()
	if vm := m.vms[name]; vm != nil && vm.State == "running" && vm.IP != ip {
		m.logln("ip %s: now %s (was %s)", name, ip, vm.IP)
		vm.IP = ip
		m.save()
	}
	m.mu.Unlock()
}

// probeSSH records whether SSH itself answers. With the SSH fallback off it
// records "never checked" (the UI shows "off") and reports false.
func (m *Manager) probeSSH(name string) bool {
	m.mu.Lock()
	fallback := m.cfg.SSHFallbackEnabled
	timeout := time.Duration(m.cfg.SSHTimeoutSec)*time.Second + 10*time.Second
	m.mu.Unlock()

	ok := false
	var checkedAt time.Time
	if fallback {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		r := m.sshExecContext(ctx, name, "true", "")
		cancel()
		ok = r.Error == "" && r.ExitCode == 0
		checkedAt = time.Now()
	}
	m.mu.Lock()
	if vm := m.vms[name]; vm != nil {
		vm.SSHOK = ok
		vm.SSHCheckedAt = checkedAt
	}
	m.mu.Unlock()
	return ok
}

func (m *Manager) probeAgent(name string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	res, handled := m.execViaAgent(ctx, name, "true", "")
	cancel()
	ok := handled && res.Error == "" && res.ExitCode == 0
	m.setAgentOK(name, ok)
	m.mu.Lock()
	m.agentProbedSinceBoot[name] = !ok
	m.mu.Unlock()
	if ok {
		m.recordAgentMode(name)
	}
	return ok
}

// agentModeProbe tells which layout the guest's agent package has: Tart Oven's
// build serves exec (--run-rpc) from the root LaunchDaemon, so it works before
// anyone logs in; upstream's and the official images' serve it from a per-user
// LaunchAgent.
const agentModeProbe = `grep -q -e '--run-rpc' /Library/LaunchDaemons/org.cirruslabs.tart-guest-daemon.plist 2>/dev/null && echo boot || echo login`

// recordAgentMode stores whether the guest's agent starts at boot or at login. It
// needs a working agent, so it runs after a successful probe; a failed check
// leaves the earlier answer alone.
func (m *Manager) recordAgentMode(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	res, handled := m.execViaAgent(ctx, name, agentModeProbe, "")
	cancel()
	if !handled || res.Error != "" || res.ExitCode != 0 {
		return
	}
	mode := "login"
	if strings.TrimSpace(res.Stdout) == "boot" {
		mode = "boot"
	}
	m.mu.Lock()
	if vm := m.vms[name]; vm != nil {
		vm.AgentMode = mode
	}
	m.mu.Unlock()
}

// watchGuestChannelsAfterBoot keeps re-probing whichever channel failed the
// boot-time check. The boot probe runs as soon as the VM has an IP, which can be
// before the guest agent is up: it starts a little after boot, and with a
// per-user agent (not Tart Oven's package) only once someone logs in. Stops once
// both channels answer, the VM stops or reboots, or the window runs out.
func (m *Manager) watchGuestChannelsAfterBoot(name string, bootedAt time.Time, agentOK, sshOK bool) {
	const window = 5 * time.Minute
	const interval = 20 * time.Second
	deadline := bootedAt.Add(window)
	for !(agentOK && sshOK) && time.Now().Before(deadline) {
		time.Sleep(interval)
		m.mu.Lock()
		vm := m.vms[name]
		sameBoot := vm != nil && vm.State == "running" && vm.StartedAt.Equal(bootedAt)
		fallback := m.cfg.SSHFallbackEnabled
		m.mu.Unlock()
		if !sameBoot {
			return
		}
		if !fallback {
			sshOK = true // nothing to wait for; the UI shows "off"
		}
		changed := false
		if !agentOK {
			if agentOK = m.probeAgent(name); agentOK {
				m.logln("agent %s: responding (after boot)", name)
				changed = true
			}
		}
		if !sshOK {
			if sshOK = m.probeSSH(name); sshOK {
				m.logln("ssh %s: reachable (after boot)", name)
				changed = true
			}
		}
		if changed {
			m.mu.Lock()
			m.save()
			m.mu.Unlock()
			m.broadcast()
			go m.applyHostnameAndRefresh(name) // the boot-time attempt had no working channel
		}
	}
}

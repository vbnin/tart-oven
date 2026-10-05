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

// execViaAgent runs command inside the guest through the Tart guest agent. The
// second return value reports whether the agent handled the call at all; when it is
// false the caller should fall back to SSH.
func (m *Manager) execViaAgent(ctx context.Context, name, command, sudoPassword string) (execResult, bool) {
	m.mu.Lock()
	home := m.cfg.VMStoragePath
	m.mu.Unlock()

	remote := command
	if sudoPassword != "" {
		remote = rewriteSudoForStdin(command)
	}

	args := []string{"exec"}
	// tart exec only forwards stdin with -i; without it sudo -S gets nothing.
	if sudoPassword != "" {
		args = append(args, "-i")
	}
	args = append(args, name, "/bin/sh", "-c", guestPATHExport+remote)
	cmd := m.tartCmdCtx(ctx, home, args...)
	if sudoPassword != "" {
		cmd.Stdin = strings.NewReader(sudoPassword + "\n")
	}
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	res := execResult{Stdout: stdout.String(), Stderr: stderr.String()}
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
	return ok
}

// watchGuestChannelsAfterBoot keeps re-probing whichever channel failed the
// boot-time check. The boot probe runs as soon as the VM has an IP, which is
// often before a user is logged in — and the guest agent is a per-user
// LaunchAgent, so it only starts after login. Stops once both channels answer,
// the VM stops or reboots, or the window runs out.
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
			go m.applyHostnameIfPending(name) // the boot-time attempt had no working channel
		}
	}
}

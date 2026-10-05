package server

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// SSH exec ("Send command" / "Get info")
// ---------------------------------------------------------------------------

type execResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exitCode"`
	Error    string `json:"error,omitempty"`
}

// sshOutcome distills an execResult into (ok, displayable text) for the SSH
// bubble and the Info column.
func sshOutcome(res execResult) (bool, string) {
	ok := res.Error == "" && res.ExitCode == 0
	info := strings.TrimSpace(res.Stdout)
	if info == "" {
		if res.Error != "" {
			info = res.Error
		} else {
			info = strings.TrimSpace(res.Stderr)
		}
	}
	return ok, info
}

// sshExec runs command in the guest, preferring the Tart guest agent and falling
// back to SSH for images without it. The name is retained because it is the
// established entry point for Get info, Send command, and the boot probe; the
// transport is chosen per call by execInGuest. If sudoPassword is non-empty it is
// fed to `sudo -S` in the guest so commands needing sudo work without a TTY; the
// password itself is never logged.
//
// The deadline is deliberately generous and independent of SSHTimeoutSec, which is
// the SSH *connect* timeout: a small connect timeout must not kill a legitimately
// long command such as `softwareupdate`.
func (m *Manager) sshExec(name, command, sudoPassword string) execResult {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	return m.execInGuest(ctx, name, command, sudoPassword)
}

func (m *Manager) sshExecContext(ctx context.Context, name, command, sudoPassword string) execResult {
	m.mu.Lock()
	vm := m.vms[name]
	ip := ""
	if vm != nil {
		ip = vm.IP
	}
	user := m.cfg.SSHUser
	if vm != nil && vm.SSHUser != "" {
		user = vm.SSHUser
	}
	key := m.cfg.SSHKey
	timeout := m.cfg.SSHTimeoutSec
	home := m.cfg.VMStoragePath
	m.mu.Unlock()

	if ip == "" {
		// Resolve on demand if we don't have a cached IP, using the same
		// multi-tier resolver the boot path uses.
		resolveCtx, cancelResolve := context.WithTimeout(ctx, 20*time.Second)
		resolved, err := m.resolveVMIPRobust(resolveCtx, home, name, 10)
		cancelResolve()
		if err != nil {
			return execResult{Error: "could not resolve IP: " + err.Error()}
		}
		ip = strings.TrimSpace(resolved)
	}
	if ip == "" {
		return execResult{Error: "no IP for VM"}
	}
	if user == "" {
		user = "admin"
	}

	args := []string{
		"-o", "BatchMode=yes", // non-interactive: key auth only, never prompt
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null", // VMs change IPs; don't record/verify host keys
		"-o", "LogLevel=ERROR", // suppress the harmless "Permanently added ..." notice
		"-o", fmt.Sprintf("ConnectTimeout=%d", timeout),
		// Detect a dead connection (e.g. the guest powering off mid-`shutdown`)
		// and disconnect in ~15s instead of hanging forever — this keeps a stop
		// from wedging the scheduler goroutine. Live hosts keep responding to
		// keepalives, so legitimately long commands are unaffected.
		"-o", "ServerAliveInterval=5",
		"-o", "ServerAliveCountMax=3",
	}
	if key != "" {
		args = append(args, "-i", key)
	}

	// Non-interactive SSH sessions get a minimal PATH that misses /usr/local/bin
	// (where jamf and other third-party tools live), so augment it to match what
	// the guest agent sees. macOS sudo without secure_path preserves PATH, so
	// sudo commands work too.
	remoteCmd := guestPATHExport + command
	// With a sudo password, make every `sudo` in the command read its password
	// from stdin (-S) instead of a TTY, and feed it on stdin. We inject -S
	// directly rather than priming `sudo -v`, because sudo's credential cache
	// isn't shared between separate sudo calls in a non-interactive SSH session.
	// (The password is only ever on stdin — never in the command or the logs.)
	if sudoPassword != "" {
		remoteCmd = guestPATHExport + rewriteSudoForStdin(command)
		m.logln("ssh sudo exec on %s: %s", name, remoteCmd)
	}
	args = append(args, "--", fmt.Sprintf("%s@%s", user, ip), remoteCmd)

	cmd := exec.CommandContext(ctx, "ssh", args...)
	if sudoPassword != "" {
		cmd.Stdin = strings.NewReader(sudoPassword + "\n")
	}
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	res := execResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.ExitCode = ee.ExitCode()
		} else {
			res.Error = err.Error()
		}
	}
	return res
}

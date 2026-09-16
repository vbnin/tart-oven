package main

import (
	"context"
	"os"
	"strings"
	"testing"
)

// The literal word "sudo " must never appear in this script: the SSH transport's
// rewriteSudoForStdin rewrites any such occurrence to "sudo -S -p ”", which reads
// a second time from stdin (already consumed once for the SUDOPASS/askpass setup)
// and conflicts outright with -A ("the -A and -S options may not be used
// together"). Our own sudo call is therefore invoked through $SUDOCMD so the
// transport's blanket rewrite can't touch it — it must keep using -A (askpass),
// which is also what covers the Homebrew formula's own internal sudo call (see
// guestAgentInstallScript's doc comment).
func TestInstallScriptNeverUsesTheLiteralSudoWordSoTheTransportCannotRewriteIt(t *testing.T) {
	script := guestAgentInstallScript("admin")
	if strings.Contains(script, "sudo ") {
		t.Fatal(`script must not contain the literal "sudo " — it would get rewritten to "sudo -S -p ''" by the SSH transport and collide with -A`)
	}
	if rewritten := rewriteSudoForStdin(script); rewritten != script {
		t.Fatal("rewriteSudoForStdin must be a no-op on this script; it should have nothing to rewrite")
	}
	if !strings.Contains(script, `"$SUDOCMD" -A`) {
		t.Fatal(`script must invoke sudo via "$SUDOCMD" -A (askpass), not the literal word`)
	}
}

func TestInstallScriptRefusesToRunHomebrewAsRoot(t *testing.T) {
	script := guestAgentInstallScript("admin")
	brew := strings.Index(script, "brew install")
	sudoCmd := strings.Index(script, `"$SUDOCMD" -A`)
	if brew < 0 || sudoCmd < 0 {
		t.Fatal("script is missing its brew install or sudo stage")
	}
	if brew > sudoCmd {
		t.Fatal("brew install must run before the sudo block; Homebrew refuses to run as root")
	}
}

// The askpass helper is the only place the password touches disk; it must be
// created with owner-only permissions and removed on exit regardless of outcome.
func TestInstallScriptAskpassHelperIsPrivateAndCleanedUp(t *testing.T) {
	script := guestAgentInstallScript("admin")
	if !strings.Contains(script, `IFS= read -r SUDOPASS`) {
		t.Fatal("script must read the sudo password from stdin exactly once, into SUDOPASS")
	}
	if !strings.Contains(script, "chmod 700") {
		t.Fatal("askpass helper must be created with owner-only permissions")
	}
	if !strings.Contains(script, `trap 'rm -rf "$ASKPASS_DIR"' EXIT`) {
		t.Fatal("askpass helper directory must be removed on exit")
	}
}

func TestLaunchdPlistsMatchTheUpstreamLayout(t *testing.T) {
	daemon := launchdPlist(guestAgentDaemonLabel, "--run-daemon", "/var/empty", "/tmp/tart-guest-daemon.log", false)
	agent := launchdPlist(guestAgentAgentLabel, "--run-agent", "/Users/admin", "/tmp/tart-guest-agent.log", true)

	// Commands run through `tart exec` inherit the agent's environment, so PATH is
	// load-bearing, not cosmetic. Verified against the plists the official Cirrus
	// images actually ship.
	for _, want := range []string{
		"<string>" + guestAgentDaemonLabel + "</string>",
		"/opt/homebrew/bin/tart-guest-agent",
		"--run-daemon",
		"<key>PATH</key>",
		guestAgentPATH,
		"<string>/var/empty</string>",
		"<key>RunAtLoad</key>",
		"<key>KeepAlive</key>",
		"/tmp/tart-guest-daemon.log",
	} {
		if !strings.Contains(daemon, want) {
			t.Errorf("daemon plist missing %q", want)
		}
	}
	for _, want := range []string{
		"--run-agent",
		"<key>PATH</key>",
		guestAgentPATH,
		"<key>TERM</key>",
		"<string>/Users/admin</string>",
		"/tmp/tart-guest-agent.log",
	} {
		if !strings.Contains(agent, want) {
			t.Errorf("agent plist missing %q", want)
		}
	}
	// The per-session agent must not inherit the root daemon's working directory.
	if strings.Contains(agent, "/var/empty") {
		t.Error("agent plist must not use the root daemon working directory")
	}
	// TERM belongs only to the interactive per-session job.
	if strings.Contains(daemon, "TERM") {
		t.Error("daemon plist should not export TERM")
	}
}
func TestLoadDefaultsSSHFallbackOnForUpgradesButPreservesExplicitFalse(t *testing.T) {
	for _, tt := range []struct {
		name string
		body string
		want bool
	}{
		{"state file predating the setting", `{"config":{"listen":"127.0.0.1:9000"},"vms":{},"history":[]}`, true},
		{"explicitly turned off", `{"config":{"listen":"127.0.0.1:9000","sshFallbackEnabled":false},"vms":{},"history":[]}`, false},
		{"explicitly turned on", `{"config":{"listen":"127.0.0.1:9000","sshFallbackEnabled":true},"vms":{},"history":[]}`, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestManager(t)
			if err := os.WriteFile(m.statePath, []byte(tt.body), 0o600); err != nil {
				t.Fatal(err)
			}
			m.load()
			if m.cfg.SSHFallbackEnabled != tt.want {
				t.Fatalf("SSHFallbackEnabled = %v, want %v", m.cfg.SSHFallbackEnabled, tt.want)
			}
		})
	}
}

func TestExecInGuestReportsWhenFallbackIsOffAndAgentIsAbsent(t *testing.T) {
	m := newTestManager(t)
	m.cfg.SSHFallbackEnabled = false
	m.cfg.TartAppPath = "/nonexistent/tart" // forces the agent path to fail to launch
	m.vms["vm1"] = &VM{Name: "vm1", State: "running", IP: "10.0.0.9"}

	res := m.execInGuest(context.Background(), "vm1", "true", "")
	if !strings.Contains(res.Error, "SSH fallback is turned off") {
		t.Fatalf("error = %q, want it to name the disabled fallback", res.Error)
	}
	if m.vms["vm1"].AgentOK {
		t.Fatal("agentOk should be false when the agent did not answer")
	}
}

// A non-interactive SSH session gets PATH=/usr/bin:/bin:/usr/sbin:/sbin, which
// contains no Homebrew prefix. Verified against a real guest: without this export
// the script reports "Homebrew is not installed" on a guest that has it.
func TestInstallScriptRepairsPATHBeforeLookingForHomebrew(t *testing.T) {
	script := guestAgentInstallScript("admin")
	export := strings.Index(script, `export PATH="/opt/homebrew/bin:/usr/local/bin:$PATH"`)
	check := strings.Index(script, "command -v brew")
	if export < 0 {
		t.Fatal("script does not repair PATH for a non-interactive session")
	}
	if export > check {
		t.Fatal("PATH must be repaired before the Homebrew check, or the check is meaningless")
	}
}

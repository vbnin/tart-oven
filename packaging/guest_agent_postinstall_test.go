package packaging

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// plistArgs returns the ProgramArguments of a launchd plist, in order.
func plistArgs(t *testing.T, plist string) []string {
	t.Helper()
	var args []string
	for i := 0; ; i++ {
		out, err := exec.Command("plutil", "-extract", "ProgramArguments."+string(rune('0'+i)), "raw", "-o", "-", plist).Output()
		if err != nil {
			return args
		}
		args = append(args, strings.TrimSpace(string(out)))
	}
}

func TestGuestAgentPostinstallWritesBootTimeExecLayout(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("uses plutil")
	}
	stubs := t.TempDir()
	calls := filepath.Join(stubs, "calls")
	for _, tool := range []string{"launchctl", "pkill", "stat"} {
		writeScript(t, filepath.Join(stubs, tool), `echo "`+tool+` $*" >> "`+calls+`"`)
	}
	root := t.TempDir()

	// Installer's arguments: package, install location, target volume.
	cmd := exec.Command("./guest-agent-postinstall.sh", "pkg", "/", root)
	cmd.Env = []string{"PATH=" + stubs + ":/usr/bin:/bin", "HOME=" + t.TempDir()}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("postinstall failed: %v\n%s", err, out)
	}

	daemon := filepath.Join(root, "Library/LaunchDaemons/org.cirruslabs.tart-guest-daemon.plist")
	agent := filepath.Join(root, "Library/LaunchAgents/org.cirruslabs.tart-guest-agent.plist")
	for _, p := range []string{daemon, agent} {
		if err := exec.Command("plutil", "-lint", p).Run(); err != nil {
			t.Fatalf("%s is not a valid plist: %v", p, err)
		}
	}
	// Exec and IP resolution come from the root daemon, so they work from boot.
	if got := strings.Join(plistArgs(t, daemon), " "); got != "/usr/local/bin/tart-guest-agent --run-daemon --run-rpc" {
		t.Errorf("daemon runs %q", got)
	}
	// The user agent must not serve exec as well: both would claim the same port.
	got := strings.Join(plistArgs(t, agent), " ")
	if got != "/usr/local/bin/tart-guest-agent --run-vdagent" || strings.Contains(got, "--run-rpc") || strings.Contains(got, "--run-agent") {
		t.Errorf("user agent runs %q", got)
	}
	// Installing onto another volume must leave the running system's launchd alone.
	if b, _ := os.ReadFile(calls); len(b) != 0 {
		t.Errorf("launchd was touched while installing onto another volume:\n%s", b)
	}
}

func TestGuestAgentPostinstallStopsTheOldAgentBeforeStartingTheDaemon(t *testing.T) {
	b, err := os.ReadFile("guest-agent-postinstall.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(b)
	order := []string{
		`launchctl bootout gui/"$CONSOLE_UID"/"$AGENT_LABEL"`, // old per-user agent
		`pkill -f 'tart-guest-agent --run-agent'`,             // any other session's copy
		`launchctl bootstrap system "$DAEMON_PLIST"`,          // daemon takes over exec
		`launchctl bootstrap gui/"$CONSOLE_UID" "$AGENT_PLIST"`,
	}
	last := -1
	for _, step := range order {
		i := strings.Index(script, step)
		if i < 0 {
			t.Fatalf("postinstall is missing %q", step)
		}
		if i < last {
			t.Errorf("%q must come later in the script", step)
		}
		last = i
	}
}

func TestAgentPackageBuildUsesThePostinstallFile(t *testing.T) {
	b, err := os.ReadFile("build-agent-pkg.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `guest-agent-postinstall.sh" "$SCRIPTS_DIR/postinstall"`) {
		t.Error("build-agent-pkg.sh does not install guest-agent-postinstall.sh as the package's postinstall")
	}
	if strings.Contains(string(b), "--run-agent") {
		t.Error("build-agent-pkg.sh still carries its own copy of the old plists")
	}
}

// build-pkg.sh reuses packaging/build/tart-guest-agent-*.pkg; one built before the
// layout changed must be rebuilt rather than bundled.
func TestPkgBuildRebuildsAStaleAgentPackage(t *testing.T) {
	b, err := os.ReadFile("build-pkg.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `guest-agent-postinstall.sh" -nt "$AGENT_PKG"`) {
		t.Error("build-pkg.sh does not rebuild an agent package that is older than guest-agent-postinstall.sh")
	}
}

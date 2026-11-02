package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFindBundledGuestAgentPkgReturnsHighestVersion(t *testing.T) {
	tmpDir := t.TempDir()
	dir1 := filepath.Join(tmpDir, "dir1")
	dir2 := filepath.Join(tmpDir, "dir2")
	if err := os.MkdirAll(dir1, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir2, 0o755); err != nil {
		t.Fatal(err)
	}

	// Create some PKG files with different versions
	pkgs := []struct {
		dir  string
		name string
	}{
		{dir1, "tart-guest-agent-0.14.0.pkg"},
		{dir1, "tart-guest-agent-0.15.0.pkg"},
		{dir2, "tart-guest-agent-0.15.1.pkg"},
		{dir2, "tart-guest-agent-0.9.9.pkg"},
	}
	for _, p := range pkgs {
		if err := os.WriteFile(filepath.Join(p.dir, p.name), []byte("test"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Add a LICENSE file next to the highest version
	if err := os.WriteFile(filepath.Join(dir2, "tart-guest-agent-LICENSE.txt"), []byte("license"), 0o644); err != nil {
		t.Fatal(err)
	}

	pkgPath, licensePath, version := findBundledGuestAgentPkg([]string{dir1, dir2})
	if version != "0.15.1" {
		t.Errorf("version = %q, want 0.15.1", version)
	}
	if filepath.Base(pkgPath) != "tart-guest-agent-0.15.1.pkg" {
		t.Errorf("pkgPath = %q, want tart-guest-agent-0.15.1.pkg", pkgPath)
	}
	if licensePath == "" {
		t.Error("licensePath should be set")
	}
}

func TestFindBundledGuestAgentPkgReturnsFalseWhenEmpty(t *testing.T) {
	tmpDir := t.TempDir()
	pkgPath, licensePath, version := findBundledGuestAgentPkg([]string{tmpDir})
	if pkgPath != "" || licensePath != "" || version != "" {
		t.Errorf("expected empty results for empty dir, got pkg=%q license=%q version=%q",
			pkgPath, licensePath, version)
	}
}

func stagingFixture(t *testing.T, versions ...string) (*Manager, string, string) {
	t.Helper()
	m := newTestManager(t)
	tmp := t.TempDir()
	src := filepath.Join(tmp, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, v := range versions {
		if err := os.WriteFile(filepath.Join(src, "tart-guest-agent-"+v+".pkg"), []byte("pkg "+v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(src, "tart-guest-agent-LICENSE.txt"), []byte("license"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.guestAgentSearchDirs = []string{src}
	m.cfg.SharedDir = filepath.Join(tmp, "shared")
	return m, src, filepath.Join(m.cfg.SharedDir, "tart-guest-agent")
}

func TestStageGuestAgentInstallerCopiesPkgLicenseAndCommand(t *testing.T) {
	m, _, stage := stagingFixture(t, "0.15.0")
	m.stageGuestAgentInstaller()

	if !m.guestAgent.Available || m.guestAgent.Version != "0.15.0" {
		t.Fatalf("guestAgent = %+v, want available 0.15.0", m.guestAgent)
	}
	if want := "/Volumes/My Shared Files/host_resources/tart-guest-agent/tart-guest-agent-0.15.0.pkg"; m.guestAgent.GuestPath != want {
		t.Errorf("GuestPath = %q, want %q", m.guestAgent.GuestPath, want)
	}
	for _, f := range []string{"tart-guest-agent-0.15.0.pkg", "tart-guest-agent-LICENSE.txt"} {
		if _, err := os.Stat(filepath.Join(stage, f)); err != nil {
			t.Errorf("%s not staged: %v", f, err)
		}
	}
	cmd := filepath.Join(stage, "Install Tart Guest Agent.command")
	info, err := os.Stat(cmd)
	if err != nil {
		t.Fatalf(".command not staged: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf(".command is not executable: %v", info.Mode())
	}
	body, _ := os.ReadFile(cmd)
	if !strings.Contains(string(body), `installer -pkg "tart-guest-agent-0.15.0.pkg" -target /`) {
		t.Errorf(".command does not install the staged package:\n%s", body)
	}
}

func TestStageGuestAgentInstallerIsIdempotent(t *testing.T) {
	m, _, stage := stagingFixture(t, "0.15.0")
	m.stageGuestAgentInstaller()
	staged := filepath.Join(stage, "tart-guest-agent-0.15.0.pkg")
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(staged, old, old); err != nil {
		t.Fatal(err)
	}

	m.stageGuestAgentInstaller()

	info, err := os.Stat(staged)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(old) {
		t.Error("identical package was copied again on the second run")
	}
}

func TestStageGuestAgentInstallerReplacesOlderPkg(t *testing.T) {
	m, src, stage := stagingFixture(t, "0.14.2")
	m.stageGuestAgentInstaller()
	if err := os.Remove(filepath.Join(src, "tart-guest-agent-0.14.2.pkg")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "tart-guest-agent-0.15.0.pkg"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}

	m.stageGuestAgentInstaller()

	if _, err := os.Stat(filepath.Join(stage, "tart-guest-agent-0.14.2.pkg")); !os.IsNotExist(err) {
		t.Error("older staged package was not removed")
	}
	if m.guestAgent.Version != "0.15.0" {
		t.Errorf("Version = %q, want 0.15.0", m.guestAgent.Version)
	}
}

func TestStageGuestAgentInstallerWithoutPkgIsUnavailable(t *testing.T) {
	m, _, stage := stagingFixture(t)
	m.stageGuestAgentInstaller()
	if m.guestAgent.Available || m.guestAgent.Error != errNoBundledAgent {
		t.Errorf("guestAgent = %+v, want unavailable with %q", m.guestAgent, errNoBundledAgent)
	}
	if _, err := os.Stat(stage); !os.IsNotExist(err) {
		t.Error("staging folder created although there was nothing to stage")
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
	// The error message should mention that SSH fallback is turned off
	if res.Error == "" {
		t.Fatal("expected an error when agent is absent and fallback is off")
	}
	if m.vms["vm1"].AgentOK {
		t.Fatal("agentOk should be false when the agent did not answer")
	}
}

// Package packaging holds the tests for the shell tooling in this directory.
package packaging

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// buildLauncher builds "Tart Oven.app" into a temp dir, pointed at bin, and
// returns the path of its executable.
func buildLauncher(t *testing.T, bin string) (appDir, exe string) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("the launcher app is built with macOS tools (sips, iconutil, plutil)")
	}
	for _, tool := range []string{"sips", "iconutil", "plutil"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
	out := t.TempDir()
	cmd := exec.Command("./build-launcher-app.sh", out, "2.1.0-dev9")
	cmd.Env = append(os.Environ(), "LAUNCHER_BIN="+bin)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build-launcher-app.sh: %v\n%s", err, b)
	}
	appDir = filepath.Join(out, "Tart Oven.app")
	return appDir, filepath.Join(appDir, "Contents", "MacOS", "tart-oven-launcher")
}

func writeScript(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// runLauncher runs the executable the way Finder would: no TTY, a bare
// environment, and stub osascript on PATH so no alert is ever shown.
func runLauncher(t *testing.T, exe, stubDir string) error {
	t.Helper()
	cmd := exec.Command(exe)
	cmd.Env = []string{"HOME=" + t.TempDir(), "PATH=" + stubDir + ":/usr/bin:/bin"}
	return cmd.Run()
}

func TestLauncherBundleIsWellFormed(t *testing.T) {
	app, exe := buildLauncher(t, "/nonexistent/tart-oven")
	plist := filepath.Join(app, "Contents", "Info.plist")
	if err := exec.Command("plutil", "-lint", plist).Run(); err != nil {
		t.Fatalf("Info.plist is invalid: %v", err)
	}
	for key, want := range map[string]string{
		"CFBundleExecutable": "tart-oven-launcher", "CFBundleIconFile": "TartOven",
		"CFBundleIdentifier": "com.tartoven.launcher", "LSUIElement": "true",
		"CFBundleShortVersionString": "2.1.0", // a -dev suffix is not a valid bundle version
	} {
		out, err := exec.Command("plutil", "-extract", key, "raw", "-o", "-", plist).Output()
		if err != nil || strings.TrimSpace(string(out)) != want {
			t.Errorf("%s = %q (%v), want %q", key, out, err, want)
		}
	}
	if fi, err := os.Stat(exe); err != nil || fi.Mode()&0o111 == 0 {
		t.Errorf("launcher executable missing or not executable: %v", err)
	}
	icns := filepath.Join(app, "Contents", "Resources", "TartOven.icns")
	if fi, err := os.Stat(icns); err != nil || fi.Size() < 1000 {
		t.Errorf("icon missing or empty: %v", err)
	}
}

func TestLauncherRunsOpenAndStaysQuietOnSuccess(t *testing.T) {
	stubs := t.TempDir()
	bin := filepath.Join(stubs, "tart-oven")
	log := filepath.Join(stubs, "calls")
	writeScript(t, bin, `echo "tart-oven $*" >> "`+log+`"`)
	writeScript(t, filepath.Join(stubs, "osascript"), `echo "osascript $*" >> "`+log+`"`)
	_, exe := buildLauncher(t, bin)

	if err := runLauncher(t, exe, stubs); err != nil {
		t.Fatalf("launcher failed: %v", err)
	}
	got, _ := os.ReadFile(log)
	if strings.TrimSpace(string(got)) != "tart-oven open" {
		t.Fatalf("calls = %q, want only \"tart-oven open\" (no alert on success)", got)
	}
}

func TestLauncherShowsTheReasonWhenStartFails(t *testing.T) {
	stubs := t.TempDir()
	bin := filepath.Join(stubs, "tart-oven")
	log := filepath.Join(stubs, "calls")
	writeScript(t, bin, `echo "tart-oven: started, but nothing answers at http://127.0.0.1:9000 yet" >&2; exit 1`)
	writeScript(t, filepath.Join(stubs, "osascript"), `for a in "$@"; do echo "$a"; done >> "`+log+`"`)
	_, exe := buildLauncher(t, bin)

	if err := runLauncher(t, exe, stubs); err == nil {
		t.Fatal("launcher should exit non-zero when the server cannot start")
	}
	got, _ := os.ReadFile(log)
	for _, want := range []string{"could not start", "nothing answers at http://127.0.0.1:9000"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("alert missing %q; osascript got:\n%s", want, got)
		}
	}
}

func TestLauncherExplainsAMissingInstall(t *testing.T) {
	stubs := t.TempDir()
	log := filepath.Join(stubs, "calls")
	writeScript(t, filepath.Join(stubs, "osascript"), `for a in "$@"; do echo "$a"; done >> "`+log+`"`)
	_, exe := buildLauncher(t, filepath.Join(stubs, "missing"))

	if err := runLauncher(t, exe, stubs); err == nil {
		t.Fatal("launcher should exit non-zero when tart-oven is not installed")
	}
	if got, _ := os.ReadFile(log); !strings.Contains(string(got), "not installed") {
		t.Errorf("alert = %q", got)
	}
}

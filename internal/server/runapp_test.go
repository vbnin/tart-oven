package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunAppNameAcceptsOnlySafeBundleNames(t *testing.T) {
	for _, ok := range []string{"HelloVM.app", "Hello VM.app", "my-app_2.app", "A.b.app"} {
		if !runAppNameRe.MatchString(ok) {
			t.Errorf("%q should be accepted", ok)
		}
	}
	for _, bad := range []string{"", ".app", "app", "-x.app", "a'b.app", "a$b.app", "a;b.app", "a/b.app", "HelloVM.zip", "x.app.exe"} {
		if runAppNameRe.MatchString(bad) {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestRunAppScriptQuotesTheGuestPath(t *testing.T) {
	// A VM name may hold a quote (validateVMName allows it); the script must still
	// be one well-formed word.
	guest := runAppGuestPath("Mac's VM", "run-1", "HelloVM.app")
	script := runAppScript(guest)
	if !strings.Contains(script, `app='/Volumes/My Shared Files/host_resources/vm-apps/Mac'\''s VM/run-1/HelloVM.app'`) {
		t.Fatalf("guest path not quoted safely in script:\n%s", script)
	}
	if !strings.Contains(script, "sleep 5\n") {
		t.Fatalf("script should wait %v before reading the log", runAppWait)
	}
	if strings.Contains(script, "%!") {
		t.Fatalf("script has an unexpanded format verb:\n%s", script)
	}
	if !strings.Contains(script, `stat -f %Su /dev/console`) {
		t.Fatal("script must refuse to run unless the VM user is at the console")
	}
}

func TestParseRunAppOutput(t *testing.T) {
	res, ok := parseRunAppOutput("pid=4242\nrunning=1\n--- log ---\nhello\nworld\n")
	if !ok || res.Pid != 4242 || !res.Running || res.Log != "hello\nworld\n" {
		t.Fatalf("got %+v ok=%v", res, ok)
	}
	if res, ok := parseRunAppOutput("pid=7\nrunning=0\n--- log ---\n"); !ok || res.Running {
		t.Fatalf("an app that exited must report running=false, got %+v ok=%v", res, ok)
	}
	for _, bad := range []string{"", "pid=0\nrunning=1\n--- log ---\n", "pid=abc\nrunning=1\n--- log ---\n", "hello\n", "pid=12\n--- log ---\n", "pid=12\nrunning=1\nno marker"} {
		if _, ok := parseRunAppOutput(bad); ok {
			t.Errorf("output %q should not parse", bad)
		}
	}
}

// A VM name may hold characters the shell would read; the script must still be
// one valid program, with the path as a single word.
func TestRunAppScriptIsValidShellForAHostileVMName(t *testing.T) {
	script := runAppScript(runAppGuestPath(`a'b $(touch x) c`, "run-2", "App.app"))
	path := filepath.Join(t.TempDir(), "run.sh")
	if err := os.WriteFile(path, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("/bin/sh", "-n", path).CombinedOutput(); err != nil {
		t.Fatalf("script does not parse: %v: %s", err, out)
	}
}

func TestRunAppHandlerRefusesOtherMethods(t *testing.T) {
	m := &Manager{}
	rec := httptest.NewRecorder()
	m.handleRunApp(rec, httptest.NewRequest(http.MethodGet, "/api/vm/run-app", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", rec.Code)
	}
}

func TestRunAppHandlerRejectsBadRequests(t *testing.T) {
	bundle := makeTestApp(t, "Rejects.app", "#!/bin/sh\nsleep 1\n")
	m := &Manager{
		vms: map[string]*VM{
			"HelloVM":  {Name: "HelloVM", State: "running"},
			"Stopped":  {Name: "Stopped", State: "stopped"},
			"NoBundle": {Name: "NoBundle", State: "running"},
		},
		cfg: Config{SharedDir: t.TempDir()},
	}
	cases := []struct {
		name, body string
		status     int
	}{
		{"relative path", `{"name":"HelloVM","appPath":"Rejects.app"}`, http.StatusBadRequest},
		{"not an app", `{"name":"HelloVM","appPath":"` + t.TempDir() + `/Rejects.zip"}`, http.StatusBadRequest},
		{"bad VM name", `{"name":"a/b","appPath":"` + bundle + `"}`, http.StatusBadRequest},
		{"VM not running", `{"name":"Stopped","appPath":"` + bundle + `"}`, http.StatusConflict},
		{"unknown VM", `{"name":"Nope","appPath":"` + bundle + `"}`, http.StatusNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/vm/run-app", strings.NewReader(c.body))
			m.handleRunApp(rec, req)
			if rec.Code != c.status {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, c.status, rec.Body.String())
			}
			var resp runAppResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.OK || resp.Error == "" {
				t.Fatalf("want an error reply, got %s (%v)", rec.Body.String(), err)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(m.cfg.SharedDir, runAppStageDir)); err == nil {
		t.Fatal("a rejected request must not stage anything in the shared folder")
	}
}

// makeTestApp builds a minimal app bundle whose executable runs body.
func makeTestApp(t *testing.T, name, body string) string {
	t.Helper()
	app := filepath.Join(t.TempDir(), name)
	macos := filepath.Join(app, "Contents", "MacOS")
	if err := os.MkdirAll(macos, 0o755); err != nil {
		t.Fatal(err)
	}
	base := strings.TrimSuffix(name, ".app")
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>CFBundleExecutable</key><string>` + base + `</string>
<key>CFBundleIdentifier</key><string>test.tartoven.` + base + `</string>
<key>CFBundlePackageType</key><string>APPL</string>
</dict></plist>`
	if err := os.WriteFile(filepath.Join(app, "Contents", "Info.plist"), []byte(plist), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(macos, base), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return app
}

// consoleUserIsMe reports whether the user running the test owns the console,
// which the guest script requires. Without that the end-to-end test is skipped.
func consoleUserIsMe(t *testing.T) bool {
	t.Helper()
	out, err := exec.Command("/usr/bin/stat", "-f", "%Su", "/dev/console").Output()
	if err != nil {
		return false
	}
	me, err := exec.Command("/usr/bin/id", "-un").Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == strings.TrimSpace(string(me))
}

// TestRunAppEndToEndOnHost runs the real staging and guest script on this Mac
// with a fake tart that runs `tart exec` commands locally. The "app" is a
// shell script, so no window opens; this checks the plumbing, not the GUI.
// The guest itself is still a live check.
func TestRunAppEndToEndOnHost(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("needs macOS ditto, stat and PlistBuddy")
	}
	if !consoleUserIsMe(t) {
		t.Skip("the logged-in console user is not the one running the test")
	}
	for _, tool := range []string{"/usr/bin/ditto", "/usr/libexec/PlistBuddy", "/bin/ps"} {
		if _, err := os.Stat(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}

	shared := t.TempDir()
	// `tart exec <vm> /bin/sh -c <guest script> tart-oven <user> <command>`: drop
	// "exec" and the VM name, point the guest's shared mount at the test folder
	// in the command text, and run the rest here.
	fakeTart := filepath.Join(t.TempDir(), "tart")
	fake := fmt.Sprintf(`#!/bin/sh
shift 2
fixed=$(printf '%%s' "$6" | sed 's#%s#%s#g')
exec "$1" "$2" "$3" "$4" "$5" "$fixed"
`, guestSharedMount, shared)
	if err := os.WriteFile(fakeTart, []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	app := fmt.Sprintf("TartOvenTest%d.app", time.Now().UnixNano())
	bundle := makeTestApp(t, app, "#!/bin/sh\necho \"hello from the test app\"\nsleep 20\n")
	base := strings.TrimSuffix(app, ".app")
	logDir := filepath.Join(runAppLogDir, base)
	t.Cleanup(func() {
		data, err := os.ReadFile(filepath.Join(logDir, "pid"))
		if err == nil {
			var pid int
			fmt.Sscan(string(data), &pid)
			if pid > 0 {
				// The test app's sleep is a child of the app's shell; end it too.
				exec.Command("/usr/bin/pkill", "-P", strconv.Itoa(pid)).Run()
				syscall.Kill(pid, syscall.SIGKILL)
			}
		}
		os.RemoveAll(logDir)
		os.Remove(runAppLogDir) // only if nothing else is left in it
	})

	m := &Manager{
		vms: map[string]*VM{"HelloVM": {Name: "HelloVM", State: "running"}},
		cfg: Config{SharedDir: shared, TartAppPath: fakeTart, VMStoragePath: t.TempDir()},
	}
	run := func() runAppResponse {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"name": "HelloVM", "appPath": bundle})
		rec := httptest.NewRecorder()
		m.handleRunApp(rec, httptest.NewRequest(http.MethodPost, "/api/vm/run-app", bytes.NewReader(body)))
		var resp runAppResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("bad reply %q: %v", rec.Body.String(), err)
		}
		if rec.Code != http.StatusOK || !resp.OK {
			t.Fatalf("status %d, reply %+v", rec.Code, resp)
		}
		return resp
	}

	first := run()
	if first.Pid <= 0 || !first.Running || !strings.Contains(first.Log, "hello from the test app") {
		t.Fatalf("first run: pid=%d running=%v log=%q", first.Pid, first.Running, first.Log)
	}
	copies := func() []string {
		m, _ := filepath.Glob(filepath.Join(shared, runAppStageDir, "HelloVM", "*", app, "Contents", "MacOS", base))
		return m
	}
	if len(copies()) != 1 {
		t.Fatalf("after one run want one staged copy, got %v", copies())
	}

	// A second run replaces the first: the old process is stopped.
	second := run()
	if second.Pid == first.Pid || !second.Running {
		t.Fatalf("second run: pid=%d (first was %d), running=%v", second.Pid, first.Pid, second.Running)
	}
	if !pidGoneSoon(first.Pid, 3*time.Second) {
		t.Fatalf("first run (pid %d) is still alive after the second run", first.Pid)
	}
	if len(copies()) != 1 {
		t.Fatalf("after two runs want only the newest staged copy, got %v", copies())
	}
}

// pidGoneSoon reports whether pid ends within d. A zombie counts as ended: it
// has stopped running and only waits to be reaped.
func pidGoneSoon(pid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		out, err := exec.Command("/bin/ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
		if err != nil || strings.HasPrefix(strings.TrimSpace(string(out)), "Z") || len(strings.TrimSpace(string(out))) == 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// A guest can write into the shared folder, so it can replace the per-VM folder
// with a symlink to a host folder. Staging must not follow it.
func TestRunAppStagingRefusesSymlinkedVMFolder(t *testing.T) {
	shared := t.TempDir()
	victim := t.TempDir()
	if err := os.MkdirAll(filepath.Join(shared, runAppStageDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(shared, runAppStageDir, "HelloVM")); err != nil {
		t.Fatal(err)
	}
	// A host app already sitting in the victim folder, with the same name as the app to run.
	existing := filepath.Join(victim, "App.app")
	marker := filepath.Join(existing, "keep.txt")
	if err := os.MkdirAll(existing, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("host data"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := makeTestApp(t, "App.app", "#!/bin/sh\n")

	_, err := stageRunApp(context.Background(), shared, "HelloVM", src)
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Fatalf("host data behind the symlink was deleted: %v", statErr)
	}
	if err == nil {
		t.Fatal("staging into a symlinked VM folder must fail")
	}
}

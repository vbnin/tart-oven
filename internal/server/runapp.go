package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Run an app built on the host inside a running guest. The app is copied into
// the VM's shared folder, then started in the logged-in user's session with its
// stdout and stderr written to a log in the guest, and the first few seconds of
// that log are returned. No debugger is attached.

const (
	// runAppStageDir is the folder under SharedDir that holds one subfolder per
	// VM, with the copies of the apps run there.
	runAppStageDir = "vm-apps"
	// runAppLogDir is where the guest keeps, per app, the pid and the log.
	runAppLogDir = "/private/tmp/tart-oven-run"
	// runAppWait is how long the app runs before its log is returned.
	runAppWait = 5 * time.Second
	// runAppTimeout bounds the whole call: the copy and the guest run.
	runAppTimeout = 2 * time.Minute
	// runAppLogTail caps the log bytes returned.
	runAppLogTail = 32 << 10
)

// Exit statuses of the guest script that the handler maps to HTTP codes.
const (
	runAppExitNotAtConsole = 3
)

// runAppNameRe accepts an app bundle name that is safe to put in a path and in
// the guest script: letters, digits, space, dot, dash and underscore.
var runAppNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._-]{0,127}\.app$`)

// runAppLocks serialises runs of the same app on the same VM: two copies into
// the same folder at once would race each other and the guest script.
var runAppLocks sync.Map // "vm/app" -> *sync.Mutex

func lockRunApp(key string) func() {
	v, _ := runAppLocks.LoadOrStore(key, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// runAppGuestPath is where the guest sees the staged copy of app for one run.
func runAppGuestPath(vm, runID, app string) string {
	return guestSharedMount + "/" + runAppStageDir + "/" + vm + "/" + runID + "/" + app
}

// runAppScriptTemplate runs in the guest as the VM's user. It refuses to start
// unless that user is the one logged in at the console (a GUI app needs that
// session), stops the copy it started last time, starts the new one with its
// output going to a log, and prints the pid, whether it is still running, and
// the log after the wait.
// %[1]s is the quoted guest path of the app, %[2]s the log folder, %[3]d the
// wait in seconds and %[4]d the tail size in bytes.
const runAppScriptTemplate = `set -u
app=%[1]s
user=$(/usr/bin/id -un)
if [ "$user" = root ] || [ "$(/usr/bin/stat -f %%Su /dev/console 2>/dev/null)" != "$user" ]; then
  echo "$user is not logged in at the console, so the app cannot open a window in this guest" >&2
  exit 3
fi
exe_name=$(/usr/libexec/PlistBuddy -c 'Print :CFBundleExecutable' "$app/Contents/Info.plist" 2>/dev/null) || { echo "no CFBundleExecutable in $app" >&2; exit 4; }
exe="$app/Contents/MacOS/$exe_name"
[ -x "$exe" ] || { echo "no executable at $exe" >&2; exit 4; }
dir=%[2]s/$(basename "$app" .app)
mkdir -p "$dir"
if [ -f "$dir/pid" ]; then
  old=$(cat "$dir/pid")
  case $old in ''|*[!0-9]*) old="" ;; esac
  # Earlier runs live in other folders, so match the bundle name, not the full path.
  match="/$(basename "$app")/Contents/MacOS/$exe_name"
  if [ -n "$old" ] && ps -p "$old" -o command= 2>/dev/null | grep -qF "$match"; then
    kill "$old" 2>/dev/null
    sleep 1
  fi
fi
"$exe" > "$dir/run.log" 2>&1 < /dev/null &
pid=$!
echo "$pid" > "$dir/pid"
echo "pid=$pid"
sleep %[3]d
if kill -0 "$pid" 2>/dev/null; then echo "running=1"; else echo "running=0"; fi
echo "--- log ---"
tail -c %[4]d "$dir/run.log"
`

// runAppScript builds the guest script for the app at guestApp.
func runAppScript(guestApp string) string {
	return fmt.Sprintf(runAppScriptTemplate,
		shellQuote(guestApp), runAppLogDir, int(runAppWait/time.Second), runAppLogTail)
}

// runAppResult is what the guest script reported.
type runAppResult struct {
	Pid     int
	Running bool
	Log     string
}

// parseRunAppOutput reads the script's output: "pid=N" and "running=0|1"
// lines, then the log after a "--- log ---" line.
func parseRunAppOutput(out string) (runAppResult, bool) {
	head, rest, found := strings.Cut(out, "--- log ---\n")
	if !found {
		return runAppResult{}, false
	}
	var res runAppResult
	var sawPid, sawRunning bool
	for _, line := range strings.Split(head, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "pid="):
			pid, err := strconv.Atoi(strings.TrimPrefix(line, "pid="))
			if err != nil || pid <= 0 {
				return runAppResult{}, false
			}
			res.Pid, sawPid = pid, true
		case strings.HasPrefix(line, "running="):
			res.Running, sawRunning = strings.TrimPrefix(line, "running=") == "1", true
		}
	}
	if !sawPid || !sawRunning {
		return runAppResult{}, false
	}
	res.Log = rest
	return res, true
}

// prepareStageDir returns <sharedDir>/vm-apps/<vm>, creating the folders that
// are missing. A folder that exists as anything but a plain folder (a symlink,
// say) is refused. The guest can write to the shared folder, so it can put a
// symlink there; following it would let a copy or a delete land on the host
// outside the shared folder.
func prepareStageDir(sharedDir, vm string) (string, error) {
	base := filepath.Join(sharedDir, runAppStageDir)
	for _, dir := range []string{base, filepath.Join(base, vm)} {
		fi, err := os.Lstat(dir)
		if errors.Is(err, fs.ErrNotExist) {
			if err := os.Mkdir(dir, 0o755); err != nil {
				return "", err
			}
			continue
		}
		if err != nil {
			return "", err
		}
		if !fi.IsDir() {
			return "", fmt.Errorf("%s is not a plain folder, so nothing is copied there", dir)
		}
	}
	return filepath.Join(base, vm), nil
}

// newRunID names the folder one run's copy goes into.
func newRunID() string {
	return "run-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

// stageRunApp copies the app bundle src into a new folder of its own,
// <sharedDir>/vm-apps/<vm>/<run id>/<name>, and returns the run id. Each run gets
// a new folder: replacing a bundle at the same path breaks what the guest sees
// of it (the guest keeps the old folder and shows the new one empty). ditto keeps
// the bundle's extended attributes and resource forks, which a plain copy can lose.
func stageRunApp(ctx context.Context, sharedDir, vm, src string) (string, error) {
	stage, err := prepareStageDir(sharedDir, vm)
	if err != nil {
		return "", err
	}
	runID := newRunID()
	runDir := filepath.Join(stage, runID)
	if err := os.Mkdir(runDir, 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(runDir, filepath.Base(src))
	out, err := exec.CommandContext(ctx, "/usr/bin/ditto", src, dst).CombinedOutput()
	if err != nil {
		os.RemoveAll(runDir)
		return "", fmt.Errorf("copy %s: %v: %s", filepath.Base(src), err, strings.TrimSpace(string(out)))
	}
	return runID, nil
}

// pruneRunAppCopies removes the copies of app from earlier runs of vm, keeping
// keepRunID. Call it only after the new run has stopped the old process. Only
// run folders that hold the same app are removed; symlinks are left alone.
func pruneRunAppCopies(sharedDir, vm, app, keepRunID string) error {
	stage := filepath.Join(sharedDir, runAppStageDir, vm)
	entries, err := os.ReadDir(stage)
	if err != nil {
		return err
	}
	var firstErr error
	for _, e := range entries {
		if e.Name() == keepRunID || !e.IsDir() { // IsDir is false for symlinks
			continue
		}
		old := filepath.Join(stage, e.Name())
		if _, err := os.Lstat(filepath.Join(old, app)); err != nil {
			continue
		}
		if err := os.RemoveAll(old); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// runAppResponse is the reply of POST /api/vm/run-app.
type runAppResponse struct {
	OK      bool   `json:"ok"`
	VM      string `json:"vm,omitempty"`
	App     string `json:"app,omitempty"`
	Pid     int    `json:"pid,omitempty"`
	Running bool   `json:"running"`
	Log     string `json:"log,omitempty"`
	Via     string `json:"via,omitempty"`
	Error   string `json:"error,omitempty"`
}

func writeRunAppResponse(w http.ResponseWriter, status int, resp runAppResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(resp)
}

// handleRunApp copies a host app bundle into a running VM's shared folder and
// starts it there. Body: {"name": "<vm>", "appPath": "/absolute/path/App.app"}.
func (m *Manager) handleRunApp(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeRunAppResponse(w, http.StatusMethodNotAllowed, runAppResponse{Error: "use POST"})
		return
	}
	var body struct {
		Name    string `json:"name"`
		AppPath string `json:"appPath"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeRunAppResponse(w, http.StatusBadRequest, runAppResponse{Error: err.Error()})
		return
	}
	if err := validateVMName(body.Name); err != nil {
		writeRunAppResponse(w, http.StatusBadRequest, runAppResponse{Error: err.Error()})
		return
	}
	if !filepath.IsAbs(body.AppPath) {
		writeRunAppResponse(w, http.StatusBadRequest, runAppResponse{Error: "appPath must be an absolute path"})
		return
	}
	app := filepath.Clean(body.AppPath)
	base := filepath.Base(app)
	if !runAppNameRe.MatchString(base) {
		writeRunAppResponse(w, http.StatusBadRequest, runAppResponse{Error: "the app must be a .app bundle whose name has only letters, digits, spaces, dots, dashes and underscores"})
		return
	}
	if fi, err := os.Stat(filepath.Join(app, "Contents", "MacOS")); err != nil || !fi.IsDir() {
		writeRunAppResponse(w, http.StatusBadRequest, runAppResponse{Error: "no app bundle at " + app})
		return
	}

	m.mu.Lock()
	vm := m.vms[body.Name]
	running := vm != nil && vm.State == "running"
	sharedDir := m.cfg.SharedDir
	m.mu.Unlock()
	if vm == nil {
		writeRunAppResponse(w, http.StatusNotFound, runAppResponse{VM: body.Name, Error: "there is no VM named " + body.Name})
		return
	}
	if sharedDir == "" {
		writeRunAppResponse(w, http.StatusInternalServerError, runAppResponse{Error: "the shared folder is not configured"})
		return
	}
	if !running {
		writeRunAppResponse(w, http.StatusConflict, runAppResponse{VM: body.Name, Error: "VM " + body.Name + " is not running; start it first"})
		return
	}

	defer lockRunApp(body.Name + "/" + base)()
	ctx, cancel := context.WithTimeout(r.Context(), runAppTimeout)
	defer cancel()

	runID, err := stageRunApp(ctx, sharedDir, body.Name, app)
	if err != nil {
		m.logln("run app %s on %s: %v", base, body.Name, err)
		writeRunAppResponse(w, http.StatusInternalServerError, runAppResponse{VM: body.Name, Error: err.Error()})
		return
	}

	m.logln("run app %s on %s", base, body.Name)
	res := m.execInGuest(ctx, body.Name, runAppScript(runAppGuestPath(body.Name, runID, base)), "")
	if res.Error != "" {
		writeRunAppResponse(w, http.StatusBadGateway, runAppResponse{VM: body.Name, App: base, Error: res.Error})
		return
	}
	if res.ExitCode != 0 {
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = fmt.Sprintf("the guest command exited with status %d", res.ExitCode)
		}
		status := http.StatusBadGateway
		if res.ExitCode == runAppExitNotAtConsole {
			status = http.StatusConflict
		}
		writeRunAppResponse(w, status, runAppResponse{VM: body.Name, App: base, Via: res.Via, Error: msg})
		return
	}
	out, ok := parseRunAppOutput(res.Stdout)
	if !ok {
		writeRunAppResponse(w, http.StatusBadGateway, runAppResponse{VM: body.Name, App: base, Via: res.Via, Error: "unexpected output from the guest"})
		return
	}
	// The new run has stopped the old process, so its copies can go now.
	if err := pruneRunAppCopies(sharedDir, body.Name, base, runID); err != nil {
		m.logln("run app %s on %s: could not remove old copies: %v", base, body.Name, err)
	}
	writeRunAppResponse(w, http.StatusOK, runAppResponse{OK: true, VM: body.Name, App: base, Pid: out.Pid, Running: out.Running, Log: out.Log, Via: res.Via})
}

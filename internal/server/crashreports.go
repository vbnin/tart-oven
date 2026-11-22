package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Crash reports read from a guest's DiagnosticReports folder. Read-only: the
// guest is not changed. The folder is the one the logged-in user's apps write
// to, and commands run as the VM's user, so $HOME is that user's home.

const (
	// crashShowMaxBytes caps one report's size in the reply.
	crashShowMaxBytes = 4 << 20
	// crashCommandTimeout bounds one guest command.
	crashCommandTimeout = 60 * time.Second
)

// crashAppRe accepts an app name used as a filter: letters, digits, space, dot,
// dash and underscore.
var crashAppRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._-]{0,127}$`)

// crashFileRe accepts a report file name, and nothing with a slash in it.
var crashFileRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._-]{0,199}\.(ips|crash)$`)

// crashListTemplate prints "<mtime epoch>\t<name>" for each report, newest first.
// %[1]s is the quoted app name; an empty name lists every report.
const crashListTemplate = `app=%[1]s
d="$HOME/Library/Logs/DiagnosticReports"
[ -d "$d" ] || exit 0
for f in "$d"/*.ips "$d"/*.crash; do
  [ -f "$f" ] || continue
  n=${f##*/}
  if [ -n "$app" ]; then case $n in "$app"-*) ;; *) continue ;; esac; fi
  printf '%%s\t%%s\n' "$(/usr/bin/stat -f %%m "$f")" "$n"
done | sort -rn
`

// crashShowTemplate prints one report, capped. %[1]s is the quoted file name and
// %[2]d the cap in bytes. Exit status 4 means there is no such report.
const crashShowTemplate = `f="$HOME/Library/Logs/DiagnosticReports/"%[1]s
[ -f "$f" ] || { echo "no crash report named $(basename "$f")" >&2; exit 4; }
head -c %[2]d "$f"
`

func crashListScript(app string) string {
	return fmt.Sprintf(crashListTemplate, shellQuote(app))
}

func crashShowScript(file string) string {
	return fmt.Sprintf(crashShowTemplate, shellQuote(file), crashShowMaxBytes)
}

// crashReportEntry is one report in the list.
type crashReportEntry struct {
	Name     string    `json:"name"`
	Modified time.Time `json:"modified"`
}

// parseCrashList reads the listing script's output. Lines that do not fit are
// skipped, so a stray line cannot pass a bad name on.
func parseCrashList(out string) []crashReportEntry {
	var reports []crashReportEntry
	for _, line := range strings.Split(out, "\n") {
		epoch, name, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok || !crashFileRe.MatchString(name) {
			continue
		}
		secs, err := strconv.ParseInt(epoch, 10, 64)
		if err != nil {
			continue
		}
		reports = append(reports, crashReportEntry{Name: name, Modified: time.Unix(secs, 0)})
	}
	return reports
}

// guestRefusal says why a guest command cannot run on name: 404 for no such VM,
// 409 for a VM that is not running. It returns 0 when the command may run.
func (m *Manager) guestRefusal(name string) (int, string) {
	m.mu.Lock()
	vm := m.vms[name]
	running := vm != nil && vm.State == "running"
	m.mu.Unlock()
	switch {
	case vm == nil:
		return http.StatusNotFound, "there is no VM named " + name
	case !running:
		return http.StatusConflict, "VM " + name + " is not running; start it first"
	}
	return 0, ""
}

type crashError struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

func writeCrashJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// handleCrashReports lists the crash reports in a running VM, newest first.
// GET /api/vm/crash-reports?name=<vm>[&app=<app name>]
func (m *Manager) handleCrashReports(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeCrashJSON(w, http.StatusMethodNotAllowed, crashError{Error: "use GET"})
		return
	}
	name := r.URL.Query().Get("name")
	app := r.URL.Query().Get("app")
	if err := validateVMName(name); err != nil {
		writeCrashJSON(w, http.StatusBadRequest, crashError{Error: err.Error()})
		return
	}
	if app != "" && !crashAppRe.MatchString(app) {
		writeCrashJSON(w, http.StatusBadRequest, crashError{Error: "app must be an app name with only letters, digits, spaces, dots, dashes and underscores"})
		return
	}
	if status, msg := m.guestRefusal(name); status != 0 {
		writeCrashJSON(w, status, crashError{Error: msg})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), crashCommandTimeout)
	defer cancel()
	res := m.execInGuest(ctx, name, crashListScript(app), "")
	if res.Error != "" || res.ExitCode != 0 {
		msg := res.Error
		if msg == "" {
			msg = strings.TrimSpace(res.Stderr)
		}
		writeCrashJSON(w, http.StatusBadGateway, crashError{Error: msg})
		return
	}
	writeCrashJSON(w, http.StatusOK, struct {
		OK      bool               `json:"ok"`
		VM      string             `json:"vm"`
		App     string             `json:"app,omitempty"`
		Reports []crashReportEntry `json:"reports"`
	}{OK: true, VM: name, App: app, Reports: parseCrashList(res.Stdout)})
}

// handleCrashReport returns one crash report's text from a running VM.
// GET /api/vm/crash-report?name=<vm>&file=<report file name>
func (m *Manager) handleCrashReport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeCrashJSON(w, http.StatusMethodNotAllowed, crashError{Error: "use GET"})
		return
	}
	name := r.URL.Query().Get("name")
	file := r.URL.Query().Get("file")
	if err := validateVMName(name); err != nil {
		writeCrashJSON(w, http.StatusBadRequest, crashError{Error: err.Error()})
		return
	}
	if !crashFileRe.MatchString(file) {
		writeCrashJSON(w, http.StatusBadRequest, crashError{Error: "file must be the name of a .ips or .crash report"})
		return
	}
	if status, msg := m.guestRefusal(name); status != 0 {
		writeCrashJSON(w, status, crashError{Error: msg})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), crashCommandTimeout)
	defer cancel()
	res := m.execInGuest(ctx, name, crashShowScript(file), "")
	switch {
	case res.Error != "":
		writeCrashJSON(w, http.StatusBadGateway, crashError{Error: res.Error})
	case res.ExitCode == 4:
		writeCrashJSON(w, http.StatusNotFound, crashError{Error: strings.TrimSpace(res.Stderr)})
	case res.ExitCode != 0:
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = fmt.Sprintf("the guest command exited with status %d", res.ExitCode)
		}
		writeCrashJSON(w, http.StatusBadGateway, crashError{Error: msg})
	default:
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(res.Stdout))
	}
}

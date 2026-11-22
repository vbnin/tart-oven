package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCrashFileRegexAcceptsOnlyReportNames(t *testing.T) {
	for _, ok := range []string{"HelloTart-2026-11-22-113000.ips", "Hello Tart_1.crash", "A.b-c.ips"} {
		if !crashFileRe.MatchString(ok) {
			t.Errorf("%q should be accepted", ok)
		}
	}
	for _, bad := range []string{"", ".ips", "../x.ips", "a/b.ips", "x.txt", "a'b.ips", "a$(id).ips", "a;b.crash", "x.ips.exe"} {
		if crashFileRe.MatchString(bad) {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestParseCrashListSkipsLinesThatDoNotFit(t *testing.T) {
	out := "1700000300\tHelloTart-3.ips\n" +
		"1700000200\tHelloTart-2.ips\n" +
		"junk line\n" +
		"notanumber\tHelloTart-4.ips\n" +
		"1700000100\t../escape.ips\n" +
		"1700000100\tbad'name.ips\n"
	got := parseCrashList(out)
	if len(got) != 2 || got[0].Name != "HelloTart-3.ips" || got[1].Name != "HelloTart-2.ips" {
		t.Fatalf("got %+v", got)
	}
	if got[0].Modified.Unix() != 1700000300 {
		t.Fatalf("modified = %v", got[0].Modified)
	}
}

func TestCrashScriptsAreValidShell(t *testing.T) {
	dir := t.TempDir()
	for name, script := range map[string]string{
		"list-all":    crashListScript(""),
		"list-app":    crashListScript("Hello Tart"),
		"show":        crashShowScript("HelloTart-1.ips"),
		"show-spaces": crashShowScript("Hello Tart_1.crash"),
	} {
		path := filepath.Join(dir, name+".sh")
		if err := os.WriteFile(path, []byte(script), 0o644); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("/bin/sh", "-n", path).CombinedOutput(); err != nil {
			t.Errorf("%s does not parse: %v: %s", name, err, out)
		}
	}
}

func TestCrashHandlersRejectBadRequests(t *testing.T) {
	m := &Manager{
		vms: map[string]*VM{
			"HelloVM": {Name: "HelloVM", State: "running"},
			"Stopped": {Name: "Stopped", State: "stopped"},
		},
		cfg: Config{},
	}
	cases := []struct {
		name, method, path string
		status             int
	}{
		{"list POST", http.MethodPost, "/api/vm/crash-reports?name=HelloVM", http.StatusMethodNotAllowed},
		{"show POST", http.MethodPost, "/api/vm/crash-report?name=HelloVM&file=a.ips", http.StatusMethodNotAllowed},
		{"list bad VM name", http.MethodGet, "/api/vm/crash-reports?name=a/b", http.StatusBadRequest},
		{"list bad app name", http.MethodGet, "/api/vm/crash-reports?name=HelloVM&app=x%27y", http.StatusBadRequest},
		{"list unknown VM", http.MethodGet, "/api/vm/crash-reports?name=Nope", http.StatusNotFound},
		{"list stopped VM", http.MethodGet, "/api/vm/crash-reports?name=Stopped", http.StatusConflict},
		{"show bad file", http.MethodGet, "/api/vm/crash-report?name=HelloVM&file=../x.ips", http.StatusBadRequest},
		{"show unknown VM", http.MethodGet, "/api/vm/crash-report?name=Nope&file=a.ips", http.StatusNotFound},
		{"show stopped VM", http.MethodGet, "/api/vm/crash-report?name=Stopped&file=a.ips", http.StatusConflict},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(c.method, c.path, nil)
			if strings.HasPrefix(c.path, "/api/vm/crash-report?") {
				m.handleCrashReport(rec, req)
			} else {
				m.handleCrashReports(rec, req)
			}
			if rec.Code != c.status {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, c.status, rec.Body.String())
			}
		})
	}
}

// TestCrashReportsEndToEndOnHost lists and reads reports through the real
// scripts, with a fake tart that runs guest commands locally under a temporary
// HOME, so the test never reads this Mac's own crash reports.
func TestCrashReportsEndToEndOnHost(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("needs macOS stat")
	}
	home := t.TempDir()
	reports := filepath.Join(home, "Library", "Logs", "DiagnosticReports")
	if err := os.MkdirAll(reports, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	files := map[string]struct {
		body string
		age  time.Duration
	}{
		"HelloTart-old.ips": {"old report", 2 * time.Hour},
		"HelloTart-new.ips": {"new report", time.Minute},
		"OtherApp-x.crash":  {"other app", time.Minute},
	}
	for name, f := range files {
		path := filepath.Join(reports, name)
		if err := os.WriteFile(path, []byte(f.body), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, now.Add(-f.age), now.Add(-f.age)); err != nil {
			t.Fatal(err)
		}
	}

	fakeTart := filepath.Join(t.TempDir(), "tart")
	// `tart exec <vm> /bin/sh -c <guest script> tart-oven <user> <command>`
	fake := fmt.Sprintf("#!/bin/sh\nshift 2\nexec env HOME=%s \"$1\" \"$2\" \"$3\" \"$4\" \"$5\" \"$6\"\n", shellQuote(home))
	if err := os.WriteFile(fakeTart, []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	m := &Manager{
		vms: map[string]*VM{"HelloVM": {Name: "HelloVM", State: "running"}},
		cfg: Config{TartAppPath: fakeTart, VMStoragePath: t.TempDir()},
	}

	list := func(query string) []string {
		t.Helper()
		rec := httptest.NewRecorder()
		m.handleCrashReports(rec, httptest.NewRequest(http.MethodGet, "/api/vm/crash-reports?"+query, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("list %s: status %d: %s", query, rec.Code, rec.Body.String())
		}
		var reply struct {
			Reports []crashReportEntry `json:"reports"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, r := range reply.Reports {
			names = append(names, r.Name)
		}
		return names
	}

	if got := list("name=HelloVM&app=HelloTart"); len(got) != 2 || got[0] != "HelloTart-new.ips" || got[1] != "HelloTart-old.ips" {
		t.Fatalf("app filter: got %v, want newest first", got)
	}
	if got := list("name=HelloVM"); len(got) != 3 || got[0] == "HelloTart-old.ips" {
		t.Fatalf("no filter: got %v", got)
	}

	show := func(file string) (int, string) {
		t.Helper()
		rec := httptest.NewRecorder()
		m.handleCrashReport(rec, httptest.NewRequest(http.MethodGet, "/api/vm/crash-report?name=HelloVM&file="+file, nil))
		return rec.Code, rec.Body.String()
	}
	if code, body := show("HelloTart-new.ips"); code != http.StatusOK || body != "new report" {
		t.Fatalf("show: %d %q", code, body)
	}
	if code, _ := show("HelloTart-missing.ips"); code != http.StatusNotFound {
		t.Fatalf("missing report: status %d, want 404", code)
	}
}

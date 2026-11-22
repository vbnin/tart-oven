package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func newCrashTestCLI(t *testing.T, handler http.HandlerFunc) *cli {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &cli{statePath: filepath.Join(t.TempDir(), "state.json"), url: srv.URL, client: srv.Client()}
}

func TestCrashReportsPrintsNewestFirst(t *testing.T) {
	c := newCrashTestCLI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/vm/crash-reports" || r.URL.Query().Get("name") != "HelloVM" || r.URL.Query().Get("app") != "HelloTart" {
			t.Errorf("unexpected request %s", r.URL.String())
		}
		w.Write([]byte(`{"ok":true,"vm":"HelloVM","app":"HelloTart","reports":[` +
			`{"name":"HelloTart-2.ips","modified":"2026-11-22T11:30:00Z"},` +
			`{"name":"HelloTart-1.ips","modified":"2026-11-22T11:00:00Z"}]}`))
	})
	var out bytes.Buffer
	if err := c.runCrashReports([]string{"HelloVM", "HelloTart"}, &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || !strings.HasSuffix(lines[0], "HelloTart-2.ips") || !strings.HasSuffix(lines[1], "HelloTart-1.ips") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestCrashReportsSaysWhenThereAreNone(t *testing.T) {
	c := newCrashTestCLI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true,"vm":"HelloVM","reports":[]}`))
	})
	var out bytes.Buffer
	if err := c.runCrashReports([]string{"HelloVM"}, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "no crash reports\n" {
		t.Fatalf("output = %q", out.String())
	}
}

func TestCrashReportPrintsTheReportText(t *testing.T) {
	c := newCrashTestCLI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/vm/crash-report" || r.URL.Query().Get("file") != "HelloTart-1.ips" {
			t.Errorf("unexpected request %s", r.URL.String())
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("Exception Type: EXC_BAD_ACCESS\n"))
	})
	var out bytes.Buffer
	if err := c.runCrashReport([]string{"HelloVM", "HelloTart-1.ips"}, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "Exception Type: EXC_BAD_ACCESS\n" {
		t.Fatalf("output = %q", out.String())
	}
}

func TestCrashReportShowsTheServerError(t *testing.T) {
	c := newCrashTestCLI(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"ok":false,"error":"no crash report named HelloTart-9.ips"}`))
	})
	var out bytes.Buffer
	err := c.runCrashReport([]string{"HelloVM", "HelloTart-9.ips"}, &out)
	if err == nil || err.Error() != "no crash report named HelloTart-9.ips" {
		t.Fatalf("err = %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("nothing should be printed on failure, got %q", out.String())
	}
}

func TestCrashCommandsCheckTheirArguments(t *testing.T) {
	c := &cli{}
	if err := c.runCrashReports(nil, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "usage: tart-oven crash-reports") {
		t.Errorf("crash-reports with no VM: err = %v", err)
	}
	if err := c.runCrashReport([]string{"HelloVM"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "usage: tart-oven crash-report") {
		t.Errorf("crash-report without a file: err = %v", err)
	}
}

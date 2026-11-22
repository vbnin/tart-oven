package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunAppPostsTheAbsolutePathAndPrintsTheLog(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/vm/run-app" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("body is not JSON: %v", err)
		}
		if body["name"] != "HelloVM" || !filepath.IsAbs(body["appPath"]) || !strings.HasSuffix(body["appPath"], "My.app") {
			t.Errorf("body = %v", body)
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "pid": 42, "running": true, "log": "hello\n"})
	}))
	defer srv.Close()

	c := &cli{statePath: filepath.Join(t.TempDir(), "state.json"), url: srv.URL, client: srv.Client()}
	var out bytes.Buffer
	if err := c.runApp([]string{"HelloVM", "My.app"}, &out); err != nil {
		t.Fatal(err)
	}
	want := "started My.app on HelloVM (pid 42)\nhello\n"
	if out.String() != want {
		t.Fatalf("output = %q, want %q", out.String(), want)
	}
}

func TestRunAppReportsTheServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "VM HelloVM is not running; start it first"})
	}))
	defer srv.Close()

	c := &cli{statePath: filepath.Join(t.TempDir(), "state.json"), url: srv.URL, client: srv.Client()}
	var out bytes.Buffer
	err := c.runApp([]string{"HelloVM", "/tmp/My.app"}, &out)
	if err == nil || err.Error() != "VM HelloVM is not running; start it first" {
		t.Fatalf("err = %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("nothing should be printed on failure, got %q", out.String())
	}
}

func TestRunAppNeedsExactlyTwoArguments(t *testing.T) {
	c := &cli{}
	for _, args := range [][]string{nil, {"HelloVM"}, {"HelloVM", "a.app", "extra"}} {
		if err := c.runApp(args, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "usage: tart-oven run-app") {
			t.Errorf("args %v: err = %v", args, err)
		}
	}
}

package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"tart-oven/internal/auth"
	"testing"
)

func TestClientAddr(t *testing.T) {
	for in, want := range map[string]string{
		"127.0.0.1:9000": "127.0.0.1:9000",
		":9000":          "127.0.0.1:9000",
		"0.0.0.0:8080":   "127.0.0.1:8080",
		"[::]:9000":      "127.0.0.1:9000",
		"192.168.1.5:90": "192.168.1.5:90",
		"garbage":        defaultAddr,
	} {
		if got := clientAddr(in); got != want {
			t.Errorf("clientAddr(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestListenAddrReadsStateFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if got := listenAddr(path); got != defaultAddr {
		t.Errorf("missing file = %q", got)
	}
	os.WriteFile(path, []byte(`{"config":{"listen":"0.0.0.0:9100"}}`), 0o600)
	if got := listenAddr(path); got != "0.0.0.0:9100" {
		t.Errorf("listen = %q", got)
	}
}

func TestStatusAndVersionAgainstAServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/vms" {
			w.Write([]byte(`{"version":"9.9-dev1","vms":[]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c := &cli{url: srv.URL, client: srv.Client()}
	if got := c.version(); got != "9.9-dev1" {
		t.Fatalf("version = %q", got)
	}
	if code := c.status(); code != 0 {
		t.Fatalf("status = %d while running", code)
	}
	srv.Close()
	if c.running() {
		t.Fatal("running after the server closed")
	}
}

func TestUnknownCommandShowsUsage(t *testing.T) {
	if code := runCommand("frobnicate", filepath.Join(t.TempDir(), "state.json")); code != 2 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(usage, "open") || !strings.Contains(usage, "status") {
		t.Fatal("usage is missing commands")
	}
}

func TestVersionFallsBackToVMsForOlderServers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/vms" {
			w.Write([]byte(`{"version":"1.54"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c := &cli{url: srv.URL, client: srv.Client()}
	if got := c.version(); got != "1.54" {
		t.Fatalf("version = %q", got)
	}
}

func TestTokenCommandsManageAuthFile(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	c := newCLI(statePath)
	if hash, _ := auth.Load(filepath.Dir(statePath)); hash != "" {
		t.Fatal("token set before generate")
	}
	if err := c.token([]string{"generate"}); err != nil {
		t.Fatal(err)
	}
	first, _ := auth.Load(filepath.Dir(statePath))
	if first == "" {
		t.Fatal("generate stored nothing")
	}
	if err := c.token([]string{"rotate"}); err != nil {
		t.Fatal(err)
	}
	if second, _ := auth.Load(filepath.Dir(statePath)); second == "" || second == first {
		t.Fatal("rotate did not replace the hash")
	}
	if err := c.token([]string{"revoke"}); err != nil {
		t.Fatal(err)
	}
	if hash, _ := auth.Load(filepath.Dir(statePath)); hash != "" {
		t.Fatal("revoke left the token")
	}
	if err := c.token([]string{"bogus"}); err == nil {
		t.Fatal("unknown token command accepted")
	}
}

func TestHTTPSAndControlKey(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	os.WriteFile(statePath, []byte(`{"config":{"listen":"127.0.0.1:9443","tlsEnabled":true}}`), 0o600)
	if c := newCLI(statePath); !strings.HasPrefix(c.url, "https://127.0.0.1:9443") {
		t.Fatalf("url = %q", c.url)
	}

	key, _ := auth.NewControlKey(dir)
	var got string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Tart-Oven-Control")
	}))
	defer srv.Close()
	c := newCLI(statePath)
	c.url = srv.URL // self-signed httptest cert: must be accepted
	if err := c.post("/api/server/stop"); err != nil {
		t.Fatal(err)
	}
	if got != key {
		t.Fatalf("control header = %q", got)
	}
}

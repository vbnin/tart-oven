package server

import (
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tart-oven/internal/auth"
)

func newAuthTestManager(t *testing.T) (*Manager, http.Handler) {
	t.Helper()
	m := &Manager{
		vms:       map[string]*VM{},
		busy:      map[string]bool{},
		statePath: filepath.Join(t.TempDir(), "state.json"),
		cfg:       Config{Listen: "127.0.0.1:9000"},
	}
	return m, m.handler()
}

func do(h http.Handler, method, path, body string, hdr map[string]string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = "192.0.2.10:5555"
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func sessionOf(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	return nil
}

func TestAuthOffByDefault(t *testing.T) {
	_, h := newAuthTestManager(t)
	if rec := do(h, "GET", "/api/config", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("config without a token set = %d", rec.Code)
	}
}

func TestTokenLifecycle(t *testing.T) {
	m, h := newAuthTestManager(t)

	rec := do(h, "POST", "/api/auth/token", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("generate = %d %s", rec.Code, rec.Body)
	}
	var out struct{ Token string }
	json.Unmarshal(rec.Body.Bytes(), &out)
	if !strings.HasPrefix(out.Token, "to_") {
		t.Fatalf("token = %q", out.Token)
	}
	creator := sessionOf(rec)
	if creator == nil || !creator.HttpOnly || creator.SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie = %+v", creator)
	}
	raw, _ := os.ReadFile(filepath.Join(filepath.Dir(m.statePath), auth.FileName))
	if strings.Contains(string(raw), out.Token) {
		t.Fatal("plaintext token on disk")
	}

	// Locked down now, except for the exempt routes.
	for _, p := range []string{"/api/config", "/api/vms", "/events", "/CHANGELOG.md", "/api/tls"} {
		if rec := do(h, "GET", p, "", nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s unauthenticated = %d", p, rec.Code)
		}
	}
	if rec := do(h, "POST", "/api/server/stop", "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("stop unauthenticated = %d", rec.Code)
	}
	for _, p := range []string{"/", "/api/health", "/api/auth/status"} {
		if rec := do(h, "GET", p, "", nil); rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want exempt", p, rec.Code)
		}
	}

	// Bearer, cookie, and the creator's own session all work.
	if rec := do(h, "GET", "/api/config", "", map[string]string{"Authorization": "Bearer " + out.Token}); rec.Code != 200 {
		t.Errorf("bearer = %d", rec.Code)
	}
	if rec := do(h, "GET", "/api/config", "", map[string]string{"Authorization": "Bearer nope"}); rec.Code != 401 {
		t.Errorf("wrong bearer = %d", rec.Code)
	}
	if rec := do(h, "GET", "/api/config", "", nil, creator); rec.Code != 200 {
		t.Errorf("creator session = %d", rec.Code)
	}

	// Login with the token yields a second session.
	bad := do(h, "POST", "/api/auth/login", `{"token":"wrong"}`, nil)
	if bad.Code != 401 || sessionOf(bad) != nil {
		t.Fatalf("bad login = %d", bad.Code)
	}
	good := do(h, "POST", "/api/auth/login", `{"token":"`+out.Token+`"}`, nil)
	login := sessionOf(good)
	if good.Code != 200 || login == nil {
		t.Fatalf("login = %d", good.Code)
	}

	// Rotation invalidates everything, including the old token and sessions.
	rot := do(h, "POST", "/api/auth/token", "", map[string]string{"Authorization": "Bearer " + out.Token})
	var next struct{ Token string }
	json.Unmarshal(rot.Body.Bytes(), &next)
	if next.Token == "" || next.Token == out.Token {
		t.Fatalf("rotate token = %q", next.Token)
	}
	if rec := do(h, "GET", "/api/config", "", map[string]string{"Authorization": "Bearer " + out.Token}); rec.Code != 401 {
		t.Errorf("old token after rotate = %d", rec.Code)
	}
	if rec := do(h, "GET", "/api/config", "", nil, login); rec.Code != 401 {
		t.Errorf("old session after rotate = %d", rec.Code)
	}
	if rec := do(h, "GET", "/api/config", "", nil, sessionOf(rot)); rec.Code != 200 {
		t.Errorf("rotating session dropped = %d", rec.Code)
	}

	// Logout ends the session.
	s := sessionOf(rot)
	do(h, "POST", "/api/auth/logout", "", nil, s)
	if rec := do(h, "GET", "/api/config", "", nil, s); rec.Code != 401 {
		t.Errorf("after logout = %d", rec.Code)
	}

	// Revoke turns auth off again.
	if rec := do(h, "POST", "/api/auth/revoke", "", map[string]string{"Authorization": "Bearer " + next.Token}); rec.Code != 200 {
		t.Fatalf("revoke = %d", rec.Code)
	}
	if rec := do(h, "GET", "/api/config", "", nil); rec.Code != 200 {
		t.Errorf("after revoke = %d", rec.Code)
	}
}

func TestCLIReplacesTokenBehindTheServersBack(t *testing.T) {
	m, h := newAuthTestManager(t)
	dir := filepath.Dir(m.statePath)
	oldTok, _ := auth.NewToken()
	auth.Save(dir, auth.Hash(oldTok))
	hdr := map[string]string{"Authorization": "Bearer " + oldTok}
	if rec := do(h, "GET", "/api/config", "", hdr); rec.Code != 200 {
		t.Fatalf("initial = %d", rec.Code)
	}
	newTok, _ := auth.NewToken()
	auth.Save(dir, auth.Hash(newTok))
	future := os.Chtimes // force a distinct mtime on coarse filesystems
	future(filepath.Join(dir, auth.FileName), timeNowPlus(2), timeNowPlus(2))
	if rec := do(h, "GET", "/api/config", "", hdr); rec.Code != 401 {
		t.Errorf("old token after CLI rotate = %d", rec.Code)
	}
	if rec := do(h, "GET", "/api/config", "", map[string]string{"Authorization": "Bearer " + newTok}); rec.Code != 200 {
		t.Errorf("new token = %d", rec.Code)
	}
	auth.Remove(dir)
	if rec := do(h, "GET", "/api/config", "", nil); rec.Code != 200 {
		t.Errorf("after CLI revoke = %d", rec.Code)
	}
}

func TestDamagedAuthFileFailsClosed(t *testing.T) {
	m, h := newAuthTestManager(t)
	os.WriteFile(filepath.Join(filepath.Dir(m.statePath), auth.FileName), []byte("garbage"), 0o600)
	if rec := do(h, "GET", "/api/config", "", nil); rec.Code != 401 {
		t.Fatalf("damaged auth file = %d, want 401", rec.Code)
	}
}

func TestLoginLockout(t *testing.T) {
	m, h := newAuthTestManager(t)
	tok, _ := auth.NewToken()
	m.auth().set(auth.Hash(tok))
	for i := 0; i < loginMaxFails; i++ {
		if rec := do(h, "POST", "/api/auth/login", `{"token":"x"}`, nil); rec.Code != 401 {
			t.Fatalf("attempt %d = %d", i, rec.Code)
		}
	}
	if rec := do(h, "POST", "/api/auth/login", `{"token":"`+tok+`"}`, nil); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("after lockout, correct token = %d", rec.Code)
	}
}

func TestControlKeyOnlyFromLoopback(t *testing.T) {
	m, h := newAuthTestManager(t)
	tok, _ := auth.NewToken()
	m.auth().set(auth.Hash(tok))
	m.auth().controlKey = "secret"
	hdr := map[string]string{controlHeader: "secret"}
	if rec := do(h, "GET", "/api/config", "", hdr); rec.Code != 401 {
		t.Errorf("remote control key = %d", rec.Code)
	}
	req := httptest.NewRequest("GET", "/api/config", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set(controlHeader, "secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("loopback control key = %d", rec.Code)
	}
	req.Header.Set(controlHeader, "wrong")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("wrong control key = %d", rec.Code)
	}
}

func TestSessionCookieSecureOverTLS(t *testing.T) {
	_, h := newAuthTestManager(t)
	req := httptest.NewRequest("POST", "/api/auth/token", nil)
	req.TLS = &tls.ConnectionState{}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if c := sessionOf(rec); c == nil || !c.Secure {
		t.Fatalf("cookie over TLS = %+v", c)
	}
}

func TestConfigNeverExposesTokenHash(t *testing.T) {
	m, h := newAuthTestManager(t)
	tok, _ := auth.NewToken()
	m.auth().set(auth.Hash(tok))
	rec := do(h, "GET", "/api/config", "", map[string]string{"Authorization": "Bearer " + tok})
	if strings.Contains(rec.Body.String(), auth.Hash(tok)) || strings.Contains(strings.ToLower(rec.Body.String()), "tokenhash") {
		t.Fatal("config leaks the token hash")
	}
}

func TestListenExposed(t *testing.T) {
	for in, want := range map[string]bool{
		"127.0.0.1:9000": false, "localhost:9000": false, "[::1]:9000": false,
		"0.0.0.0:9000": true, ":9000": true, "192.168.1.5:9000": true, "garbage": true,
	} {
		if got := listenExposed(in); got != want {
			t.Errorf("listenExposed(%q) = %v", in, got)
		}
	}
}

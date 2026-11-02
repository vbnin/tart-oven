package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	tartoven "tart-oven"
	"tart-oven/internal/auth"
)

// ---------------------------------------------------------------------------
// UI access token
//
// Auth is off until a token is generated. Only the token's hash is stored, in
// auth.json beside state.json, which the CLI can also replace or delete, so a
// lost token is recoverable without the UI. Browsers trade the token for an
// HttpOnly session cookie (EventSource cannot send headers); scripts send it
// as a Bearer token.
// ---------------------------------------------------------------------------

const (
	sessionCookie = "tart_oven_session"
	sessionTTL    = 7 * 24 * time.Hour
	maxSessions   = 100
	loginMaxFails = 5
	loginLockout  = time.Minute
	controlHeader = "X-Tart-Oven-Control"
)

type loginFail struct {
	count int
	until time.Time
}

type authState struct {
	dir        string
	controlKey string

	mu       sync.Mutex
	hash     string
	mtime    time.Time
	loaded   bool
	sessions map[string]time.Time
	fails    map[string]*loginFail

	agents      []auth.AgentToken // agent-scope tokens, reloaded when the file changes
	agentMtime  time.Time
	agentLoaded bool
}

// principal is who a request is acting as. Agent principals may only reach
// /api/agent/*; name is the agent token's label (the lease owner).
type principal struct {
	agent bool
	name  string
}

type principalKey struct{}

// principalOf returns the caller recorded by authGuard. Requests that never
// passed the guard (auth off, or a handler called directly) act as "local".
func principalOf(r *http.Request) principal {
	if p, ok := r.Context().Value(principalKey{}).(principal); ok {
		return p
	}
	return principal{name: "local"}
}

func newAuthState(dir string) *authState {
	return &authState{dir: dir, sessions: map[string]time.Time{}, fails: map[string]*loginFail{}}
}

// authSt returns the Manager's auth state, created on first use.
func (m *Manager) auth() *authState {
	m.authOnce.Do(func() {
		if m.authSt == nil {
			m.authSt = newAuthState(filepath.Dir(m.statePath))
		}
	})
	return m.authSt
}

// current returns the active token hash ("" = auth disabled). It re-reads
// auth.json when its mtime changes, so `tart-oven token rotate` takes effect
// at once; a changed hash drops every browser session.
func (a *authState) current() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.refreshLocked()
}

func (a *authState) refreshLocked() string {
	fi, err := os.Stat(filepath.Join(a.dir, auth.FileName))
	hash := ""
	if err == nil {
		if a.loaded && fi.ModTime().Equal(a.mtime) {
			return a.hash
		}
		loaded, lerr := auth.Load(a.dir)
		if lerr != nil {
			loaded = auth.Locked // damaged file: fail closed
		}
		hash, a.mtime = loaded, fi.ModTime()
	}
	a.loaded = true
	if hash != a.hash {
		a.hash = hash
		a.sessions = map[string]time.Time{}
	}
	return a.hash
}

// set stores a new hash (or disables auth when hash is "") and ends every session.
func (a *authState) set(hash string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	var err error
	if hash == "" {
		err = auth.Remove(a.dir)
		if err == nil {
			// Agent tokens only mean something next to a dashboard token;
			// don't let them quietly come back when one is generated later.
			err = auth.RemoveAgentTokens(a.dir)
		}
	} else {
		err = auth.Save(a.dir, hash)
	}
	if err != nil {
		return err
	}
	a.loaded = false
	a.hash = ""
	a.agentLoaded = false
	a.sessions = map[string]time.Time{}
	a.refreshLocked()
	return nil
}

// agentTokens returns the current agent tokens, re-reading the file when its
// mtime changes so `tart-oven token agent ...` takes effect at once. A damaged
// file yields no tokens (fail closed).
func (a *authState) agentTokens() []auth.AgentToken {
	a.mu.Lock()
	defer a.mu.Unlock()
	fi, err := os.Stat(filepath.Join(a.dir, auth.AgentFileName))
	if err != nil {
		a.agents, a.agentLoaded = nil, true
		return nil
	}
	if a.agentLoaded && fi.ModTime().Equal(a.agentMtime) {
		return a.agents
	}
	tokens, lerr := auth.LoadAgentTokens(a.dir)
	if lerr != nil {
		tokens = nil
	}
	a.agents, a.agentMtime, a.agentLoaded = tokens, fi.ModTime(), true
	return a.agents
}

// invalidateAgentTokens forces the next agentTokens call to re-read the file.
func (a *authState) invalidateAgentTokens() {
	a.mu.Lock()
	a.agentLoaded = false
	a.mu.Unlock()
}

func (a *authState) newSession() (string, error) {
	id, err := auth.NewSessionID()
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	for k, exp := range a.sessions {
		if now.After(exp) {
			delete(a.sessions, k)
		}
	}
	for len(a.sessions) >= maxSessions {
		var oldest string
		for k, exp := range a.sessions {
			if oldest == "" || exp.Before(a.sessions[oldest]) {
				oldest = k
			}
		}
		delete(a.sessions, oldest)
	}
	a.sessions[id] = now.Add(sessionTTL)
	return id, nil
}

func (a *authState) validSession(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.sessions[id]
	if ok && time.Now().After(exp) {
		delete(a.sessions, id)
		return false
	}
	return ok
}

func (a *authState) endSession(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.sessions, id)
}

// locked reports whether ip is in a login lockout and when it ends.
func (a *authState) locked(ip string) (time.Duration, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if f := a.fails[ip]; f != nil && time.Now().Before(f.until) {
		return time.Until(f.until), true
	}
	return 0, false
}

func (a *authState) recordLogin(ip string, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if ok {
		delete(a.fails, ip)
		return
	}
	f := a.fails[ip]
	if f == nil {
		f = &loginFail{}
		a.fails[ip] = f
	}
	if f.count++; f.count >= loginMaxFails {
		f.count = 0
		f.until = time.Now().Add(loginLockout)
	}
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func isLoopbackRequest(r *http.Request) bool {
	ip := net.ParseIP(remoteIP(r))
	return ip != nil && ip.IsLoopback()
}

// identify reports who the request is: the local CLI's control key, a Bearer
// token (dashboard or agent scope), or a live session cookie.
func (a *authState) identify(r *http.Request, hash string) (principal, bool) {
	if key := r.Header.Get(controlHeader); key != "" && a.controlKey != "" && isLoopbackRequest(r) &&
		subtle.ConstantTimeCompare([]byte(key), []byte(a.controlKey)) == 1 {
		return principal{name: "admin"}, true
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		token := strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
		if auth.Match(hash, token) {
			return principal{name: "admin"}, true
		}
		if t, ok := auth.MatchAgent(a.agentTokens(), token); ok {
			return principal{agent: true, name: t.Name}, true
		}
	}
	if c, err := r.Cookie(sessionCookie); err == nil && a.validSession(c.Value) {
		return principal{name: "admin"}, true
	}
	return principal{}, false
}

// authenticated reports whether the request carries a full-access credential.
// An agent token is not one: it opens only /api/agent/*.
func (a *authState) authenticated(r *http.Request, hash string) bool {
	p, ok := a.identify(r, hash)
	return ok && !p.agent
}

// authExempt lists what works without credentials: the page shell (which
// renders the login form), its icon, and the probes the login flow and the
// CLI need.
func authExempt(r *http.Request) bool {
	switch r.URL.Path {
	case "/", "/icon.png", "/api/health", "/api/auth/status", "/api/auth/login":
		return true
	}
	return false
}

// agentPath reports whether path belongs to the agent API, the only part of
// the server an agent-scope token may use (plus the exempt health probe).
func agentPath(path string) bool {
	return strings.HasPrefix(path, "/api/agent/") || path == "/api/agent"
}

// authGuard enforces the token on every non-exempt request once one is set.
func (m *Manager) authGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		a := m.auth()
		hash := a.current()
		if hash == "" || authExempt(r) {
			next.ServeHTTP(w, r)
			return
		}
		if p, ok := a.identify(r, hash); ok {
			if p.agent && !agentPath(r.URL.Path) {
				w.Header().Set("Cache-Control", "no-store")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				w.Write([]byte(`{"error":"forbidden: an agent token can only use /api/agent/*"}` + "\n"))
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"unauthorized"}` + "\n"))
			return
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

func (m *Manager) setSessionCookie(w http.ResponseWriter, r *http.Request, id string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    id,
		Path:     "/",
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
	})
}

func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
}

// listenExposed reports whether the listen address accepts non-local clients.
func listenExposed(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return true
	}
	if host == "" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return !ip.IsLoopback()
	}
	return host != "localhost"
}

func (m *Manager) registerAuthRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]string{"version": tartoven.Version})
	})

	mux.HandleFunc("/api/auth/status", func(w http.ResponseWriter, r *http.Request) {
		a := m.auth()
		hash := a.current()
		m.mu.Lock()
		exposed := listenExposed(m.cfg.Listen)
		m.mu.Unlock()
		writeJSON(w, map[string]any{
			"enabled":       hash != "",
			"authenticated": hash == "" || a.authenticated(r, hash),
			"exposed":       exposed,
			"tls":           m.tlsActive || r.TLS != nil,
		})
	})

	mux.HandleFunc("/api/auth/login", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		a := m.auth()
		ip := remoteIP(r)
		if wait, locked := a.locked(ip); locked {
			w.Header().Set("Retry-After", time.Duration(wait+time.Second).Truncate(time.Second).String())
			http.Error(w, "too many failed attempts; try again later", http.StatusTooManyRequests)
			return
		}
		var body struct {
			Token string `json:"token"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		hash := a.current()
		if hash == "" {
			writeJSON(w, map[string]bool{"ok": true}) // nothing to log in to
			return
		}
		ok := auth.Match(hash, strings.TrimSpace(body.Token))
		a.recordLogin(ip, ok)
		if !ok {
			m.logln("rejected UI login from %s", ip)
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}
		id, err := a.newSession()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		m.setSessionCookie(w, r, id)
		writeJSON(w, map[string]bool{"ok": true})
	})

	mux.HandleFunc("/api/auth/logout", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if c, err := r.Cookie(sessionCookie); err == nil {
			m.auth().endSession(c.Value)
		}
		clearSessionCookie(w)
		writeJSON(w, map[string]bool{"ok": true})
	})

	// Generate (first time) or rotate. The plaintext is returned once and
	// never stored. Every other session ends; the caller stays signed in.
	mux.HandleFunc("/api/auth/token", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		a := m.auth()
		token, err := auth.NewToken()
		if err == nil {
			err = a.set(auth.Hash(token))
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		m.logln("UI access token generated")
		if id, err := a.newSession(); err == nil {
			m.setSessionCookie(w, r, id)
		}
		writeJSON(w, map[string]string{"token": token})
	})

	mux.HandleFunc("/api/auth/revoke", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := m.auth().set(""); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		m.logln("UI access token removed")
		clearSessionCookie(w)
		writeJSON(w, map[string]bool{"ok": true})
	})

	// Agent-scope tokens (full-access callers only: the guard keeps agent
	// tokens out of everything but /api/agent/*).
	mux.HandleFunc("GET /api/auth/agent-tokens", func(w http.ResponseWriter, r *http.Request) {
		type view struct {
			ID        string    `json:"id"`
			Name      string    `json:"name"`
			CreatedAt time.Time `json:"createdAt"`
		}
		a := m.auth()
		tokens := make([]view, 0)
		for _, t := range a.agentTokens() {
			tokens = append(tokens, view{t.ID, t.Name, t.CreatedAt})
		}
		writeJSON(w, map[string]any{"tokens": tokens, "authEnabled": a.current() != ""})
	})
	mux.HandleFunc("POST /api/auth/agent-tokens", func(w http.ResponseWriter, r *http.Request) {
		a := m.auth()
		if a.current() == "" {
			http.Error(w, "generate the dashboard access token first: agent tokens only limit access when a login is required", http.StatusConflict)
			return
		}
		var body struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		token, err := auth.AddAgentToken(a.dir, body.Name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		a.invalidateAgentTokens()
		m.logln("agent token %q created", strings.TrimSpace(body.Name))
		writeJSON(w, map[string]string{"token": token, "name": strings.TrimSpace(body.Name)})
	})
	mux.HandleFunc("DELETE /api/auth/agent-tokens/{id}", func(w http.ResponseWriter, r *http.Request) {
		a := m.auth()
		if err := auth.RevokeAgentToken(a.dir, r.PathValue("id")); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		a.invalidateAgentTokens()
		m.logln("agent token %q revoked", r.PathValue("id"))
		writeJSON(w, map[string]bool{"ok": true})
	})
}

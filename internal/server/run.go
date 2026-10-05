// Package server is tart-oven's core: a small fleet manager for macOS VMs
// running under Tart.
//
// One binary, few dependencies. It runs `tart` commands directly (as the
// logged-in console user via a LaunchAgent, so no sudo / user-switching), keeps
// all state in a single Manager struct guarded by a mutex, persists to a small
// JSON file, drives automatic VM runs from a scheduler goroutine, and serves a
// live dashboard over HTTP + Server-Sent Events.
//
// Build:   go build -o tart-oven ./cmd/tart-oven
// Run:     ./tart-oven            (reads/writes ~/.tart-oven/state.json)
package server

import (
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os/exec"
	"time"

	tartoven "tart-oven"
	"tart-oven/internal/auth"
	"tart-oven/internal/mdm"
	"tart-oven/internal/perf"
	"tart-oven/internal/sshkey"
)

// ---------------------------------------------------------------------------
// Run
// ---------------------------------------------------------------------------

// Run starts the tart-oven server.
func Run(statePath, listenOverride string) error {
	m := &Manager{
		vms:                  map[string]*VM{},
		busy:                 map[string]bool{},
		opStart:              map[string]time.Time{},
		runningCmds:          map[string]*exec.Cmd{},
		agentProbedSinceBoot: map[string]bool{},
		subs:                 map[chan []byte]struct{}{},
		statePath:            statePath,
		reload:               make(chan struct{}, 1),
		performanceCollector: perf.NewCollector(),
	}
	m.mdmCopier = mdm.NewSFTPProfileCopier()
	m.mdmResolveIP = m.resolveMDMIPWithTart
	m.load()
	if listenOverride != "" {
		m.cfg.Listen = listenOverride
	}

	// Set up file logging (after config is loaded so we have LogPath).
	setupLogging(m.cfg.LogPath)

	// Reconcile reality at startup: detect JSON support, check storage, sync.
	m.detectTartJSON()
	m.detectProvisioningSupport()
	m.updateTartVersion()
	m.checkStorage()
	m.ensureSharedDir()
	m.stageGuestAgentInstaller()
	m.reconcile()
	if n := m.closeStaleHistory(); n > 0 {
		m.mu.Lock()
		m.save()
		m.mu.Unlock()
		log.Printf("history: closed %d run(s) whose stop was never recorded", n)
	}
	m.updatePerformance(time.Now())
	m.hostIP = localIP()

	go m.schedulerLoop()
	go m.updateCheckLoop()

	// Background monitor: keep storage status, VM states and timers fresh even
	// between scheduler ticks. It also heals ops that got stuck "busy" and
	// reconciles against tart, so a VM that tart stopped on its own (or one
	// wedged in "stopping") self-corrects within ~10s.
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for range t.C {
			m.checkStorage()
			m.healStuck(maxOpAge)
			m.reconcile()
			m.broadcast()
		}
	}()

	// Refresh native host performance metrics once a minute.
	go func() {
		t := time.NewTicker(60 * time.Second)
		defer t.Stop()
		for range t.C {
			m.updatePerformance(time.Now())
			m.updateTartVersion()
			m.hostIP = localIP()
			m.broadcast()
		}
	}()

	// The SSH fallback (used only for guests without the Tart guest agent) needs a
	// usable identity. Nothing is ever pushed into a guest automatically.
	if _, err := sshkey.EnsureKeyPair(m.cfg.SSHKey); err != nil {
		log.Printf("ssh identity unavailable, SSH fallback disabled: %v", err)
	}

	log.Printf("tart-oven %s", tartoven.Version)
	log.Printf("host IP: %s", m.hostIP)
	log.Printf("tart binary: %s", m.cfg.TartAppPath)
	log.Printf("tart JSON list support: %v", m.tartJSON)
	log.Printf("state file: %s", m.statePath)
	log.Printf("TART_HOME: %s (mounted=%v)", m.cfg.VMStoragePath, m.storageMounted)

	a := m.auth()
	if key, err := auth.NewControlKey(a.dir); err != nil {
		log.Printf("control key unavailable, the CLI cannot stop or restart a token-protected server: %v", err)
	} else {
		a.controlKey = key
	}
	tlsConf, err := m.startupTLS()
	if err != nil {
		return fmt.Errorf("HTTPS is enabled but cannot start: %w", err)
	}
	m.tlsActive = tlsConf != nil
	scheme := "http"
	if m.tlsActive {
		scheme = "https"
	}
	if a.current() == "" && listenExposed(m.cfg.Listen) {
		log.Printf("WARNING: listening on %s with no access token; anyone who can reach it controls this Mac. Set one in Settings or run `tart-oven token generate`.", m.cfg.Listen)
	} else if listenExposed(m.cfg.Listen) && !m.tlsActive {
		log.Printf("WARNING: listening on %s without HTTPS; the access token travels unencrypted", m.cfg.Listen)
	}
	log.Printf("listening on %s://%s", scheme, m.cfg.Listen)

	srv := &http.Server{
		Addr:              m.cfg.Listen,
		Handler:           m.handler(),
		TLSConfig:         tlsConf,
		ReadHeaderTimeout: 15 * time.Second,
	}
	if m.tlsActive {
		return srv.ListenAndServeTLS("", "")
	}
	return srv.ListenAndServe()
}

// handler is the full request chain: CSRF guard, then the access token, then routes.
func (m *Manager) handler() http.Handler {
	return sameOriginGuard(m.authGuard(m.routes()))
}

// sameOriginGuard rejects state-changing requests that carry a cross-origin
// Origin header. Browsers attach Origin to every non-GET cross-origin request
// (simple requests included), so this blocks a malicious page the operator has
// open from driving the local API via forged POSTs (CSRF). Requests with no
// Origin header (curl, same-origin navigations) are left untouched.
func sameOriginGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			// Safe methods: no state change to protect.
		default:
			if origin := r.Header.Get("Origin"); origin != "" {
				if u, err := url.Parse(origin); err != nil || u.Host != r.Host {
					http.Error(w, "cross-origin request forbidden", http.StatusForbidden)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

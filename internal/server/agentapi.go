package server

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	tartoven "tart-oven"
)

// ---------------------------------------------------------------------------
// Agent API: /api/agent/*
//
// A narrow, opt-in surface for AI agents: lease a VM cloned from an approved
// template, wait until it is ready, run commands in it, extend or destroy it.
// Unlike the dashboard's endpoints it returns real HTTP statuses and
// {"error":{"code","message"}} bodies, and an agent-scope token can reach
// nothing else (see authGuard). All of it is off until Configuration enables it.
// ---------------------------------------------------------------------------

//go:embed agentguide.md
var agentGuide []byte

//go:embed openapi.json
var agentOpenAPI []byte

const (
	defaultAgentExecSec = 120
	maxAgentExecSec     = 30 * 60
	maxAgentCommandLen  = 64 * 1024
	agentOutputLimit    = 1 << 20 // per stream
	maxAgentEnvVars     = 50
	defaultAgentWaitSec = 300
	maxAgentWaitSec     = 900
)

func writeAgentError(w http.ResponseWriter, e *agentError) {
	body := map[string]any{"error": map[string]string{"code": e.Code, "message": e.Message}}
	for k, v := range e.Extra {
		body[k] = v
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.Status)
	json.NewEncoder(w).Encode(body)
}

// decodeAgentBody reads a JSON request body strictly, so a misspelled field is
// reported instead of silently ignored.
func decodeAgentBody(w http.ResponseWriter, r *http.Request, v any) *agentError {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return agentErr(http.StatusBadRequest, "invalid_request", "bad JSON body: %v", err)
	}
	return nil
}

// agentHandler gates a handler on the feature switch and hands it the caller.
func (m *Manager) agentHandler(h func(http.ResponseWriter, *http.Request, principal)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		on := m.cfg.AgentEnabled
		m.mu.Unlock()
		if !on {
			writeAgentError(w, agentErr(http.StatusForbidden, "agent_api_disabled",
				"the agent API is off; enable it in Configuration > Agentic AI access (shown by Server Settings > Display agentic AI features)"))
			return
		}
		h(w, r, principalOf(r))
	}
}

// agentVisible reports whether p may see or act on a lease owned by owner.
// Agent tokens only reach their own; full-access callers reach all.
func agentVisible(p principal, owner string) bool { return !p.agent || owner == p.name }

type agentSharedView struct {
	Host  string `json:"host"`
	Guest string `json:"guest"`
}

type agentVMView struct {
	Name             string           `json:"name"`
	Phase            string           `json:"phase"` // cloning | booting | ready | failed | stopped
	Ready            bool             `json:"ready"`
	State            string           `json:"state,omitempty"`
	IP               string           `json:"ip,omitempty"`
	GuestAgentOK     bool             `json:"guestAgentOk"`
	SSHOK            bool             `json:"sshOk"`
	Owner            string           `json:"owner"`
	Template         string           `json:"template"`
	CreatedAt        *time.Time       `json:"createdAt,omitempty"`
	ExpiresAt        *time.Time       `json:"expiresAt,omitempty"`
	SecondsRemaining int              `json:"secondsRemaining"`
	SharedFolder     *agentSharedView `json:"sharedFolder,omitempty"`
	Error            *agentErrorView  `json:"error,omitempty"`
	TimedOut         bool             `json:"timedOut,omitempty"`
}

type agentErrorView struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// agentViewOf describes an agent VM, or reports false when there is no such
// agent VM (or failed-provisioning record).
func (m *Manager) agentViewOf(name string) (agentVMView, bool) {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	vm, op, sharedDir := m.vms[name], m.agentOps[name], m.cfg.SharedDir
	leased := vm != nil && vm.Lease != nil
	if !leased && (op == nil || op.Phase != "failed") {
		return agentVMView{}, false
	}
	v := agentVMView{Name: name}
	if op != nil {
		v.Owner, v.Template = op.Owner, op.Template
	}
	if leased {
		l := vm.Lease
		v.Owner, v.Template = l.Owner, l.Template
		created, expires := l.CreatedAt, l.ExpiresAt
		v.CreatedAt, v.ExpiresAt = &created, &expires
		v.SecondsRemaining = max(0, int(expires.Sub(now).Seconds()))
		v.State, v.IP, v.GuestAgentOK, v.SSHOK = vm.State, vm.IP, vm.AgentOK, vm.SSHOK
		v.SharedFolder = &agentSharedView{
			Host:  sharedDir + "/" + agentSharedSub + "/" + name,
			Guest: guestSharedMount + "/" + agentSharedSub + "/" + name,
		}
	}
	switch {
	case op != nil && op.Phase == "failed":
		v.Phase = "failed"
		v.Error = &agentErrorView{Code: op.Code, Message: op.Message}
	case op != nil && op.Phase == "cloning":
		v.Phase = "cloning"
	case vm.State == "running" && (vm.AgentOK || vm.SSHOK):
		v.Phase, v.Ready = "ready", true
	case vm.State == "running" || vm.State == "starting" || (op != nil && op.Phase == "booting"):
		v.Phase = "booting"
	default:
		v.Phase = "stopped"
		msg := vm.LastError
		if msg == "" {
			msg = "the VM is not running (it was stopped, or Tart Oven restarted); destroy it and create a new one"
		}
		v.Error = &agentErrorView{Code: "not_running", Message: msg}
	}
	return v, true
}

// lookupAgentVM resolves {name} for p, answering 404 for anything p must not
// know about (another agent's VM looks exactly like a missing one).
func (m *Manager) lookupAgentVM(w http.ResponseWriter, r *http.Request, p principal) (agentVMView, bool) {
	name := r.PathValue("name")
	v, ok := m.agentViewOf(name)
	if !ok || !agentVisible(p, v.Owner) {
		writeAgentError(w, agentErr(http.StatusNotFound, "not_found", "no agent VM named %q", name))
		return agentVMView{}, false
	}
	return v, true
}

func (m *Manager) registerAgentRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/agent/info", m.agentHandler(m.handleAgentInfo))
	mux.HandleFunc("GET /api/agent/guide", m.agentHandler(func(w http.ResponseWriter, r *http.Request, _ principal) {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Write(agentGuide)
	}))
	mux.HandleFunc("GET /api/agent/openapi.json", m.agentHandler(func(w http.ResponseWriter, r *http.Request, _ principal) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(agentOpenAPI)
	}))
	mux.HandleFunc("GET /api/agent/vms", m.agentHandler(m.handleAgentList))
	mux.HandleFunc("POST /api/agent/vms", m.agentHandler(m.handleAgentCreate))
	mux.HandleFunc("GET /api/agent/vms/{name}", m.agentHandler(func(w http.ResponseWriter, r *http.Request, p principal) {
		if v, ok := m.lookupAgentVM(w, r, p); ok {
			writeJSON(w, v)
		}
	}))
	mux.HandleFunc("GET /api/agent/vms/{name}/wait", m.agentHandler(m.handleAgentWait))
	mux.HandleFunc("POST /api/agent/vms/{name}/exec", m.agentHandler(m.handleAgentExec))
	mux.HandleFunc("POST /api/agent/vms/{name}/extend", m.agentHandler(m.handleAgentExtend))
	mux.HandleFunc("DELETE /api/agent/vms/{name}", m.agentHandler(m.handleAgentDestroy))
}

func (m *Manager) handleAgentInfo(w http.ResponseWriter, r *http.Request, p principal) {
	type templateView struct {
		Name      string `json:"name"`
		Available bool   `json:"available"`
		State     string `json:"state,omitempty"`
	}
	m.mu.Lock()
	templates := make([]templateView, 0, len(m.cfg.AgentTemplates))
	for _, name := range m.cfg.AgentTemplates {
		tv := templateView{Name: name}
		if vm := m.vms[name]; vm != nil {
			tv.State = vm.State
			tv.Available = vm.State == "stopped"
		}
		templates = append(templates, tv)
	}
	active := m.agentActiveLocked()
	maxTTL := m.cfg.AgentMaxTTLMin
	shared := m.cfg.SharedDir
	m.mu.Unlock()
	writeJSON(w, map[string]any{
		"version":   tartoven.Version,
		"you":       p.name,
		"templates": templates,
		"capacity":  map[string]int{"maxRunning": hardMaxConcurrent, "running": active, "free": max(0, hardMaxConcurrent-active)},
		"limits": map[string]int{
			"defaultTtlMinutes": min(defaultAgentTTLMin, maxTTL),
			"maxTtlMinutes":     maxTTL,
			"defaultExecSec":    defaultAgentExecSec,
			"maxExecSec":        maxAgentExecSec,
			"maxOutputBytes":    agentOutputLimit,
			"maxWaitSec":        maxAgentWaitSec,
		},
		"sharedFolder": map[string]string{"hostRoot": shared + "/" + agentSharedSub, "guestRoot": guestSharedMount + "/" + agentSharedSub},
		"guide":        "/api/agent/guide",
		"openapi":      "/api/agent/openapi.json",
	})
}

func (m *Manager) handleAgentList(w http.ResponseWriter, r *http.Request, p principal) {
	m.mu.Lock()
	names := make([]string, 0, len(m.vms)+len(m.agentOps))
	seen := map[string]bool{}
	for n, vm := range m.vms {
		if vm.Lease != nil {
			names, seen[n] = append(names, n), true
		}
	}
	for n := range m.agentOps {
		if !seen[n] {
			names = append(names, n)
		}
	}
	m.mu.Unlock()
	sort.Strings(names)
	out := make([]agentVMView, 0, len(names))
	for _, n := range names {
		if v, ok := m.agentViewOf(n); ok && agentVisible(p, v.Owner) {
			out = append(out, v)
		}
	}
	writeJSON(w, map[string]any{"vms": out})
}

func (m *Manager) handleAgentCreate(w http.ResponseWriter, r *http.Request, p principal) {
	var req agentCreateReq
	if e := decodeAgentBody(w, r, &req); e != nil {
		writeAgentError(w, e)
		return
	}
	name, e := m.startAgentVM(p.name, req)
	if e != nil {
		writeAgentError(w, e)
		return
	}
	v, _ := m.agentViewOf(name)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Location", "/api/agent/vms/"+name)
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(v)
}

func (m *Manager) handleAgentWait(w http.ResponseWriter, r *http.Request, p principal) {
	v, ok := m.lookupAgentVM(w, r, p)
	if !ok {
		return
	}
	secs := defaultAgentWaitSec
	if s := r.URL.Query().Get("timeout"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > maxAgentWaitSec {
			writeAgentError(w, agentErr(400, "invalid_request", "timeout must be 1-%d seconds", maxAgentWaitSec))
			return
		}
		secs = n
	}
	deadline := time.NewTimer(time.Duration(secs) * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		if v.Ready || v.Phase == "failed" || v.Phase == "stopped" {
			writeJSON(w, v)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-deadline.C:
			v.TimedOut = true
			writeJSON(w, v)
			return
		case <-tick.C:
		}
		var found bool
		if v, found = m.agentViewOf(v.Name); !found {
			writeAgentError(w, agentErr(404, "not_found", "agent VM %q no longer exists", v.Name))
			return
		}
	}
}

var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// agentCommand wraps the caller's command with its working directory and
// environment. Keys are validated; values are single-quoted.
func agentCommand(command, cwd string, env map[string]string) (string, error) {
	var b strings.Builder
	keys := make([]string, 0, len(env))
	for k := range env {
		if !envKeyRe.MatchString(k) {
			return "", errors.New("invalid environment variable name " + strconv.Quote(k))
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString("export " + k + "=" + shellQuote(env[k]) + "; ")
	}
	if cwd != "" {
		b.WriteString("cd " + shellQuote(cwd) + " || exit 1; ")
	}
	b.WriteString(command)
	return b.String(), nil
}

type agentExecReq struct {
	Command    string            `json:"command"`
	TimeoutSec int               `json:"timeoutSec"`
	Cwd        string            `json:"cwd"`
	Env        map[string]string `json:"env"`
}

type agentExecResp struct {
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	ExitCode   int    `json:"exitCode"`
	TimedOut   bool   `json:"timedOut"`
	Truncated  bool   `json:"truncated"`
	DurationMs int64  `json:"durationMs"`
}

func (m *Manager) handleAgentExec(w http.ResponseWriter, r *http.Request, p principal) {
	v, ok := m.lookupAgentVM(w, r, p)
	if !ok {
		return
	}
	var req agentExecReq
	if e := decodeAgentBody(w, r, &req); e != nil {
		writeAgentError(w, e)
		return
	}
	switch {
	case strings.TrimSpace(req.Command) == "":
		writeAgentError(w, agentErr(400, "invalid_request", "command is required"))
		return
	case len(req.Command) > maxAgentCommandLen:
		writeAgentError(w, agentErr(400, "invalid_request", "command is longer than %d bytes; write a script to the shared folder instead", maxAgentCommandLen))
		return
	case len(req.Env) > maxAgentEnvVars:
		writeAgentError(w, agentErr(400, "invalid_request", "at most %d environment variables", maxAgentEnvVars))
		return
	}
	secs := req.TimeoutSec
	if secs == 0 {
		secs = defaultAgentExecSec
	}
	if secs < 1 || secs > maxAgentExecSec {
		writeAgentError(w, agentErr(400, "invalid_request", "timeoutSec must be 1-%d", maxAgentExecSec))
		return
	}
	command, err := agentCommand(req.Command, req.Cwd, req.Env)
	if err != nil {
		writeAgentError(w, agentErr(400, "invalid_request", "%v", err))
		return
	}
	if !v.Ready {
		e := agentErr(409, "not_ready", "VM %s is %s, not ready for commands; call GET /api/agent/vms/%s/wait first", v.Name, v.Phase, v.Name)
		e.Extra = map[string]any{"phase": v.Phase}
		writeAgentError(w, e)
		return
	}

	preview := req.Command
	if len(preview) > 100 {
		preview = preview[:100] + "..."
	}
	m.logln("agent %s exec on %s (%d bytes): %s", p.name, v.Name, len(req.Command), strings.ReplaceAll(preview, "\n", " "))

	ctx, cancel := context.WithTimeout(withExecOutputLimit(r.Context(), agentOutputLimit), time.Duration(secs)*time.Second)
	defer cancel()
	started := time.Now()
	res := m.execInGuest(ctx, v.Name, command, "")
	if r.Context().Err() != nil {
		return // the caller went away; nothing to answer
	}
	timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)
	if res.Error != "" && !timedOut {
		e := agentErr(502, "exec_failed", "%s", res.Error)
		writeAgentError(w, e)
		return
	}
	exit := res.ExitCode
	if timedOut {
		exit = -1
	}
	writeJSON(w, agentExecResp{
		Stdout: res.Stdout, Stderr: res.Stderr, ExitCode: exit, TimedOut: timedOut,
		Truncated: res.Truncated, DurationMs: time.Since(started).Milliseconds(),
	})
}

func (m *Manager) handleAgentExtend(w http.ResponseWriter, r *http.Request, p principal) {
	v, ok := m.lookupAgentVM(w, r, p)
	if !ok {
		return
	}
	var req struct {
		TTLMinutes int `json:"ttlMinutes"`
	}
	if e := decodeAgentBody(w, r, &req); e != nil {
		writeAgentError(w, e)
		return
	}
	m.mu.Lock()
	maxTTL := m.cfg.AgentMaxTTLMin
	m.mu.Unlock()
	if req.TTLMinutes == 0 {
		req.TTLMinutes = min(defaultAgentTTLMin, maxTTL)
	}
	if req.TTLMinutes < 1 || req.TTLMinutes > maxTTL {
		writeAgentError(w, agentErr(400, "invalid_ttl", "ttlMinutes must be between 1 and %d", maxTTL))
		return
	}
	m.mu.Lock()
	vm := m.vms[v.Name]
	if vm == nil || vm.Lease == nil {
		m.mu.Unlock()
		writeAgentError(w, agentErr(404, "not_found", "no agent VM named %q", v.Name))
		return
	}
	if time.Now().After(vm.Lease.ExpiresAt) {
		m.mu.Unlock()
		writeAgentError(w, agentErr(409, "lease_expired", "the lease already expired and the VM is being deleted"))
		return
	}
	vm.Lease.ExpiresAt = time.Now().Add(time.Duration(req.TTLMinutes) * time.Minute)
	m.save()
	m.mu.Unlock()
	m.broadcast()
	nv, _ := m.agentViewOf(v.Name)
	writeJSON(w, nv)
}

func (m *Manager) handleAgentDestroy(w http.ResponseWriter, r *http.Request, p principal) {
	v, ok := m.lookupAgentVM(w, r, p)
	if !ok {
		return
	}
	m.logln("agent %s destroys %s", p.name, v.Name)
	if err := m.destroyLeasedVM(v.Name); err != nil {
		writeAgentError(w, agentErr(500, "destroy_failed", "%v", err))
		return
	}
	m.mu.Lock()
	delete(m.agentOps, v.Name)
	m.mu.Unlock()
	writeJSON(w, map[string]any{"destroyed": true, "name": v.Name})
}

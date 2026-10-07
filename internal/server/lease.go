package server

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Agent leases
//
// A VM created through the agent API carries a Lease. While it is set the
// scheduler neither starts nor stops the VM, and when it expires the VM is
// stopped and deleted, so a crashed or forgetful agent cannot hold one of the
// host's two VM slots forever. Provisioning (clone, boot, wait for the guest
// agent) is tracked in an agentOp until the VM is ready or has failed.
// ---------------------------------------------------------------------------

const (
	defaultAgentTTLMin = 60
	maxAgentTTLMin     = 24 * 60 // ceiling for the configurable maximum

	agentVMPrefix = "agent-"
	// agentSharedSub is the folder under the shared directory where each agent
	// VM gets its own subfolder, so agents never see each other's files.
	agentSharedSub = "agent"
	// guestSharedMount is where Virtualization.framework shows the host_resources
	// share inside a macOS guest.
	guestSharedMount = "/Volumes/My Shared Files/host_resources"

	// The clone must finish before healStuck (maxOpAge) would clear the busy
	// flag that protects the half-built VM from reconcile.
	agentCloneTimeout = maxOpAge - 30*time.Second

	leaseReapInterval  = 30 * time.Second
	agentOpKeep        = time.Hour
	agentSharedKeepFor = 24 * time.Hour
)

// Lease marks a VM as owned by an agent and bounds its lifetime.
type Lease struct {
	Owner     string    `json:"owner"`    // agent token name
	Template  string    `json:"template"` // VM it was cloned from
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// agentOp tracks one agent VM's provisioning. Phase is cloning, booting, done or
// failed; Code and Message describe a failure.
type agentOp struct {
	Owner    string
	Template string
	Phase    string
	Code     string
	Message  string
	Updated  time.Time
}

// agentError is a client-facing failure with a stable machine-readable code.
type agentError struct {
	Status  int
	Code    string
	Message string
	Extra   map[string]any
}

func (e *agentError) Error() string { return e.Code + ": " + e.Message }

func agentErr(status int, code, format string, a ...any) *agentError {
	return &agentError{Status: status, Code: code, Message: fmt.Sprintf(format, a...)}
}

// cleanNameList trims names, drops blanks and removes duplicates, keeping order.
func cleanNameList(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, n := range in {
		if n = strings.TrimSpace(n); n != "" && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

var labelRe = regexp.MustCompile(`[^a-z0-9]+`)

// agentLabel turns a caller-supplied label into a short, name-safe slug.
func agentLabel(s string) string {
	s = strings.Trim(labelRe.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if len(s) > 20 {
		s = strings.Trim(s[:20], "-")
	}
	return s
}

func (m *Manager) setAgentOp(name string, op *agentOp) {
	// Caller holds m.mu.
	if m.agentOps == nil {
		m.agentOps = map[string]*agentOp{}
	}
	op.Updated = time.Now()
	m.agentOps[name] = op
}

// agentSharedHostDir is the host folder an agent VM can use to exchange files.
func (m *Manager) agentSharedHostDir(name string) string {
	m.mu.Lock()
	dir := m.cfg.SharedDir
	m.mu.Unlock()
	return filepath.Join(dir, agentSharedSub, name)
}

// agentActiveLocked counts the VM slots in use: running or starting VMs, plus
// agent VMs still being cloned that are about to take one. Caller holds m.mu.
func (m *Manager) agentActiveLocked() int {
	n := 0
	for _, vm := range m.vms {
		if vm.State == "running" || vm.State == "starting" {
			n++
		}
	}
	for name, op := range m.agentOps {
		if op.Phase == "cloning" {
			if vm := m.vms[name]; vm == nil || vm.State != "running" && vm.State != "starting" {
				n++
			}
		}
	}
	return n
}

type agentCreateReq struct {
	Template   string `json:"template"`
	Label      string `json:"label"`
	TTLMinutes int    `json:"ttlMinutes"`
	CPU        int    `json:"cpu"`
	Memory     int    `json:"memory"`
	DiskSize   int    `json:"diskSize"`
	Headless   *bool  `json:"headless"`
}

// startAgentVM validates the request, reserves a name and a slot, and begins
// provisioning in the background. It returns as soon as the VM is registered.
func (m *Manager) startAgentVM(owner string, req agentCreateReq) (string, *agentError) {
	req.Template = strings.TrimSpace(req.Template)
	if req.Template == "" {
		return "", agentErr(400, "invalid_request", "template is required")
	}
	if req.CPU < 0 || req.CPU > 32 || req.Memory < 0 || req.Memory > 131072 || req.DiskSize < 0 || req.DiskSize > 1024 {
		return "", agentErr(400, "invalid_request", "cpu, memory (MB) or diskSize (GB) is out of range")
	}

	m.mu.Lock()
	allowed := false
	for _, t := range m.cfg.AgentTemplates {
		if t == req.Template {
			allowed = true
		}
	}
	templates := append([]string(nil), m.cfg.AgentTemplates...)
	maxTTL := m.cfg.AgentMaxTTLMin
	tpl := m.vms[req.Template]
	m.mu.Unlock()

	if !allowed {
		e := agentErr(403, "template_not_allowed", "%q is not an agent template; allowed: %s", req.Template, strings.Join(templates, ", "))
		e.Extra = map[string]any{"templates": templates}
		return "", e
	}
	if tpl == nil {
		return "", agentErr(404, "template_not_found", "template VM %q does not exist", req.Template)
	}
	ttl := req.TTLMinutes
	if ttl == 0 {
		ttl = min(defaultAgentTTLMin, maxTTL)
	}
	if ttl < 1 || ttl > maxTTL {
		return "", agentErr(400, "invalid_ttl", "ttlMinutes must be between 1 and %d", maxTTL)
	}

	label := agentLabel(req.Label)
	nameTpl := agentVMPrefix + "$RAND8"
	if label != "" {
		nameTpl = agentVMPrefix + label + "-$RAND8"
	}
	name, err := m.allocVMName(newVMNamer(nameTpl, nil))
	if err != nil {
		return "", agentErr(500, "internal", "%v", err)
	}

	now := time.Now()
	lease := &Lease{Owner: owner, Template: req.Template, CreatedAt: now, ExpiresAt: now.Add(time.Duration(ttl) * time.Minute)}

	m.mu.Lock()
	// Re-check the template and the slots under the lock that registers the VM.
	if tpl = m.vms[req.Template]; tpl == nil || tpl.State != "stopped" {
		m.mu.Unlock()
		m.releaseVMName(name)
		if tpl == nil {
			return "", agentErr(404, "template_not_found", "template VM %q does not exist", req.Template)
		}
		return "", agentErr(409, "template_busy", "template VM %q is %s; stop it before cloning", req.Template, tpl.State)
	}
	if active := m.agentActiveLocked(); active >= hardMaxConcurrent {
		var leased []string
		for n, v := range m.vms {
			if v.Lease != nil {
				leased = append(leased, n)
			}
		}
		m.mu.Unlock()
		m.releaseVMName(name)
		e := agentErr(409, "capacity", "%d of %d VM slots are in use; destroy a VM or wait for one to finish", active, hardMaxConcurrent)
		e.Extra = map[string]any{"leasedVMs": leased}
		return "", e
	}
	// Register the VM now, busy, so the scheduler ignores it and reconcile
	// keeps it while the clone is still being written.
	m.vms[name] = &VM{Name: name, State: "stopped", Lease: lease}
	m.setBusy(name, true)
	m.setAgentOp(name, &agentOp{Owner: owner, Template: req.Template, Phase: "cloning"})
	m.save()
	m.mu.Unlock()
	m.broadcast()

	headless := req.Headless == nil || *req.Headless
	m.logln("agent %s: creating %s from %s (lease %dm)", owner, name, req.Template, ttl)
	go m.provisionAgentVM(name, req, headless)
	return name, nil
}

// provisionAgentVM clones the template, applies the requested resources and
// boots the VM. Any failure deletes the half-built VM and records why.
func (m *Manager) provisionAgentVM(name string, req agentCreateReq, headless bool) {
	t := m.newTask("clone", name)
	m.broadcast()
	timer := time.AfterFunc(agentCloneTimeout, func() { m.cancelTask(t.ID) })
	err := m.runInto(t, "clone", req.Template, name)
	if err == nil {
		if args := buildSetArgs(name, req.CPU, req.Memory, req.DiskSize, "", false, false); len(args) > 2 {
			err = m.runInto(t, args...)
		}
	}
	timer.Stop()
	m.finishTask(t, err)
	m.releaseVMName(name)
	if err != nil {
		m.failAgentVM(name, "clone_failed", fmt.Sprintf("cloning %q failed: %v", req.Template, err))
		return
	}

	if err := os.MkdirAll(m.agentSharedHostDir(name), 0o755); err != nil {
		m.logln("agent: shared folder for %s: %v", name, err) // the VM still works without it
	}

	m.mu.Lock()
	if op := m.agentOps[name]; op != nil {
		op.Phase = "booting"
		m.setAgentOp(name, op)
	}
	m.setBusy(name, false) // doRun takes the busy flag itself
	m.save()
	m.mu.Unlock()
	m.broadcast()

	// Shared (NAT) networking, and none of the dashboard's custom run args:
	// an agent VM behaves the same whatever the host has configured, and is not
	// given its own address on the physical network.
	m.doRun(name, "agent", runOptions{Headless: headless, Override: &runOverride{Args: []string{}, Network: "shared"}})

	m.mu.Lock()
	vm := m.vms[name]
	started := vm != nil && vm.State == "running"
	reason := ""
	if vm != nil {
		reason = vm.LastError
	}
	m.mu.Unlock()
	if !started {
		if reason == "" {
			reason = "the VM did not start"
		}
		code := "boot_failed"
		if strings.Contains(reason, "VMs already running") {
			code = "capacity"
		}
		m.failAgentVM(name, code, reason)
		return
	}
	m.mu.Lock()
	if op := m.agentOps[name]; op != nil {
		op.Phase = "done"
		m.setAgentOp(name, op)
	}
	m.mu.Unlock()
	m.broadcast()
}

// failAgentVM records why provisioning failed, then removes the VM.
func (m *Manager) failAgentVM(name, code, message string) {
	m.logln("agent: %s failed (%s): %s", name, code, message)
	m.mu.Lock()
	op := m.agentOps[name]
	if op == nil {
		op = &agentOp{}
	}
	op.Phase, op.Code, op.Message = "failed", code, message
	m.setAgentOp(name, op)
	if vm := m.vms[name]; vm != nil && vm.Lease != nil {
		m.setBusy(name, false)
	}
	m.mu.Unlock()
	if err := m.destroyLeasedVM(name); err != nil {
		m.logln("agent: cleaning up %s: %v", name, err)
	}
}

// waitNotBusy waits for an in-flight operation on name to finish.
func (m *Manager) waitNotBusy(name string, limit time.Duration) bool {
	deadline := time.Now().Add(limit)
	for {
		m.mu.Lock()
		busy := m.busy[name]
		m.mu.Unlock()
		if !busy {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// destroyLeasedVM stops and deletes a leased VM. It refuses VMs that carry no
// lease, so an agent (or a bug) can never remove a template or a hand-made VM.
func (m *Manager) destroyLeasedVM(name string) error {
	if !m.waitNotBusy(name, 3*time.Minute) {
		return fmt.Errorf("%s is busy", name)
	}
	m.mu.Lock()
	vm := m.vms[name]
	if vm == nil {
		delete(m.agentOps, name)
		m.mu.Unlock()
		return nil
	}
	if vm.Lease == nil {
		m.mu.Unlock()
		return errors.New("not an agent VM")
	}
	active := vm.State == "running" || vm.State == "starting" || vm.State == "stopping"
	m.mu.Unlock()

	if active {
		m.doStop(name)
	}
	out, derr := m.tartCmd(m.storage(), "delete", name).CombinedOutput()
	m.reconcile()
	m.mu.Lock()
	_, stillThere := m.vms[name]
	if !stillThere {
		// Keep a failed op so a status call can still report why; drop the rest.
		if op := m.agentOps[name]; op != nil && op.Phase != "failed" {
			delete(m.agentOps, name)
		}
	}
	m.mu.Unlock()
	m.broadcast()
	if stillThere {
		if derr != nil {
			return fmt.Errorf("tart delete: %v: %s", derr, strings.TrimSpace(string(out)))
		}
		return errors.New("VM still exists after delete")
	}
	return nil
}

// reapLeases deletes every VM whose lease has run out and forgets stale ops.
func (m *Manager) reapLeases(now time.Time) {
	var expired []string
	m.mu.Lock()
	for name, vm := range m.vms {
		if vm.Lease != nil && now.After(vm.Lease.ExpiresAt) && !m.busy[name] && !m.reaping[name] {
			expired = append(expired, name)
		}
	}
	for _, name := range expired {
		if m.reaping == nil {
			m.reaping = map[string]bool{}
		}
		m.reaping[name] = true
	}
	for name, op := range m.agentOps {
		if op.Phase == "failed" && now.Sub(op.Updated) > agentOpKeep {
			delete(m.agentOps, name)
		}
	}
	m.mu.Unlock()

	for _, name := range expired {
		m.logln("agent lease for %s expired; deleting it", name)
		go func(name string) {
			if err := m.destroyLeasedVM(name); err != nil {
				m.logln("agent: deleting expired %s: %v", name, err)
			}
			m.mu.Lock()
			delete(m.reaping, name)
			m.mu.Unlock()
		}(name)
	}
}

// pruneAgentSharedDirs removes the shared subfolders of agent VMs that are
// long gone. They are kept for a day after the VM so a result written just
// before a lease ran out can still be collected.
func (m *Manager) pruneAgentSharedDirs(now time.Time) {
	m.mu.Lock()
	root := filepath.Join(m.cfg.SharedDir, agentSharedSub)
	live := make(map[string]bool, len(m.vms))
	for n := range m.vms {
		live[n] = true
	}
	m.mu.Unlock()
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), agentVMPrefix) || live[e.Name()] {
			continue
		}
		if info, err := e.Info(); err == nil && now.Sub(info.ModTime()) > agentSharedKeepFor {
			os.RemoveAll(filepath.Join(root, e.Name()))
		}
	}
}

// leaseLoop enforces lease expiry independently of the scheduler, so a paused
// scheduler does not leak agent VMs.
func (m *Manager) leaseLoop() {
	m.reapLeases(time.Now())
	m.pruneAgentSharedDirs(time.Now())
	reap := time.NewTicker(leaseReapInterval)
	prune := time.NewTicker(time.Hour)
	defer reap.Stop()
	defer prune.Stop()
	for {
		select {
		case now := <-reap.C:
			m.reapLeases(now)
		case now := <-prune.C:
			m.pruneAgentSharedDirs(now)
		}
	}
}

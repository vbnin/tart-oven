package server

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"tart-oven/internal/perf"
)

// ---------------------------------------------------------------------------
// VM management (create / clone / set / rename / delete)
// ---------------------------------------------------------------------------

// shortID returns an 8-hex-char suffix, matching the style of the existing VM
// names (e.g. My_VM-0056405C).
func shortID() string { return fmt.Sprintf("%08X", rand.Uint32()) }

// lastLine returns the last non-empty line of s (for compact error messages).
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return ""
}

// prefixIfNotEmpty returns prefix+s when s is non-empty, else "".
func prefixIfNotEmpty(prefix, s string) string {
	if s == "" {
		return ""
	}
	return prefix + s
}

// newTask registers a management task and trims the list to the last 20. Its
// ctx/cancel let a running create/clone be killed on demand from the UI.
func (m *Manager) newTask(kind, target string) *Task {
	ctx, cancel := context.WithCancel(context.Background())
	t := &Task{
		ID:        fmt.Sprintf("%d", time.Now().UnixNano()),
		Kind:      kind,
		Target:    target,
		Status:    "running",
		StartedAt: time.Now(),
		ctx:       ctx,
		cancel:    cancel,
	}
	m.mu.Lock()
	m.tasks = append(m.tasks, t)
	if len(m.tasks) > 20 {
		m.tasks = m.tasks[len(m.tasks)-20:]
	}
	m.mu.Unlock()
	return t
}

// cancelBatch stops a create batch: no further VMs are started, and the task
// building the current one is cancelled.
func (m *Manager) cancelBatch(id string) bool {
	m.mu.Lock()
	b := m.batches[id]
	if b == nil {
		m.mu.Unlock()
		return false
	}
	b.Cancelled = true
	var running []string
	for _, t := range m.tasks {
		if t.Batch == id && t.Status == "running" {
			running = append(running, t.ID)
		}
	}
	m.mu.Unlock()
	for _, tid := range running {
		m.cancelTask(tid)
	}
	m.broadcast()
	return true
}

// cancelTask finds a running task by ID and cancels its in-flight command.
// The command's own exit path (runInto/cmdInto returning a context error)
// marks the task cancelled/finished — this just triggers that.
func (m *Manager) cancelTask(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.tasks {
		if t.ID == id {
			if t.Status != "running" || t.cancel == nil {
				return false
			}
			t.cancel()
			return true
		}
	}
	return false
}

// appendTaskOutput appends to a task's output (capped) and broadcasts at most
// once per second so a chatty download doesn't flood SSE subscribers.
func (m *Manager) appendTaskOutput(t *Task, s string) {
	m.mu.Lock()
	t.Output, t.termCarry = applyTerminalOutput(t.Output, t.termCarry, s)
	if len(t.Output) > 8192 {
		t.Output = t.Output[len(t.Output)-8192:]
	}
	do := time.Since(t.lastBcast) > time.Second
	if do {
		t.lastBcast = time.Now()
	}
	m.mu.Unlock()
	if do {
		m.broadcast()
	}
}

func (m *Manager) finishTask(t *Task, err error) {
	m.mu.Lock()
	t.FinishedAt = time.Now()
	switch {
	case err != nil && t.ctx.Err() == context.Canceled:
		t.Status = "cancelled"
		t.Error = "cancelled by user"
	case err != nil:
		t.Status = "error"
		t.Error = err.Error()
	default:
		t.Status = "success"
	}
	// Release the context's resources now that we're done with it, and clear
	// the func so further cancelTask calls on this ID become no-ops.
	if t.cancel != nil {
		t.cancel()
		t.cancel = nil
	}
	m.mu.Unlock()
}

// taskWriter funnels a command's stdout/stderr into a task's output.
type taskWriter struct {
	m *Manager
	t *Task
}

func (w *taskWriter) Write(p []byte) (int, error) {
	w.m.appendTaskOutput(w.t, string(p))
	return len(p), nil
}

// runInto runs a tart command, streaming output into the task. Bound to the
// task's context, so cancelTask kills it (and any child process) outright.
func (m *Manager) runInto(t *Task, args ...string) error {
	m.appendTaskOutput(t, "$ tart "+strings.Join(args, " ")+"\n")
	m.logln("$ tart %s", strings.Join(args, " "))
	cmd := m.tartCmdCtx(t.ctx, m.storage(), args...)
	w := &taskWriter{m, t}
	cmd.Stdout = w
	cmd.Stderr = w
	err := cmd.Run()
	if err != nil {
		m.logln("tart %s → %v", strings.Join(args, " "), err)
	}
	return err
}

// cmdInto runs an arbitrary command, streaming output into the task. Bound to
// the task's context, so cancelTask kills it outright.
func (m *Manager) cmdInto(t *Task, name string, args ...string) error {
	m.appendTaskOutput(t, "$ "+name+" "+strings.Join(args, " ")+"\n")
	m.logln("$ %s %s", name, strings.Join(args, " "))
	cmd := exec.CommandContext(t.ctx, name, args...)
	w := &taskWriter{m, t}
	cmd.Stdout = w
	cmd.Stderr = w
	err := cmd.Run()
	if err != nil {
		m.logln("%s → %v", name, err)
	}
	return err
}

// installTart downloads the latest tart release from GitHub (the manual install
// documented at tart.run) and places tart.app under the configured Tart app
// path (default /Applications). Used for both first install and "Update Tart".
func (m *Manager) installTart() {
	t := m.newTask("install", "tart")
	m.broadcast()

	m.mu.Lock()
	binPath := m.cfg.TartAppPath
	m.mu.Unlock()
	if strings.TrimSpace(binPath) == "" {
		binPath = defaultTartBin
	}
	dest := appBundleFromBin(binPath) // e.g. /Applications/tart.app

	tmp, err := os.MkdirTemp("", "tart-install")
	if err != nil {
		m.finishTask(t, err)
		m.broadcast()
		return
	}
	defer os.RemoveAll(tmp)

	archive := filepath.Join(tmp, "tart.tar.gz")
	steps := [][]string{
		{"curl", "-fsSL", "-o", archive, "https://github.com/openai/tart/releases/latest/download/tart.tar.gz"},
		{"tar", "-xzf", archive, "-C", tmp},
		{"rm", "-rf", dest},
		{"cp", "-R", filepath.Join(tmp, "tart.app"), dest},
	}
	for _, s := range steps {
		if err = m.cmdInto(t, s[0], s[1:]...); err != nil {
			break
		}
	}
	if err == nil {
		m.detectTartJSON() // re-probe now that tart exists / changed
		m.detectProvisioningSupport()
		m.updateTartVersion()
		m.mu.Lock()
		ver := m.tartVersion
		m.mu.Unlock()
		m.appendTaskOutput(t, "\nTart installed at "+dest+" ("+ver+")\n")
		m.reconcile()
	}
	m.finishTask(t, err)
	m.broadcast()
}

// createReq is the body of POST /api/vm/create.
type createReq struct {
	Mode         string `json:"mode"` // "ipsw" | "clone"
	Source       string `json:"source"`
	FromIpsw     string `json:"fromIpsw"`
	NameTemplate string `json:"nameTemplate"` // VM name with optional $RAND8 / $AUTONUM; blank = $RAND8
	Prefix       string `json:"prefix"`       // legacy API: prefix + $RAND8, used only when NameTemplate is empty
	Count        int    `json:"count"`
	CPU          int    `json:"cpu"`
	Memory       int    `json:"memory"`
	DiskSize     int    `json:"diskSize"`
	Display      string `json:"display"`
	RandomMac    bool   `json:"randomMac"`
	RandomSerial bool   `json:"randomSerial"`
	AutoEnroll   bool   `json:"autoEnroll"` // clone-only: flag the VM so its next boot runs the MDM auto-enroll script

	// Clone-only: Jamf server profile to enroll each clone into at its first
	// boot. Implies AutoEnroll.
	AutoEnrollProfile string `json:"autoEnrollProfile"`

	// Clone-only: guest hostname applied at first boot. HostnameFromName wins;
	// a custom Hostname gets the VM's random suffix when creating several.
	Hostname         string `json:"hostname"`
	HostnameFromName bool   `json:"hostnameFromName"`

	// Guest provisioning (macOS 27+, IPSW creates only — see createVMs).
	ProvisioningEnabled     bool   `json:"provisioningEnabled"`
	ProvisioningFullName    string `json:"provisioningFullName"`
	ProvisioningUsername    string `json:"provisioningUsername"`
	ProvisioningPassword    string `json:"provisioningPassword"`
	ProvisioningAutoLogin   bool   `json:"provisioningAutoLogin"`
	ProvisioningRemoteLogin bool   `json:"provisioningRemoteLogin"`
}

// normalizeIpswSource accepts an http(s) URL or an existing local .ipsw file
// and returns the value to hand to `tart create --from-ipsw`.
func normalizeIpswSource(source string) (string, error) {
	source = strings.TrimSpace(source)
	if source == "" || source == "latest" {
		return "", errors.New("enter a local .ipsw path or a download URL")
	}
	if strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://") {
		return source, nil
	}
	// tart gets the path verbatim, so expand ~ here rather than rely on a shell.
	if strings.HasPrefix(source, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			source = filepath.Join(home, source[2:])
		}
	}
	if !strings.HasSuffix(strings.ToLower(source), ".ipsw") {
		return "", errors.New("local IPSW path must end with .ipsw")
	}
	if _, err := os.Stat(source); os.IsNotExist(err) {
		return "", fmt.Errorf("IPSW file not found: %s", source)
	} else if err != nil {
		return "", fmt.Errorf("cannot access IPSW file: %v", err)
	}
	return source, nil
}

// buildSetArgs assembles `tart set` args, omitting anything not provided.
func buildSetArgs(name string, cpu, memory, diskSize int, display string, randMac, randSerial bool) []string {
	args := []string{"set", name}
	if cpu > 0 {
		args = append(args, "--cpu", fmt.Sprintf("%d", cpu))
	}
	if memory > 0 {
		args = append(args, "--memory", fmt.Sprintf("%d", memory))
	}
	if diskSize > 0 {
		args = append(args, "--disk-size", fmt.Sprintf("%d", diskSize))
	}
	if display != "" {
		args = append(args, "--display", display)
	}
	if randMac {
		args = append(args, "--random-mac")
	}
	if randSerial {
		args = append(args, "--random-serial")
	}
	return args
}

// createVMs runs the (possibly multiple) create/clone operations sequentially,
// each as its own task, then applies the requested settings via `tart set`.
func (m *Manager) createVMs(req createReq, batchID string) {
	defer func() {
		m.mu.Lock()
		delete(m.batches, batchID)
		m.mu.Unlock()
		m.broadcast()
	}()
	namer := newVMNamer(req.NameTemplate, nil)
	for i := 0; i < req.Count; i++ {
		m.mu.Lock()
		b := m.batches[batchID]
		if b != nil && b.Cancelled {
			m.mu.Unlock()
			return
		}
		if b != nil {
			b.Started = i + 1
		}
		m.mu.Unlock()

		id := shortID()
		name, nameErr := m.allocVMName(namer)
		if nameErr != nil {
			m.logln("create: %v", nameErr)
			t := m.newTask(req.Mode, req.NameTemplate)
			m.mu.Lock()
			t.Batch = batchID
			m.mu.Unlock()
			m.finishTask(t, nameErr)
			m.broadcast()
			return
		}
		t := m.newTask(req.Mode, name)
		m.mu.Lock()
		t.Batch = batchID
		m.mu.Unlock()
		m.broadcast()

		var createArgs []string
		var setDisk int // disk-size applied via `set` (clone only; ipsw sets it at create)
		switch req.Mode {
		case "clone":
			createArgs = []string{"clone", req.Source, name}
			setDisk = req.DiskSize // grow the cloned disk if a larger size is asked
		default: // ipsw
			createArgs = []string{"create", name, "--from-ipsw", req.FromIpsw}
			if req.DiskSize > 0 {
				createArgs = append(createArgs, "--disk-size", fmt.Sprintf("%d", req.DiskSize))
			}
		}

		err := m.runInto(t, createArgs...)
		if err == nil {
			setArgs := buildSetArgs(name, req.CPU, req.Memory, setDisk, req.Display, req.RandomMac, req.RandomSerial)
			if len(setArgs) > 2 { // more than just "set <name>"
				err = m.runInto(t, setArgs...)
			}
		}
		if err == nil && req.Mode == "clone" && (req.HostnameFromName || req.Hostname != "") {
			m.mu.Lock()
			vm := m.vms[name]
			if vm == nil {
				vm = &VM{Name: name}
				m.vms[name] = vm
			}
			vm.HostnameFromName = req.HostnameFromName
			vm.Hostname = cloneHostname(req.Hostname, id, req.Count)
			m.save()
			m.mu.Unlock()
		}
		if err == nil && req.Mode == "clone" && (req.AutoEnroll || req.AutoEnrollProfile != "") {
			m.mu.Lock()
			vm := m.vms[name]
			if vm == nil {
				vm = &VM{Name: name}
				m.vms[name] = vm
			}
			vm.AutoEnroll = true
			vm.AutoEnrollProfile = req.AutoEnrollProfile
			m.save()
			m.mu.Unlock()
		}
		// Guest provisioning only applies to a fresh --from-ipsw create: it's
		// consumed on the guest's first boot, and a clone's guest has already
		// had its first boot.
		if err == nil && req.Mode != "clone" && req.ProvisioningEnabled {
			m.mu.Lock()
			if m.supportsProvisioning {
				vm := m.vms[name]
				if vm == nil {
					vm = &VM{Name: name}
					m.vms[name] = vm
				}
				vm.PendingProvisioning = renderProvisioningOpts(
					req.ProvisioningFullName, req.ProvisioningUsername, req.ProvisioningPassword,
					req.ProvisioningAutoLogin, req.ProvisioningRemoteLogin)
				m.save()
			}
			m.mu.Unlock()
		}
		m.finishTask(t, err)
		m.reconcile() // pick the new VM up before its name is released
		m.releaseVMName(name)
		if releasable, released := perf.MaybeReleaseGoMemory(perf.RuntimeGoMemory{}); released {
			m.logln("released idle Go heap after %s task (%.0f MiB eligible)", req.Mode, float64(releasable)/(1<<20))
		}
		m.reconcile()
		m.broadcast()
		if m.cfg.JamfRecon {
			go runJamf()
		}
	}
}

// isActive reports whether a VM is running or mid-start (can't be edited/deleted).
func (m *Manager) isActive(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	vm := m.vms[name]
	return vm != nil && (vm.State == "running" || vm.State == "starting" || m.busy[name])
}

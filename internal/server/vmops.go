package server

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"

	"tart-oven/internal/perf"
)

// ---------------------------------------------------------------------------
// VM operations. doRun/doStop perform the slow exec work WITHOUT holding the
// lock, marking the VM busy so the scheduler and reconcile won't touch it.
// HTTP handlers invoke these in their own goroutines; the scheduler invokes
// them inline (it is already its own goroutine).
// ---------------------------------------------------------------------------

func (m *Manager) failOp(name string, err error) {
	log.Printf("op on %q failed: %v", name, err)
	m.mu.Lock()
	if vm := m.vms[name]; vm != nil {
		vm.State = "stopped"
		vm.LastError = err.Error()
	}
	m.setBusy(name, false)
	m.mu.Unlock()
	m.broadcast()
}

// takeAutoEnroll checks whether a VM needs an auto-enroll attempt, clears the
// flag and the chosen Jamf profile to make it one-shot, and returns the profile
// to enroll into ("" = the guest's own) and whether to run the attempt. Must be
// called under m.mu.
func takeAutoEnroll(vm *VM) (profileID string, run bool) {
	if !vm.AutoEnroll {
		return "", false
	}
	profileID = vm.AutoEnrollProfile
	vm.AutoEnroll = false
	vm.AutoEnrollProfile = ""
	// Skip if already enrolled — the flag was set in error or the VM moved
	// hosts with stale state; either way, don't enroll over an active profile.
	return profileID, !vm.MDMEnrolled
}

// runWantsNoGraphics reports whether a VM run should use --no-graphics. The
// scheduler config only applies to scheduled runs; manual runs respect the
// per-run headless flag or an explicit --no-graphics in Custom run arguments.
// runOptions are the per-run choices for doRun.
type runOptions struct {
	Headless bool         // Run headless / restart of a headless run
	Override *runOverride // "Run with arguments": replaces Custom run arguments for this run
}

// runOverride is a "Run with arguments" selection. Args replace the
// configured Custom run arguments; Network "shared" skips bridging.
type runOverride struct {
	Args    []string `json:"args"`
	Network string   `json:"network,omitempty"` // "" = as configured | "shared"
}

const maxOverrideArgs = 40

func validateRunOverride(o *runOverride) error {
	if o == nil {
		return nil
	}
	if len(o.Args) > maxOverrideArgs {
		return fmt.Errorf("too many arguments (max %d)", maxOverrideArgs)
	}
	for _, a := range o.Args {
		if len(a) > 512 || strings.ContainsAny(a, "\x00\n\r") {
			return fmt.Errorf("invalid argument %q", a)
		}
	}
	if o.Network != "" && o.Network != "shared" {
		return fmt.Errorf("unknown network %q", o.Network)
	}
	return nil
}

// hasNetArg reports whether args already pick a network mode, in which case
// Tart Oven must not add its own --net-bridged on top.
func hasNetArg(args []string) bool {
	return hasArg(args, "--net-bridged") || hasArg(args, "--net-host") || hasArg(args, "--net-softnet")
}

func runWantsNoGraphics(trigger string, headless, cfgNoGraphics bool, extra []string) bool {
	if headless || hasArg(extra, "--no-graphics") {
		return true
	}
	return trigger == "scheduler" && cfgNoGraphics
}

// runWantsNoAudio reports whether a VM run should use --no-audio. The scheduler
// config only applies to scheduled runs; manual runs respect an explicit
// --no-audio in Custom run arguments.
func runWantsNoAudio(trigger string, cfgNoAudio bool, extra []string) bool {
	if hasArg(extra, "--no-audio") {
		return true
	}
	return trigger == "scheduler" && cfgNoAudio
}

func (m *Manager) doRun(name, trigger string, opts runOptions) {
	headless := opts.Headless
	m.mu.Lock()
	if m.busy[name] {
		m.mu.Unlock()
		return
	}
	vm := m.vms[name]
	if vm == nil {
		vm = &VM{Name: name}
		m.vms[name] = vm
	}
	if perf.DeferVMStartForHistory(m.performanceHistory) {
		if vm.State == "" {
			vm.State = "stopped"
		}
		vm.LastError = "not started: host is under critical memory pressure"
		m.mu.Unlock()
		m.broadcast()
		m.logln("deferred start of %s: host is under critical memory pressure", name)
		return
	}
	// Hard concurrency ceiling: Virtualization.framework allows at most 2 VMs.
	// Enforced here so it covers manual runs as well as the scheduler.
	active := 0
	for n, v := range m.vms {
		if n != name && (v.State == "running" || v.State == "starting") {
			active++
		}
	}
	if active >= hardMaxConcurrent {
		vm.LastError = fmt.Sprintf("not started: %d VMs already running (host max %d)", active, hardMaxConcurrent)
		m.mu.Unlock()
		m.broadcast()
		log.Printf("refusing to start %q: %d already running", name, active)
		return
	}
	now := time.Now()
	m.setBusy(name, true)
	vm.State = "starting"
	vm.LastError = ""
	vm.LastRun = now
	// A fresh boot deserves a fresh guest-agent probe (see execInGuest).
	delete(m.agentProbedSinceBoot, name)
	// Advance the sequential-scheduler cursor on every start (manual or
	// scheduler) so the next sequential pick continues after whatever last ran.
	m.lastSequential = name
	// Log this run to the history; keep the pointer so we can fill in the IP
	// once it resolves and the StoppedAt when it later stops.
	ev := &RunEvent{Name: name, StartedAt: now, Trigger: trigger}
	m.history = append(m.history, ev)
	m.pruneHistory()
	home := m.cfg.VMStoragePath
	shared := m.cfg.SharedDir
	jamf := m.cfg.JamfRecon
	extra := splitArgs(m.cfg.RunArgs)
	if opts.Override != nil {
		extra = append([]string(nil), opts.Override.Args...)
	}
	statusCmd := m.cfg.StatusCommand
	window := time.Duration(m.cfg.WindowMinutes) * time.Minute
	bootTimeout := m.cfg.BootTimeoutSec
	netPriority := m.cfg.NetPriority
	if opts.Override != nil && opts.Override.Network == "shared" {
		netPriority = "shared"
	}
	vm.RunOverride = opts.Override
	pendingProvisioning := vm.PendingProvisioning
	supportsProvisioning := m.supportsProvisioning
	m.save()
	m.mu.Unlock()
	m.broadcast()

	// "shared" opts out of bridged networking entirely (tart's own default:
	// shared/NAT through the host), for networks — typically enterprise
	// Wi-Fi with client/MAC isolation — that silently drop a bridged VM's
	// second MAC address at the access point, leaving the guest with a
	// DHCP lease but no route to its gateway.
	var iface string
	if netPriority != "shared" && !hasNetArg(extra) {
		var err error
		iface, err = activeInterface(netPriority)
		if err != nil {
			m.logln("run %s: no active network interface: %v", name, err)
			m.failOp(name, err)
			return
		}
	}

	// tart run blocks for the life of the VM, so start it detached and reap it
	// in the background to avoid zombies. Any user-supplied custom arguments
	// (e.g. --vnc, --no-audio) are appended last. We capture its stdout/stderr
	// into runLog so a boot failure surfaces tart's actual error message.
	runArgs := []string{"run", name,
		"--dir=host_resources:" + shared}
	if netPriority != "shared" && !hasNetArg(extra) {
		// Only auto-inject the detected interface if the user hasn't already
		// supplied their own --net-bridged in Custom run arguments — tart
		// treats --net-bridged as a repeatable flag, so both would apply and
		// bridge the guest onto the same physical interface twice.
		runArgs = append(runArgs, "--net-bridged="+iface)
	}
	if runWantsNoGraphics(trigger, headless, m.cfg.NoGraphics, extra) && !hasArg(extra, "--no-graphics") {
		runArgs = append(runArgs, "--no-graphics")
	}
	if runWantsNoAudio(trigger, m.cfg.NoAudio, extra) && !hasArg(extra, "--no-audio") {
		runArgs = append(runArgs, "--no-audio")
	}
	runArgs = append(runArgs, extra...)
	if pendingProvisioning != "" && supportsProvisioning {
		runArgs = append(runArgs, "--provisioning-opts="+pendingProvisioning)
	}
	logArgs := make([]string, len(runArgs))
	for i, a := range runArgs {
		logArgs[i] = redactProvisioningOpts(a) // never let the guest password reach the log file
	}
	m.logln("$ tart %s", strings.Join(logArgs, " "))
	runLog := &boundedBuffer{max: 8192}
	cmd := m.tartCmd(home, runArgs...)
	cmd.Stdout = runLog
	cmd.Stderr = runLog
	if err := cmd.Start(); err != nil {
		m.logln("run %s: failed to start tart: %v", name, err)
		m.failOp(name, err)
		return
	}
	m.mu.Lock()
	m.runningCmds[name] = cmd
	m.mu.Unlock()
	go func(targetCmd *exec.Cmd) {
		werr := targetCmd.Wait()
		m.mu.Lock()
		if m.runningCmds[name] == targetCmd {
			delete(m.runningCmds, name)
		}
		busy := m.busy[name]
		m.mu.Unlock()
		if werr != nil {
			// A quick non-zero exit usually means tart couldn't start the VM.
			m.logln("tart run %s exited: %v%s", name, werr, prefixIfNotEmpty("\n", runLog.String()))
		}
		// The `tart run` process ended — if it wasn't us stopping it (e.g. tart
		// quit on its own), reconcile so the UI reflects the VM as stopped
		// promptly instead of waiting for the next monitor tick.
		if !busy {
			m.reconcile()
			m.broadcast()
		}
	}(cmd)

	if jamf {
		go runJamf()
	}

	// Resolve the IP by matching Tart's configured VM MAC against the host's
	// ARP table and Tart IP resolvers. Keep polling until the guest receives an IP
	// or the configured boot deadline expires.
	ipCtx, cancelIP := context.WithTimeout(context.Background(), time.Duration(bootTimeout)*time.Second)
	ip, ipErr := pollVMIP(ipCtx, 1500*time.Millisecond, func(ctx context.Context) (string, error) {
		return m.resolveVMIPRobust(ctx, home, name, 1)
	})
	cancelIP()

	// Boot failure: the VM came up but never handed us an IP. Stop it so it
	// doesn't hold a concurrency slot, flag it, and let the next scheduler tick
	// pick a different VM.
	if ipErr != nil || ip == "" {
		tartOut := runLog.String()
		m.logln("boot failure %s: no IP after %ds.%s", name, bootTimeout, prefixIfNotEmpty(" tart said: ", tartOut))
		m.tartCmd(home, "stop", name, "-t", "10").Run()
		m.mu.Lock()
		c := m.runningCmds[name]
		m.mu.Unlock()
		if c != nil && c.Process != nil {
			c.Process.Kill()
		}
		if jamf {
			go runJamf()
		}
		detail := "boot failure: no IP after boot"
		if tartOut != "" {
			detail += " — " + lastLine(tartOut)
		}
		m.mu.Lock()
		vm.State = "stopped"
		vm.IP = ""
		vm.StartedAt = time.Time{}
		vm.StopAt = time.Time{}
		vm.Headless = false
		vm.RunOverride = nil
		vm.BootFailed = true
		vm.LastError = detail
		ev.StoppedAt = time.Now() // close the history entry
		m.setBusy(name, false)
		m.save()
		m.mu.Unlock()
		m.broadcast()
		return
	}

	m.mu.Lock()
	vm.State = "running"
	vm.IP = ip
	vm.StartedAt = time.Now()
	bootedAt := vm.StartedAt
	vm.StopAt = time.Now().Add(window)
	vm.Headless = runWantsNoGraphics(trigger, headless, m.cfg.NoGraphics, extra)
	vm.BootFailed = false // a clean boot clears any previous failure flag
	vm.LastError = ""
	vm.SSHOK = false // pending check; UI shows "checking…"
	vm.SSHCheckedAt = time.Time{}
	vm.AgentOK = false
	vm.AgentCheckedAt = time.Time{}
	ev.IP = ip
	if pendingProvisioning != "" {
		// First boot resolved an IP, so the guest applied the provisioning
		// options — don't reapply them on the next start.
		vm.PendingProvisioning = ""
	}
	m.setBusy(name, false)
	m.save()
	m.mu.Unlock()
	m.broadcast()
	log.Printf("started %q (ip=%s, window=%s)", name, ip, window)

	agentOK, sshOK := m.probeGuestChannels(name)
	go m.watchGuestChannelsAfterBoot(name, bootedAt, agentOK, sshOK)
	if agentOK || sshOK {
		m.applyHostnameIfPending(name)
	}

	// The status command only fills the Info column; connectivity was probed above.
	res := m.sshExec(name, statusCmd, "")
	infoOK, info := sshOutcome(res)
	m.mu.Lock()
	vm.Info = info
	vm.InfoAt = time.Now()
	m.save()
	m.mu.Unlock()
	m.broadcast()
	m.logln("info %s: ok=%v", name, infoOK)

	m.refreshMDMStatus(name)
	m.broadcast()

	// A VM cloned with "Auto enroll at boot" runs the MDM enrollment script
	// once it's up, whether this boot was the scheduler's or a manual Run —
	// that's the whole point of the flag. takeAutoEnroll clears the flag to
	// make it a one-time attempt regardless of outcome, and skips if already
	// enrolled. The trigger != "auto-enroll" guard stops this from firing a
	// second time when it was the manual /api/vm/auto-enroll endpoint that
	// called doRun to boot a stopped VM — that endpoint runs the enrollment
	// script itself right after doRun returns.
	m.mu.Lock()
	enrollProfile, needsEnroll := takeAutoEnroll(vm)
	m.save()
	m.mu.Unlock()
	if needsEnroll && trigger != "auto-enroll" {
		m.runAutoEnrollTask(name, trigger, enrollProfile)
	}
}

// sshPriorityShutdownCommand is the guest command run when "Prioritize shutdown
// using SSH" is enabled — a clean macOS shutdown request, tried before the fast
// tart stop.
const sshPriorityShutdownCommand = "sudo /sbin/shutdown -h now"

// sshPriorityShutdownWait bounds how long a prioritized SSH shutdown gets before
// falling back to the standard tart stop.
const sshPriorityShutdownWait = 30 * time.Second

func (m *Manager) doStop(name string) {
	m.mu.Lock()
	if m.busy[name] {
		m.mu.Unlock()
		return
	}
	vm := m.vms[name]
	if vm == nil {
		m.mu.Unlock()
		return
	}
	m.setBusy(name, true)
	vm.State = "stopping"
	home := m.cfg.VMStoragePath
	jamf := m.cfg.JamfRecon
	prioritizeSSH := m.cfg.PrioritizeSSHShutdown
	ip := vm.IP
	agentOK := vm.AgentOK
	sudoPassword := vm.SSHPassword
	if sudoPassword == "" {
		sudoPassword = m.cfg.SSHPassword
	}
	m.mu.Unlock()
	m.broadcast()

	stopped := false

	// Preferred, when enabled: ask the guest to shut down cleanly (via the
	// guest agent or SSH), bounded to sshPriorityShutdownWait. The connection
	// usually drops as the guest powers off, so the command's own result is
	// ignored — what matters is whether the VM actually stops within the deadline.
	if prioritizeSSH && (agentOK || ip != "") {
		deadline := time.Now().Add(sshPriorityShutdownWait)
		m.logln("clean shutdown %s: %q", name, sshPriorityShutdownCommand)
		sshCtx, cancel := context.WithDeadline(context.Background(), deadline)
		res := m.execInGuest(sshCtx, name, sshPriorityShutdownCommand, sudoPassword)
		skipped := false
		cancel()
		// A dropped connection is the normal outcome of a successful shutdown,
		// but a sudo refusal means the guest never got the request — skip the wait.
		if res.Error == "" && res.ExitCode != 0 && strings.Contains(res.Stderr, "sudo:") {
			m.logln("clean shutdown %s failed: %s", name, strings.TrimSpace(res.Stderr))
			deadline = time.Now()
			skipped = true
		}
		for time.Now().Before(deadline) {
			if !m.isRunning(name) {
				stopped = true
				break
			}
			time.Sleep(1 * time.Second)
		}
		if stopped {
			m.logln("%s shut down cleanly via guest command", name)
		} else {
			if !skipped {
				m.logln("%s did not stop within %s; falling back to tart stop", name, sshPriorityShutdownWait)
			} else {
				m.logln("%s: falling back to tart stop", name)
			}
		}
	}

	if !stopped {
		m.logln("$ tart stop %s -t 5", name)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		out, err := m.tartCmdCtx(ctx, home, "stop", name, "-t", "5").CombinedOutput()
		cancel()
		if err != nil {
			m.logln("tart stop %s → %v%s", name, err, prefixIfNotEmpty(" ", strings.TrimSpace(string(out))))
		}

		for i := 0; i < 6; i++ {
			time.Sleep(500 * time.Millisecond)
			if !m.isRunning(name) {
				stopped = true
				break
			}
		}
		if !stopped {
			m.logln("%s still running; forcing process termination", name)
			m.mu.Lock()
			cmd := m.runningCmds[name]
			m.mu.Unlock()
			if cmd != nil && cmd.Process != nil {
				cmd.Process.Kill()
			} else {
				m.tartCmd(home, "stop", name, "-t", "1").Run()
			}
		}
	}

	if jamf {
		go runJamf()
	}

	m.mu.Lock()
	vm.State = "stopped"
	vm.StartedAt = time.Time{}
	vm.StopAt = time.Time{}
	vm.Headless = false
	vm.RunOverride = nil
	// Keep last known IP, SSH status, and Info as a reference after the VM stops.
	// Close the most recent open history event for this VM.
	for i := len(m.history) - 1; i >= 0; i-- {
		if m.history[i].Name == name && m.history[i].StoppedAt.IsZero() {
			m.history[i].StoppedAt = time.Now()
			break
		}
	}
	m.setBusy(name, false)
	m.save()
	m.mu.Unlock()
	m.broadcast()
	log.Printf("stopped %q", name)
}

func (m *Manager) runSSHOperation(ctx context.Context, name, command, password string) execResult {
	if m.sshOperation != nil {
		return m.sshOperation(ctx, name, command, password)
	}
	return m.sshExecContext(ctx, name, command, password)
}

func (m *Manager) probeVMRunning(name string) (bool, error) {
	if m.runningProbe != nil {
		return m.runningProbe(name)
	}
	return m.vmRunningState(name)
}

func (m *Manager) finishVMRun(name, state string) {
	m.mu.Lock()
	if vm := m.vms[name]; vm != nil {
		vm.State = state
		vm.StartedAt = time.Time{}
		vm.StopAt = time.Time{}
		vm.Headless = false
		vm.RunOverride = nil
		vm.LastError = ""
	}
	for i := len(m.history) - 1; i >= 0; i-- {
		if m.history[i].Name == name && m.history[i].StoppedAt.IsZero() {
			m.history[i].StoppedAt = time.Now()
			break
		}
	}
	m.setBusy(name, false)
	m.save()
	m.mu.Unlock()
	m.broadcast()
}

func (m *Manager) vmRunningState(name string) (bool, error) {
	list, err := m.listTart()
	if err != nil {
		return false, err
	}
	for _, t := range list {
		if t.Name == name {
			return t.State == "running", nil
		}
	}
	return false, nil
}

func (m *Manager) isRunning(name string) bool {
	running, _ := m.vmRunningState(name)
	return running
}

package server

import (
	"time"
)

// clampAutoCheckSeconds keeps the auto-check interval in its allowed range:
// 0 (or less) is off, a smaller non-zero value is raised to the minimum, and a
// larger one is cut to the maximum.
func clampAutoCheckSeconds(v int) int {
	switch {
	case v <= 0:
		return 0
	case v < minAutoCheckSeconds:
		return minAutoCheckSeconds
	case v > maxAutoCheckSeconds:
		return maxAutoCheckSeconds
	}
	return v
}

// autoCheckLoop re-checks the running VMs every Config.AutoCheckSeconds. The
// interval is re-read each cycle and the wait is cut short by autoCheckReload
// when the setting changes, so a new value takes effect at once. With the
// setting at 0 it just waits for a change.
func (m *Manager) autoCheckLoop() {
	for {
		m.mu.Lock()
		secs := m.cfg.AutoCheckSeconds
		m.mu.Unlock()
		if secs <= 0 {
			<-m.autoCheckReload
			continue
		}
		timer := time.NewTimer(time.Duration(secs) * time.Second)
		select {
		case <-timer.C:
			m.autoCheckRunningVMs()
		case <-m.autoCheckReload:
			timer.Stop()
		}
	}
}

// autoCheckBootGrace is how long after a start the auto-check leaves a VM
// alone: the boot sequence (channel probes and their retries, status command,
// MDM and internet checks, hostname, auto-enroll) is still running then.
const autoCheckBootGrace = 5 * time.Minute

// autoCheckWantsLocked reports whether the auto-check should check this VM now.
// It skips VMs that are not running or have an operation in flight, VMs that
// booted a moment ago, VMs leased to an agent (the check re-decides whether
// agent exec goes through the agent or SSH, which an agent in the middle of a
// command should not see change), and VMs being auto-enrolled. Caller holds mu.
func (m *Manager) autoCheckWantsLocked(name string, vm *VM, now time.Time) bool {
	if vm == nil || vm.State != "running" || m.busy[name] || vm.Lease != nil {
		return false
	}
	if !vm.StartedAt.IsZero() && now.Sub(vm.StartedAt) < autoCheckBootGrace {
		return false
	}
	for _, t := range m.tasks {
		if t.Kind == "enroll" && t.Target == name && t.Status == "running" {
			return false
		}
	}
	return true
}

// autoCheckTargets lists the running VMs worth checking now.
func (m *Manager) autoCheckTargets() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	var names []string
	for name, vm := range m.vms {
		if m.autoCheckWantsLocked(name, vm, now) {
			names = append(names, name)
		}
	}
	return names
}

// autoCheckRunningVMs runs the same check as "Refresh info" on every running
// VM, one after the other (each can take a few seconds when a channel is down).
// Results that arrive after a VM began stopping are dropped (liveVMLocked).
func (m *Manager) autoCheckRunningVMs() {
	for _, name := range m.autoCheckTargets() {
		m.autoCheckOne(name)
	}
}

func (m *Manager) autoCheckOne(name string) {
	m.mu.Lock()
	stillWanted := m.autoCheckWantsLocked(name, m.vms[name], time.Now())
	m.mu.Unlock()
	if !stillWanted {
		return // stopped, became busy or began enrolling while earlier VMs were checked
	}
	m.refreshVMInfo(name)
}

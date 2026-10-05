package server

import (
	"math/rand"
	"sort"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Scheduler
// ---------------------------------------------------------------------------

func isOCI(source string) bool {
	return strings.EqualFold(strings.TrimSpace(source), "OCI")
}

func eligibleForScheduler(vm *VM, excludeOCI bool, excluded map[string]bool, busy bool) bool {
	if vm == nil || vm.State != "stopped" || busy {
		return false
	}
	if strings.Contains(vm.Name, templateMarker) || excluded[vm.Name] {
		return false
	}
	return !excludeOCI || !isOCI(vm.Source)
}

// schedulerLoop waits for the configured interval and ticks. The interval is
// re-read each cycle and the wait is interruptible via the reload channel so
// config changes take effect immediately.
func (m *Manager) schedulerLoop() {
	for {
		m.mu.Lock()
		iv := time.Duration(m.cfg.IntervalMinutes) * time.Minute
		m.mu.Unlock()
		select {
		case <-time.After(iv):
			m.tick()
		case <-m.reload:
			// interval changed; recompute and wait again
		}
	}
}

// tick: stop expired VMs, then (if storage is mounted) start one fresh VM up to
// the concurrency limit.
func (m *Manager) tick() {
	m.checkStorage()
	m.reconcile()

	m.mu.Lock()
	if m.cfg.Paused {
		m.mu.Unlock()
		return
	}
	mounted := m.storageMounted
	maxc := m.cfg.MaxConcurrent
	excludeOCI := m.cfg.ExcludeOCIFromScheduler
	now := time.Now()
	within := !m.cfg.DailyEnabled || inDailyWindow(now, m.cfg.DailyStart, m.cfg.DailyStop)
	excluded := make(map[string]bool, len(m.cfg.Excluded))
	for _, e := range m.cfg.Excluded {
		if e = strings.TrimSpace(e); e != "" {
			excluded[e] = true
		}
	}

	var toStop []string
	running := 0
	for _, vm := range m.vms {
		if vm.State == "running" {
			running++
			// Outside the daily active hours, stop everything; inside, only stop
			// VMs whose run window has expired.
			if !within || (!vm.StopAt.IsZero() && now.After(vm.StopAt)) {
				toStop = append(toStop, vm.Name)
			}
		}
	}

	// Split candidates so a VM that just failed to boot is retried only when no
	// fresh VM is available — i.e. the next tick prefers "another one".
	var fresh, failed []string
	if within && mounted && running-len(toStop) < maxc {
		for name, vm := range m.vms {
			if !eligibleForScheduler(vm, excludeOCI, excluded, m.busy[name]) {
				continue
			}
			if vm.BootFailed {
				failed = append(failed, name)
			} else {
				fresh = append(fresh, name)
			}
		}
	}
	mode := m.cfg.SchedulerMode
	lastSeq := m.lastSequential
	m.mu.Unlock()

	for _, n := range toStop {
		m.doStop(n)
	}
	candidates := fresh
	if len(candidates) == 0 {
		candidates = failed // everything else failed too; give them another go
	}
	if len(candidates) > 0 {
		var pick string
		if mode == "sequential" {
			// Walk the eligible VMs in alphabetical order, advancing past the
			// last VM that ran so we cycle through the whole list. (doRun updates
			// the cursor for both manual and scheduled starts.)
			sort.Strings(candidates)
			pick = nextSequential(candidates, lastSeq)
		} else {
			pick = candidates[rand.Intn(len(candidates))]
		}
		m.doRun(pick, "scheduler", runOptions{})
	}
}

// nextSequential returns the first name in the sorted list that comes after
// last (alphabetically), wrapping to the first when there is none.
func nextSequential(sorted []string, last string) string {
	for _, n := range sorted {
		if n > last {
			return n
		}
	}
	return sorted[0]
}

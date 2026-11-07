package server

import (
	"errors"
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

// A suspended VM is eligible only when the scheduler suspends VMs itself: it
// then resumes them like stopped ones. Otherwise a suspended VM was parked on
// purpose and is left alone.
func eligibleForScheduler(vm *VM, excludeOCI bool, excluded map[string]bool, busy, resumeSuspended bool) bool {
	if vm == nil || busy || !(vm.State == "stopped" || (vm.State == "suspended" && resumeSuspended)) {
		return false
	}
	if strings.Contains(vm.Name, templateMarker) || excluded[vm.Name] || vm.Lease != nil || vm.SnapshotOf != "" {
		return false
	}
	return !excludeOCI || !isOCI(vm.Source)
}

// endsBySuspend reports whether the scheduler suspends this VM at the end of
// its run window instead of stopping it: only with the setting on, and only if
// the VM was started suspendable, because otherwise the saved state could
// never be restored.
func endsBySuspend(vm *VM, schedulerSuspend bool) bool {
	return schedulerSuspend && vm.Suspendable
}

// stillRunningIdle reports whether name is running with nothing in flight on
// it: no start, stop or suspend, and no memory snapshot copying it. tick decides
// what to end under the lock but acts afterwards, one VM at a time, and a
// suspend can take minutes; the VM may have been suspended, stopped or
// snapshotted by someone else in the meantime. Ending it blindly could discard a
// suspended VM's saved memory (doStop runs `tart stop`) or stop a VM that is
// being copied.
func (m *Manager) stillRunningIdle(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	vm := m.vms[name]
	return vm != nil && vm.State == "running" && !m.busy[name] && !m.snapshotHold[name]
}

// suspendOrStop ends a scheduled run by suspending the VM. Only if `tart
// suspend` itself fails is it stopped instead, so an expired VM never keeps
// running (and holding one of the host's two VM slots) because the suspend
// failed. If the VM is no longer in a state to be suspended, it is left alone.
func (m *Manager) suspendOrStop(name string) {
	if !m.stillRunningIdle(name) {
		return
	}
	err := m.doSuspend(name)
	if err == nil {
		return
	}
	var failed *suspendFailedError
	if !errors.As(err, &failed) {
		m.logln("scheduler: not suspending %s: %v", name, err)
		return
	}
	m.logln("scheduler: could not suspend %s (%v); stopping it instead", name, err)
	if m.stillRunningIdle(name) {
		m.doStop(name)
	}
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
	schedulerSuspend := m.cfg.SchedulerSuspend
	now := time.Now()
	within := !m.cfg.DailyEnabled || inDailyWindow(now, m.cfg.DailyStart, m.cfg.DailyStop)
	excluded := make(map[string]bool, len(m.cfg.Excluded))
	for _, e := range m.cfg.Excluded {
		if e = strings.TrimSpace(e); e != "" {
			excluded[e] = true
		}
	}

	var toStop, toSuspend []string
	running := 0
	for _, vm := range m.vms {
		if vm.State == "running" {
			running++
			if vm.Lease != nil {
				continue // leased VMs live and die by their lease, not the run window
			}
			// Outside the daily active hours, stop everything; inside, only stop
			// VMs whose run window has expired.
			if !within || (!vm.StopAt.IsZero() && now.After(vm.StopAt)) {
				if endsBySuspend(vm, schedulerSuspend) {
					toSuspend = append(toSuspend, vm.Name)
				} else {
					toStop = append(toStop, vm.Name)
				}
			}
		}
	}

	// Split candidates so a VM that just failed to boot is retried only when no
	// fresh VM is available — i.e. the next tick prefers "another one".
	var fresh, failed []string
	if within && mounted && running-len(toStop)-len(toSuspend) < maxc {
		for name, vm := range m.vms {
			if !eligibleForScheduler(vm, excludeOCI, excluded, m.busy[name] || m.snapshotHold[name], schedulerSuspend) || m.familyConflictLocked(name) != "" {
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

	for _, n := range toSuspend {
		m.suspendOrStop(n)
	}
	for _, n := range toStop {
		if m.stillRunningIdle(n) {
			m.doStop(n)
		}
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
		if m.schedulerRunHook != nil {
			m.schedulerRunHook(pick)
		} else {
			m.doRun(pick, "scheduler", runOptions{})
		}
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

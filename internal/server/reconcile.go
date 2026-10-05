package server

import (
	"log"
	"os"
	"time"
)

// ---------------------------------------------------------------------------
// Reconciliation & storage status
// ---------------------------------------------------------------------------

// reconcile syncs our map with reality from `tart list`. Tart is authoritative
// for existence and running-state; we preserve our own timers (StartedAt/StopAt)
// for VMs that are genuinely still running. VMs with an op in flight are left
// alone so we don't clobber a transient starting/stopping state.
func (m *Manager) reconcile() {
	list, err := m.listTart()
	if err != nil {
		log.Printf("tart list failed (storage unmounted?): %v", err)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	seen := make(map[string]bool, len(list))
	for _, t := range list {
		seen[t.Name] = true
		vm := m.vms[t.Name]
		if vm == nil {
			vm = &VM{Name: t.Name}
			m.vms[t.Name] = vm
		}
		vm.Source = t.Source
		vm.Disk = t.Disk
		vm.Size = t.Size
		vm.Accessed = t.Accessed
		if m.busy[t.Name] {
			continue // mid-operation; leave transient state untouched
		}
		vm.State = t.State
		if t.State != "running" {
			// Clear only the live-session timers; keep last known IP / SSH
			// status / Info for reference after the VM stops.
			vm.StartedAt = time.Time{}
			vm.StopAt = time.Time{}
			vm.Headless = false
			vm.RunOverride = nil
		}
	}
	// Drop VMs that no longer exist in tart (and aren't mid-operation).
	for name := range m.vms {
		if !seen[name] && !m.busy[name] {
			delete(m.vms, name)
		}
	}
}

// ensureSharedDir creates the host_resources bind-mount folder if it's
// missing. `tart run` crashes outright if the --dir target doesn't exist, so
// this must run before any VM starts (called once at startup).
func (m *Manager) ensureSharedDir() {
	m.mu.Lock()
	dir := m.cfg.SharedDir
	m.mu.Unlock()
	if dir == "" {
		return
	}
	if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("shared dir %q missing and could not be created: %v", dir, err)
		return
	}
	log.Printf("created missing shared dir %q", dir)
}

// checkStorage updates the mounted flag and broadcasts on change.
func (m *Manager) checkStorage() {
	m.mu.Lock()
	p := m.cfg.VMStoragePath
	m.mu.Unlock()

	fi, err := os.Stat(p)
	ok := err == nil && fi.IsDir()

	m.mu.Lock()
	changed := ok != m.storageMounted
	m.storageMounted = ok
	m.mu.Unlock()
	if changed {
		m.broadcast()
	}
}

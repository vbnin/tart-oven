package server

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	tartoven "tart-oven"
	"tart-oven/internal/perf"
)

// ---------------------------------------------------------------------------
// SSE / broadcast
// ---------------------------------------------------------------------------

func (m *Manager) snapshot() stateSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()

	excluded := make(map[string]bool, len(m.cfg.Excluded))
	for _, e := range m.cfg.Excluded {
		excluded[strings.TrimSpace(e)] = true
	}

	vms := make([]*VM, 0, len(m.vms))
	for _, vm := range m.vms {
		// Make a copy with computed UI flags so we don't mutate stored state.
		v := *vm
		v.Template = strings.Contains(v.Name, templateMarker)
		v.Excluded = excluded[v.Name]
		v.Busy = m.busy[v.Name]
		v.SSHPassword = "" // write-only from clients; never echo the stored value
		vms = append(vms, &v)
	}
	sort.Slice(vms, func(i, j int) bool { return vms[i].Name < vms[j].Name })

	// Deep-copy tasks and logs: snapshotJSON marshals the returned snapshot
	// after m.mu is released, while appendTaskOutput/finishTask mutate live
	// task fields under the lock. Sharing the pointers would be a data race.
	tasks := make([]*Task, len(m.tasks))
	for i, t := range m.tasks {
		tc := *t
		tasks[i] = &tc
	}
	logs := append([]string(nil), m.logs...)
	batches := make([]createBatch, 0, len(m.batches))
	for _, b := range m.batches {
		batches = append(batches, *b)
	}

	var latestSample perf.PerformanceSample
	if n := len(m.performanceHistory); n > 0 {
		latestSample = m.performanceHistory[n-1]
	}

	return stateSnapshot{
		VMs:                  vms,
		Config:               newConfigView(m.cfg),
		StorageMounted:       m.storageMounted,
		StoragePath:          m.cfg.VMStoragePath,
		WithinHours:          !m.cfg.DailyEnabled || inDailyWindow(time.Now(), m.cfg.DailyStart, m.cfg.DailyStop),
		Now:                  time.Now(),
		Version:              tartoven.Version,
		TartJSON:             m.tartJSON,
		TartInstalled:        tartInstalledAt(m.cfg.TartAppPath),
		TartVersion:          m.tartVersion,
		Updates:              m.updatesLocked(tartInstalledAt(m.cfg.TartAppPath)),
		SupportsProvisioning: m.supportsProvisioning,
		Tasks:                tasks,
		CreateBatches:        batches,
		Performance:          latestSample,
		HostIP:               m.hostIP,
		Logs:                 logs,
	}
}

func (m *Manager) snapshotJSON() []byte {
	data, _ := json.Marshal(m.snapshot())
	return data
}

func (m *Manager) broadcast() {
	data := m.snapshotJSON()
	m.mu.Lock()
	for ch := range m.subs {
		select {
		case ch <- data:
		default: // slow consumer; drop this update for them
		}
	}
	m.mu.Unlock()
}

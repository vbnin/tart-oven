package server

import (
	"net/http"
	"time"

	"tart-oven/internal/perf"
)

func (m *Manager) updatePerformance(now time.Time) {
	m.mu.Lock()
	collector := m.performanceCollector
	vmStoragePath := m.cfg.VMStoragePath
	m.mu.Unlock()

	sample := collector.Collect(now, vmStoragePath)

	m.mu.Lock()
	m.performanceHistory = perf.AppendSample(m.performanceHistory, sample)
	m.mu.Unlock()
}

func (m *Manager) performanceSnapshot() perf.PerformanceSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()

	history := make([]perf.PerformanceSample, len(m.performanceHistory))
	copy(history, m.performanceHistory)
	snapshot := perf.PerformanceSnapshot{History: history}
	if len(history) > 0 {
		snapshot.Latest = history[len(history)-1]
	}
	return snapshot
}

func (m *Manager) handlePerformance(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, m.performanceSnapshot())
}

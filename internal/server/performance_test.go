package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"tart-oven/internal/perf"
	"testing"
	"time"
)

func TestManagerUpdatePerformanceAppendsSample(t *testing.T) {
	source := &fakePerformanceSource{cpu: 55.6, memoryUsed: 2 << 30, memoryTotal: 8 << 30, vmUsed: 20 << 30, vmTotal: 80 << 30}
	m := &Manager{cfg: Config{VMStoragePath: "/vm"}, performanceCollector: &perf.Collector{Source: source}}
	m.updatePerformance(time.Unix(100, 0))
	if len(m.performanceHistory) != 1 {
		t.Fatalf("history = %d", len(m.performanceHistory))
	}
	sample := m.performanceHistory[0]
	if sample.CPUPercent != 55.6 || sample.MemoryUsedBytes != 2<<30 || sample.VMDiskTotalBytes != 80<<30 {
		t.Fatalf("sample = %+v", sample)
	}
}

func TestManagerUpdatePerformanceDoesNotHoldManagerLockWhileCollecting(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	source := &fakePerformanceSource{cpuEntered: entered, cpuRelease: release}
	m := &Manager{cfg: Config{VMStoragePath: "/vm"}, performanceCollector: &perf.Collector{Source: source}}

	done := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
			<-done
		}
	}()
	go func() {
		m.updatePerformance(time.Unix(100, 0))
		close(done)
	}()
	<-entered

	if !m.mu.TryLock() {
		t.Fatal("Manager lock held while collecting performance metrics")
	}
	m.mu.Unlock()
	close(release)
	released = true
	<-done
}

func TestPerformanceSnapshotCopiesHistory(t *testing.T) {
	m := &Manager{performanceHistory: []perf.PerformanceSample{{UptimeSeconds: 1}}}
	s := m.performanceSnapshot()
	s.History[0].UptimeSeconds = 99
	if m.performanceHistory[0].UptimeSeconds != 1 {
		t.Fatal("history escaped Manager lock")
	}
}

func TestPerformanceEndpointReturnsOnlyPerformancePayload(t *testing.T) {
	m := &Manager{performanceHistory: []perf.PerformanceSample{{CPUPercent: 12, CPUAvailable: true}}}
	w := httptest.NewRecorder()
	m.handlePerformance(w, httptest.NewRequest(http.MethodGet, "/api/performance", nil))
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), `"vms"`) || !strings.Contains(w.Body.String(), `"history"`) {
		t.Fatalf("response = %d %s", w.Code, w.Body.String())
	}
}

func TestPerformanceEndpointRejectsPost(t *testing.T) {
	m := &Manager{}
	w := httptest.NewRecorder()
	m.handlePerformance(w, httptest.NewRequest(http.MethodPost, "/api/performance", nil))
	if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("response = %d Allow=%q", w.Code, w.Header().Get("Allow"))
	}
}

type fakePerformanceSource struct {
	cpu                             float64
	memoryUsed, memoryTotal         uint64
	pressure                        int
	systemUsed, systemTotal         uint64
	vmUsed, vmTotal                 uint64
	reads, writes                   uint64
	uptime                          uint64
	cpuErr, memoryErr, pressureErr  error
	systemDiskErr, vmDiskErr, ioErr error
	uptimeErr                       error
	cpuEntered                      chan struct{}
	cpuRelease                      chan struct{}
}

func (s *fakePerformanceSource) CPUPercent() (float64, error) {
	if s.cpuEntered != nil {
		close(s.cpuEntered)
		<-s.cpuRelease
	}
	return s.cpu, s.cpuErr
}

func (s *fakePerformanceSource) VirtualMemory() (uint64, uint64, error) {
	return s.memoryUsed, s.memoryTotal, s.memoryErr
}

func (s *fakePerformanceSource) MemoryPressure() (int, error) {
	return s.pressure, s.pressureErr
}

func (s *fakePerformanceSource) DiskUsage(path string) (uint64, uint64, error) {
	if path == "/" {
		return s.systemUsed, s.systemTotal, s.systemDiskErr
	}
	return s.vmUsed, s.vmTotal, s.vmDiskErr
}

func (s *fakePerformanceSource) DiskCounters() (uint64, uint64, error) {
	return s.reads, s.writes, s.ioErr
}

func (s *fakePerformanceSource) Uptime() (uint64, error) {
	return s.uptime, s.uptimeErr
}

func TestSnapshotExposesLatestPerformanceSample(t *testing.T) {
	m := &Manager{
		vms:  map[string]*VM{},
		busy: map[string]bool{},
		performanceHistory: []perf.PerformanceSample{
			{Timestamp: time.Unix(100, 0), CPUPercent: 12},
			{Timestamp: time.Unix(160, 0), CPUPercent: 34, MemoryPressure: "normal", CPUAvailable: true},
		},
	}
	snap := m.snapshot()
	if snap.Performance.CPUPercent != 34 || snap.Performance.MemoryPressure != "normal" {
		t.Fatalf("performance = %+v", snap.Performance)
	}
}

func TestSnapshotHandlesEmptyPerformanceHistory(t *testing.T) {
	m := &Manager{vms: map[string]*VM{}, busy: map[string]bool{}}
	if got := m.snapshot().Performance.Timestamp; !got.IsZero() {
		t.Fatalf("timestamp = %v, want zero", got)
	}
}

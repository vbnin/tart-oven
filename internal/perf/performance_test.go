package perf

import (
	"errors"
	"testing"
	"time"
)

func TestAppendPerformanceSampleKeepsNewest1440(t *testing.T) {
	var history []PerformanceSample
	for i := 0; i < PerformanceHistoryLimit+2; i++ {
		history = AppendSample(history, PerformanceSample{UptimeSeconds: uint64(i)})
	}
	if len(history) != PerformanceHistoryLimit {
		t.Fatalf("length = %d", len(history))
	}
	if history[0].UptimeSeconds != 2 || history[len(history)-1].UptimeSeconds != 1441 {
		t.Fatalf("wrong retained range: %d..%d", history[0].UptimeSeconds, history[len(history)-1].UptimeSeconds)
	}
}

func TestAppendPerformanceSampleDoesNotMutateInputAtCapacity(t *testing.T) {
	history := make([]PerformanceSample, PerformanceHistoryLimit, PerformanceHistoryLimit)
	history[0].UptimeSeconds = 7
	next := AppendSample(history, PerformanceSample{UptimeSeconds: 99})
	if history[0].UptimeSeconds != 7 || next[len(next)-1].UptimeSeconds != 99 {
		t.Fatal("input mutated")
	}
}

func TestPerformanceCollectorCollectsIndependentGroups(t *testing.T) {
	source := &fakePerformanceSource{cpu: 42.5, memoryUsed: 6 << 30, memoryTotal: 16 << 30, pressure: 2, systemUsed: 50 << 30, systemTotal: 100 << 30, vmUsed: 300 << 30, vmTotal: 500 << 30, reads: 1000, writes: 2000, uptime: 3600}
	c := Collector{Source: source}
	s := c.Collect(time.Unix(100, 0), "/Volumes/VMs")
	if s.CPUPercent != 42.5 || s.MemoryPressure != "warning" || s.VMDiskTotalBytes != 500<<30 || !s.UptimeAvailable {
		t.Fatalf("sample = %+v", s)
	}
	if s.DiskReadBytesPerSecond != 0 || s.DiskWriteBytesPerSecond != 0 {
		t.Fatal("first I/O rate is not zero")
	}
}

func TestPerformanceCollectorCalculatesDiskRatesAndCounterReset(t *testing.T) {
	source := &fakePerformanceSource{reads: 1000, writes: 2000}
	c := Collector{Source: source}
	c.Collect(time.Unix(100, 0), "/vm")
	source.reads, source.writes = 7000, 5000
	s := c.Collect(time.Unix(160, 0), "/vm")
	if s.DiskReadBytesPerSecond != 100 || s.DiskWriteBytesPerSecond != 50 {
		t.Fatalf("rates = %v/%v", s.DiskReadBytesPerSecond, s.DiskWriteBytesPerSecond)
	}
	source.reads, source.writes = 1, 1
	s = c.Collect(time.Unix(220, 0), "/vm")
	if s.DiskReadBytesPerSecond != 0 || s.DiskWriteBytesPerSecond != 0 {
		t.Fatal("counter reset produced a rate")
	}
}

func TestClampCPUPercent(t *testing.T) {
	tests := []struct {
		name    string
		percent float64
		want    float64
	}{
		{name: "below zero", percent: -0.1, want: 0},
		{name: "zero", percent: 0, want: 0},
		{name: "within range", percent: 42.5, want: 42.5},
		{name: "one hundred", percent: 100, want: 100},
		{name: "above one hundred", percent: 100.1, want: 100},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ClampCPUPercent(test.percent); got != test.want {
				t.Fatalf("ClampCPUPercent(%v) = %v, want %v", test.percent, got, test.want)
			}
		})
	}
}

func TestPerformanceCollectorTreatsNonPositiveElapsedTimeAsZeroRate(t *testing.T) {
	tests := []struct {
		name       string
		secondTime time.Time
	}{
		{name: "same timestamp", secondTime: time.Unix(100, 0)},
		{name: "timestamp moved backwards", secondTime: time.Unix(99, 0)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := &fakePerformanceSource{reads: 1000, writes: 2000}
			collector := Collector{Source: source}
			collector.Collect(time.Unix(100, 0), "/vm")
			source.reads, source.writes = 7000, 5000

			sample := collector.Collect(test.secondTime, "/vm")
			if !sample.DiskIOAvailable {
				t.Fatal("disk I/O marked unavailable")
			}
			if sample.DiskReadBytesPerSecond != 0 || sample.DiskWriteBytesPerSecond != 0 {
				t.Fatalf("rates = %v/%v, want 0/0", sample.DiskReadBytesPerSecond, sample.DiskWriteBytesPerSecond)
			}
		})
	}
}

func TestPerformanceCollectorMarksOnlyFailedGroupUnavailable(t *testing.T) {
	tests := []struct {
		name string
		fail func(*fakePerformanceSource)
	}{
		{name: "cpu", fail: func(source *fakePerformanceSource) { source.cpuErr = errors.New("cpu unavailable") }},
		{name: "memory", fail: func(source *fakePerformanceSource) { source.memoryErr = errors.New("memory unavailable") }},
		{name: "pressure", fail: func(source *fakePerformanceSource) { source.pressureErr = errors.New("pressure unavailable") }},
		{name: "system disk", fail: func(source *fakePerformanceSource) { source.systemDiskErr = errors.New("system disk unavailable") }},
		{name: "vm disk", fail: func(source *fakePerformanceSource) { source.vmDiskErr = errors.New("vm disk unavailable") }},
		{name: "disk io", fail: func(source *fakePerformanceSource) { source.ioErr = errors.New("disk io unavailable") }},
		{name: "uptime", fail: func(source *fakePerformanceSource) { source.uptimeErr = errors.New("uptime unavailable") }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := &fakePerformanceSource{
				cpu: 25, memoryUsed: 6 << 30, memoryTotal: 16 << 30, pressure: 1,
				systemUsed: 50 << 30, systemTotal: 100 << 30,
				vmUsed: 300 << 30, vmTotal: 500 << 30,
				reads: 1000, writes: 2000, uptime: 3600,
			}
			test.fail(source)

			sample := (&Collector{Source: source}).Collect(time.Unix(100, 0), "/vm")
			availability := map[string]bool{
				"cpu": sample.CPUAvailable, "memory": sample.MemoryAvailable,
				"pressure": sample.PressureAvailable, "system disk": sample.SystemDiskAvailable,
				"vm disk": sample.VMDiskAvailable, "disk io": sample.DiskIOAvailable,
				"uptime": sample.UptimeAvailable,
			}
			for group, got := range availability {
				want := group != test.name
				if got != want {
					t.Errorf("%s availability = %t, want %t when %s fails", group, got, want, test.name)
				}
			}
		})
	}
}

func TestPerformanceCollectorKeepsCPUWhenMemoryFails(t *testing.T) {
	source := &fakePerformanceSource{cpu: 25, memoryErr: errors.New("unavailable"), uptime: 99}
	s := (&Collector{Source: source}).Collect(time.Unix(100, 0), "/vm")
	if !s.CPUAvailable || s.MemoryAvailable || !s.UptimeAvailable {
		t.Fatalf("flags = %+v", s)
	}
}

func TestPressureNameUsesAppleKernelLevels(t *testing.T) {
	for level, want := range map[int]string{0: "normal", 1: "normal", 2: "warning", 4: "critical"} {
		if got := PressureName(level); got != want {
			t.Errorf("level %d = %q, want %q", level, got, want)
		}
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

func TestMaybeReleaseGoMemoryOnlyReleasesMeaningfulIdleHeap(t *testing.T) {
	tests := []struct {
		name         string
		idleBytes    uint64
		releaseBytes uint64
		wantFreed    bool
	}{
		{
			name:         "retained heap above threshold",
			idleBytes:    96 << 20,
			releaseBytes: 16 << 20,
			wantFreed:    true,
		},
		{
			name:         "small retained heap",
			idleBytes:    32 << 20,
			releaseBytes: 8 << 20,
			wantFreed:    false,
		},
		{
			name:         "idle heap already returned",
			idleBytes:    96 << 20,
			releaseBytes: 96 << 20,
			wantFreed:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			memory := &fakeGoMemory{idle: tt.idleBytes, released: tt.releaseBytes}
			gotBytes, gotFreed := MaybeReleaseGoMemory(memory)
			if gotFreed != tt.wantFreed {
				t.Fatalf("released = %v, want %v", gotFreed, tt.wantFreed)
			}
			wantBytes := tt.idleBytes - tt.releaseBytes
			if gotBytes != wantBytes {
				t.Fatalf("releasable bytes = %d, want %d", gotBytes, wantBytes)
			}
			wantCalls := 0
			if tt.wantFreed {
				wantCalls = 1
			}
			if memory.freeCalls != wantCalls {
				t.Fatalf("FreeOSMemory calls = %d, want %d", memory.freeCalls, wantCalls)
			}
		})
	}
}

func TestVMStartPressureGateOnlyBlocksAvailableCriticalState(t *testing.T) {
	tests := []struct {
		name   string
		sample PerformanceSample
		want   bool
	}{
		{"critical", PerformanceSample{MemoryPressure: "critical", PressureAvailable: true}, true},
		{"warning", PerformanceSample{MemoryPressure: "warning", PressureAvailable: true}, false},
		{"normal", PerformanceSample{MemoryPressure: "normal", PressureAvailable: true}, false},
		{"unavailable critical", PerformanceSample{MemoryPressure: "critical", PressureAvailable: false}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DeferVMStartForPressure(tt.sample); got != tt.want {
				t.Fatalf("DeferVMStartForPressure(%+v) = %v, want %v", tt.sample, got, tt.want)
			}
		})
	}
}

func TestVMStartPressureGateKeepsLastAvailableStateAcrossCollectionFailure(t *testing.T) {
	history := []PerformanceSample{
		{MemoryPressure: "critical", PressureAvailable: true},
		{MemoryPressure: "", PressureAvailable: false},
	}
	if !DeferVMStartForHistory(history) {
		t.Fatal("unavailable sample cleared last available critical pressure")
	}

	history = append(history, PerformanceSample{MemoryPressure: "warning", PressureAvailable: true})
	if DeferVMStartForHistory(history) {
		t.Fatal("available warning sample did not clear critical-pressure deferral")
	}
}

type fakeGoMemory struct {
	idle      uint64
	released  uint64
	freeCalls int
}

func (f *fakeGoMemory) HeapMemory() (uint64, uint64) { return f.idle, f.released }
func (f *fakeGoMemory) FreeOSMemory()                { f.freeCalls++ }

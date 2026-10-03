package main

import (
	"strings"
	"testing"
	"time"
)

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
			gotBytes, gotFreed := maybeReleaseGoMemory(memory)
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

type fakeGoMemory struct {
	idle      uint64
	released  uint64
	freeCalls int
}

func (f *fakeGoMemory) HeapMemory() (uint64, uint64) { return f.idle, f.released }
func (f *fakeGoMemory) FreeOSMemory()                { f.freeCalls++ }

func TestDoRunDefersStartDuringCriticalMemoryPressure(t *testing.T) {
	m := newTestManager(t)
	m.vms["base"] = &VM{Name: "base", State: "stopped"}
	m.performanceHistory = []PerformanceSample{{
		Timestamp:         time.Now(),
		MemoryPressure:    "critical",
		PressureAvailable: true,
	}}

	m.doRun("base", "manual", false)

	vm := m.vms["base"]
	if vm.State != "stopped" {
		t.Fatalf("state = %q, want stopped", vm.State)
	}
	if !strings.Contains(vm.LastError, "critical memory pressure") {
		t.Fatalf("LastError = %q, want critical-pressure explanation", vm.LastError)
	}
	if m.busy["base"] {
		t.Fatal("critical-pressure deferral left VM busy")
	}
	if len(m.history) != 0 {
		t.Fatalf("history entries = %d, want no attempted run", len(m.history))
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
			if got := deferVMStartForPressure(tt.sample); got != tt.want {
				t.Fatalf("deferVMStartForPressure(%+v) = %v, want %v", tt.sample, got, tt.want)
			}
		})
	}
}

func TestVMStartPressureGateKeepsLastAvailableStateAcrossCollectionFailure(t *testing.T) {
	history := []PerformanceSample{
		{MemoryPressure: "critical", PressureAvailable: true},
		{MemoryPressure: "", PressureAvailable: false},
	}
	if !deferVMStartForHistory(history) {
		t.Fatal("unavailable sample cleared last available critical pressure")
	}

	history = append(history, PerformanceSample{MemoryPressure: "warning", PressureAvailable: true})
	if deferVMStartForHistory(history) {
		t.Fatal("available warning sample did not clear critical-pressure deferral")
	}
}

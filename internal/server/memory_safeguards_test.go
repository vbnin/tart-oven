package server

import (
	"strings"
	"tart-oven/internal/perf"
	"testing"
	"time"
)

func TestDoRunDefersStartDuringCriticalMemoryPressure(t *testing.T) {
	m := newTestManager(t)
	m.vms["base"] = &VM{Name: "base", State: "stopped"}
	m.performanceHistory = []perf.PerformanceSample{{
		Timestamp:         time.Now(),
		MemoryPressure:    "critical",
		PressureAvailable: true,
	}}

	m.doRun("base", "manual", runOptions{})

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

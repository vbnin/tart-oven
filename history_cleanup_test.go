package main

import (
	"testing"
	"time"
)

func TestCloseStaleHistoryKeepsOnlyNewestOpenRunOfRunningVM(t *testing.T) {
	t0 := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	stoppedOld := &RunEvent{Name: "a", StartedAt: t0}
	closed := &RunEvent{Name: "a", StartedAt: t0.Add(time.Hour), StoppedAt: t0.Add(2 * time.Hour)}
	runningOld := &RunEvent{Name: "b", StartedAt: t0}
	runningNow := &RunEvent{Name: "b", StartedAt: t0.Add(3 * time.Hour)}
	deleted := &RunEvent{Name: "gone", StartedAt: t0}
	m := &Manager{
		vms: map[string]*VM{
			"a": {Name: "a", State: "stopped"},
			"b": {Name: "b", State: "running"},
		},
		history: []*RunEvent{stoppedOld, closed, runningOld, runningNow, deleted},
	}

	if n := m.closeStaleHistory(); n != 3 {
		t.Fatalf("closed %d runs, want 3", n)
	}
	for _, ev := range []*RunEvent{stoppedOld, runningOld, deleted} {
		if !ev.StopUnknown || !ev.StoppedAt.Equal(ev.StartedAt) {
			t.Errorf("%s run at %v not marked stop-unknown: %+v", ev.Name, ev.StartedAt, ev)
		}
	}
	if !runningNow.StoppedAt.IsZero() || runningNow.StopUnknown {
		t.Errorf("newest run of a running VM was closed: %+v", runningNow)
	}
	if closed.StopUnknown || !closed.StoppedAt.Equal(t0.Add(2*time.Hour)) {
		t.Errorf("an already-closed run was modified: %+v", closed)
	}
	if n := m.closeStaleHistory(); n != 0 {
		t.Errorf("second pass closed %d runs, want 0", n)
	}
}

package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunAddsSuspendableOnlyWhenWanted(t *testing.T) {
	cases := []struct {
		name         string
		cfg          bool
		extra        []string
		wasSuspended bool
		want         bool
	}{
		{"default off", false, nil, false, false},
		{"setting on", true, nil, false, true},
		{"asked in Run with arguments", false, []string{"--suspendable"}, false, true},
		{"a suspended VM always resumes with it", false, nil, true, true},
	}
	for _, c := range cases {
		if got := runWantsSuspendable(c.cfg, c.extra, c.wasSuspended); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSuspendOnlyWorksForVMsStartedSuspendable(t *testing.T) {
	m := newSnapshotTestManager(t)
	addVM(t, m, "web", "aa:aa:aa:aa:aa:aa", "d")

	m.vms["web"].State = "stopped"
	if err := m.doSuspend("web"); err == nil {
		t.Fatal("suspended a VM that is not running")
	}

	// Without --suspendable tart suspends "successfully" but the state can never
	// be restored, so Tart Oven must refuse.
	m.vms["web"].State = "running"
	err := m.doSuspend("web")
	if err == nil || !strings.Contains(err.Error(), "Suspendable") {
		t.Fatalf("a VM started without --suspendable was suspended: %v", err)
	}
	if _, serr := os.Stat(filepath.Join(vmDir(m, "web"), "state.vzvmsave")); serr == nil {
		t.Fatal("tart suspend was called anyway")
	}
}

func TestSuspendMarksTheVMSuspendedAndClosesItsRun(t *testing.T) {
	m := newSnapshotTestManager(t)
	addVM(t, m, "web", "aa:aa:aa:aa:aa:aa", "d")
	m.vms["web"].State = "running"
	m.vms["web"].Suspendable = true
	m.history = append(m.history, &RunEvent{Name: "web"})

	if err := m.doSuspend("web"); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if got := m.vms["web"].State; got != "suspended" {
		t.Fatalf("state = %q", got)
	}
	if !m.vms["web"].Suspendable {
		t.Fatal("a suspended VM must stay marked suspendable: it resumes with the flag")
	}
	if m.history[0].StoppedAt.IsZero() {
		t.Fatal("the run was not closed in the history")
	}
	if m.busy["web"] {
		t.Fatal("VM left busy")
	}
}

func TestReconcileKeepsSuspendableOnlyWhileSuspendedOrRunning(t *testing.T) {
	m := newSnapshotTestManager(t)
	addVM(t, m, "web", "aa:aa:aa:aa:aa:aa", "d")
	m.vms["web"].Suspendable = true

	// Suspended on disk: the marker stays.
	if err := os.WriteFile(filepath.Join(vmDir(m, "web"), "state.vzvmsave"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.reconcile()
	if !m.vms["web"].Suspendable || m.vms["web"].State != "suspended" {
		t.Fatalf("suspended VM: %+v", m.vms["web"])
	}

	// Fully stopped: the next start decides again.
	os.Remove(filepath.Join(vmDir(m, "web"), "state.vzvmsave"))
	m.reconcile()
	if m.vms["web"].Suspendable || m.vms["web"].State != "stopped" {
		t.Fatalf("stopped VM: %+v", m.vms["web"])
	}
}

func TestSchedulerLeavesSuspendedVMsAlone(t *testing.T) {
	vm := &VM{Name: "web", State: "suspended"}
	if eligibleForScheduler(vm, false, map[string]bool{}, false) {
		t.Fatal("the scheduler would cold-start a suspended VM, throwing its saved state away")
	}
}

func TestSnapshotOfASuspendedVMKeepsItsMemory(t *testing.T) {
	m := newSnapshotTestManager(t)
	addVM(t, m, "web", "c2:d1:8d:2d:41:9b", "disk")
	m.vms["web"].State = "suspended"
	if err := os.WriteFile(filepath.Join(vmDir(m, "web"), "state.vzvmsave"), []byte("ram"), 0o644); err != nil {
		t.Fatal(err)
	}

	name := takeSnapshot(t, m, "web", "with memory")
	if _, err := os.Stat(filepath.Join(vmDir(m, name), "state.vzvmsave")); err != nil {
		t.Fatal("the clone lost the saved memory")
	}
	if got := readFile(t, filepath.Join(vmDir(m, name), "config.json")); !strings.Contains(got, "c2:d1:8d:2d:41:9b") {
		t.Fatalf("MAC changed on a suspended clone: %s", got)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.vms["web"].Snapshots[0].Memory {
		t.Fatal("the snapshot is not marked as having memory")
	}
	if m.vms[name].State == "running" {
		t.Fatal("snapshot state")
	}
}

func TestSnapshotWithMemorySuspendsClonesAndResumes(t *testing.T) {
	m := newSnapshotTestManager(t)
	addVM(t, m, "web", "c2:d1:8d:2d:41:9b", "disk")
	m.vms["web"].State = "running"
	m.vms["web"].Suspendable = true
	resumed := make(chan string, 2)
	m.resumeHook = func(name string, _ runOptions) { resumed <- name }

	name, err := m.startSnapshot("web", "live", true)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-resumed:
		if got != "web" {
			t.Fatalf("resumed %q, want the original", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the original was not resumed after the copy")
	}
	waitTasks(t, m)
	if _, err := os.Stat(filepath.Join(vmDir(m, name), "state.vzvmsave")); err != nil {
		t.Fatal("the snapshot has no saved memory")
	}
	if got := readFile(t, filepath.Join(vmDir(m, name), "config.json")); !strings.Contains(got, "c2:d1:8d:2d:41:9b") {
		t.Fatalf("MAC: %s", got)
	}
}

func TestSnapshotWithMemoryNeedsASuspendableRunningVM(t *testing.T) {
	m := newSnapshotTestManager(t)
	addVM(t, m, "web", "aa:aa:aa:aa:aa:aa", "d")
	if _, err := m.startSnapshot("web", "", true); err == nil {
		t.Fatal("memory snapshot of a stopped VM")
	}
	m.vms["web"].State = "running"
	if _, err := m.startSnapshot("web", "", true); err == nil {
		t.Fatal("memory snapshot of a VM that was not started suspendable")
	}
}

// Stop on a suspended VM discards its saved memory. It must not go through the
// clean-shutdown path, which sees "not running" and would call that done.
func TestStopDiscardsASuspendedVMsSavedState(t *testing.T) {
	m := newSnapshotTestManager(t)
	addVM(t, m, "web", "aa:aa:aa:aa:aa:aa", "d")
	m.cfg.PrioritizeSSHShutdown = true
	m.vms["web"].State = "suspended"
	m.vms["web"].IP = "192.0.2.50" // a stale address must not tempt an SSH shutdown
	state := filepath.Join(vmDir(m, "web"), "state.vzvmsave")
	if err := os.WriteFile(state, []byte("ram"), 0o644); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() { m.doStop("web"); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("stopping a suspended VM hung (it tried an SSH shutdown)")
	}
	if _, err := os.Stat(state); err == nil {
		t.Fatal("the saved state is still there")
	}
	if got := m.vms["web"].State; got != "stopped" {
		t.Fatalf("state = %q", got)
	}
}

func TestSuspendableIsOnForNewConfigsOnly(t *testing.T) {
	if !defaultConfig().Suspendable {
		t.Fatal("a new config should start VMs suspendable")
	}

	// A config saved before the setting existed keeps VMs as they were.
	m := newTestManager(t)
	if err := os.WriteFile(m.statePath, []byte(`{"config":{"listen":"127.0.0.1:9000","intervalMinutes":5}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	m.load()
	if m.cfg.Suspendable {
		t.Fatal("an existing config without the setting must not change behaviour")
	}

	// And an explicit choice is kept.
	if err := os.WriteFile(m.statePath, []byte(`{"config":{"listen":"127.0.0.1:9000","suspendable":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	m.load()
	if !m.cfg.Suspendable {
		t.Fatal("an explicit true was lost")
	}
}

func TestConfigNoLongerKnowsNoAudio(t *testing.T) {
	b, _ := json.Marshal(defaultConfig())
	if strings.Contains(string(b), "noAudio") {
		t.Fatal("noAudio is gone from the config")
	}
}

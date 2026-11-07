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
		name             string
		vmSetting        bool
		trigger          string
		schedulerSuspend bool
		extra            []string
		wasSuspended     bool
		want             bool
	}{
		{"default off", false, "manual", false, nil, false, false},
		{"VM setting on, manual start", true, "manual", false, nil, false, true},
		{"VM setting on, scheduler start", true, "scheduler", false, nil, false, true},
		{"asked in Run with arguments", false, "manual", false, []string{"--suspendable"}, false, true},
		{"a suspended VM always resumes with it", false, "manual", false, nil, true, true},
		{"scheduler setting overrides a VM that is off", false, "scheduler", true, nil, false, true},
		{"scheduler setting leaves manual starts alone", false, "manual", true, nil, false, false},
		{"scheduler setting off does not turn a VM's own setting off", true, "scheduler", false, nil, false, true},
	}
	for _, c := range cases {
		if got := runWantsSuspendable(c.vmSetting, c.trigger, c.schedulerSuspend, c.extra, c.wasSuspended); got != c.want {
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

func TestSchedulerLeavesSuspendedVMsAloneUnlessItSuspendsThemItself(t *testing.T) {
	vm := &VM{Name: "web", State: "suspended"}
	if eligibleForScheduler(vm, false, map[string]bool{}, false, false) {
		t.Fatal("the scheduler would cold-start a suspended VM, throwing its saved state away")
	}
	if !eligibleForScheduler(vm, false, map[string]bool{}, false, true) {
		t.Fatal("with scheduler suspend on, a suspended VM must be resumed like a stopped one")
	}
	// The other exclusions still apply to a suspended VM.
	if eligibleForScheduler(vm, false, map[string]bool{"web": true}, false, true) {
		t.Fatal("an excluded VM was picked")
	}
	if eligibleForScheduler(vm, false, map[string]bool{}, true, true) {
		t.Fatal("a busy VM was picked")
	}
	if eligibleForScheduler(&VM{Name: "web", State: "running"}, false, map[string]bool{}, false, true) {
		t.Fatal("a running VM was picked")
	}
}

func TestSchedulerSuspendsOnlyVMsStartedSuspendable(t *testing.T) {
	suspendable := &VM{Name: "a", State: "running", Suspendable: true}
	plain := &VM{Name: "b", State: "running"}
	if !endsBySuspend(suspendable, true) {
		t.Fatal("a suspendable VM should be suspended when the setting is on")
	}
	if endsBySuspend(suspendable, false) {
		t.Fatal("suspended with the setting off")
	}
	if endsBySuspend(plain, true) {
		t.Fatal("a VM not started suspendable cannot be restored, so it must be stopped")
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

func TestSuspendIsOffForNewVMsAndTheSchedulerByDefault(t *testing.T) {
	if defaultConfig().SchedulerSuspend {
		t.Fatal("scheduler suspend must start off")
	}
	if (&VM{}).StartSuspendable {
		t.Fatal("a new VM must not start suspendable")
	}
}

// The global "Start VMs suspendable" setting became a per-VM one. A state file
// saved with the global on must keep every existing VM starting suspendable.
func TestOldGlobalSuspendableMovesToEveryExistingVM(t *testing.T) {
	cases := []struct {
		name, state string
		want        bool
	}{
		{"global on", `{"config":{"listen":"127.0.0.1:9000","suspendable":true},"vms":{"a":{"name":"a"},"b":{"name":"b"}}}`, true},
		{"global off", `{"config":{"listen":"127.0.0.1:9000","suspendable":false},"vms":{"a":{"name":"a"},"b":{"name":"b"}}}`, false},
		{"no key (older state)", `{"config":{"listen":"127.0.0.1:9000"},"vms":{"a":{"name":"a"},"b":{"name":"b"}}}`, false},
	}
	for _, c := range cases {
		m := newTestManager(t)
		if err := os.WriteFile(m.statePath, []byte(c.state), 0o600); err != nil {
			t.Fatal(err)
		}
		m.load()
		for _, n := range []string{"a", "b"} {
			if m.vms[n] == nil || m.vms[n].StartSuspendable != c.want {
				t.Errorf("%s: VM %s StartSuspendable = %v, want %v", c.name, n, m.vms[n] != nil && m.vms[n].StartSuspendable, c.want)
			}
		}
		// The old key is not written back, so the migration runs only once.
		m.mu.Lock()
		m.save()
		m.mu.Unlock()
		saved, _ := os.ReadFile(m.statePath)
		if strings.Contains(string(saved), `"suspendable":`) && strings.Contains(string(saved), `"config":{`) {
			var p struct {
				Config map[string]any `json:"config"`
			}
			_ = json.Unmarshal(saved, &p)
			if _, still := p.Config["suspendable"]; still {
				t.Errorf("%s: the old config key was saved again", c.name)
			}
		}
	}
}

func TestConfigNoLongerKnowsNoAudio(t *testing.T) {
	b, _ := json.Marshal(defaultConfig())
	if strings.Contains(string(b), "noAudio") {
		t.Fatal("noAudio is gone from the config")
	}
}

// A snapshot with memory suspends the original, copies it, then resumes it.
// While it is copied the original is "suspended" and not busy, so the scheduler
// (which resumes suspended VMs when it suspends them itself) must leave it be.
func TestSchedulerLeavesAVMBeingSnapshottedWithMemoryAlone(t *testing.T) {
	m := newSnapshotTestManager(t)
	addVM(t, m, "web", "c2:d1:8d:2d:41:9b", "disk")
	m.vms["web"].State = "running"
	m.vms["web"].Suspendable = true
	m.cfg.SchedulerSuspend = true
	m.cfg.Paused = false
	m.cfg.DailyEnabled = false
	m.storageMounted = true
	held := make(chan bool, 1)
	release := make(chan struct{})
	m.resumeHook = func(name string, _ runOptions) {
		// The copy is done and the original is suspended: is it still held?
		m.mu.Lock()
		vm := m.vms[name]
		ok := vm != nil && vm.State == "suspended" && !m.busy[name] && m.snapshotHold[name] &&
			!eligibleForScheduler(vm, false, nil, m.busy[name] || m.snapshotHold[name], true)
		m.mu.Unlock()
		held <- ok
		<-release
	}
	if _, err := m.startSnapshot("web", "live", true); err != nil {
		t.Fatal(err)
	}
	select {
	case ok := <-held:
		if !ok {
			t.Error("the original was eligible for the scheduler while its snapshot was being made")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no resume")
	}
	close(release)
	waitTasks(t, m)
	deadline := time.Now().Add(5 * time.Second)
	for {
		m.mu.Lock()
		h := m.snapshotHold["web"]
		m.mu.Unlock()
		if !h {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the hold was never released")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

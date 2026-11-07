package server

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// tickFakeTart knows what the scheduler tick needs. A VM is "running" while its
// directory holds a `running` marker, "suspended" once it holds a saved state.
// Dropping a file named suspend-fails in the VM storage makes `tart suspend` fail.
const tickFakeTart = `#!/bin/sh
V="$TART_HOME/vms"
case "$1" in
  list)
    printf '['; sep=''
    for d in "$V"/*/; do
      [ -d "$d" ] || continue
      st=stopped
      [ -f "$d/state.vzvmsave" ] && st=suspended
      [ -f "$d/running" ] && st=running
      printf '%s{"Source":"local","Name":"%s","Disk":50,"Size":10,"Accessed":"now","State":"%s","Running":false}' "$sep" "$(basename "$d")" "$st"
      sep=','
    done
    printf ']\n';;
  suspend)
    [ -f "$TART_HOME/suspend-fails" ] && { echo "boom" >&2; exit 1; }
    rm -f "$V/$2/running"; echo ram > "$V/$2/state.vzvmsave";;
  stop) rm -f "$V/$2/running" "$V/$2/state.vzvmsave";;
  *) echo "fake tart: unsupported: $*" >&2; exit 1;;
esac
`

// newTickManager is a manager whose scheduler is on and whose daily window and
// run windows have all expired.
func newTickManager(t *testing.T, schedulerSuspend bool) *Manager {
	t.Helper()
	m := newSnapshotTestManager(t)
	if err := os.WriteFile(m.cfg.TartAppPath, []byte(tickFakeTart), 0o755); err != nil {
		t.Fatal(err)
	}
	m.cfg.Paused = false
	m.cfg.DailyEnabled = false
	m.cfg.SchedulerSuspend = schedulerSuspend
	return m
}

// addRunning adds a VM that is running, started suspendable or not, whose run
// window has already ended.
func addRunning(t *testing.T, m *Manager, name string, suspendable bool) {
	t.Helper()
	addVM(t, m, name, "aa:aa:aa:aa:aa:aa", "d")
	if err := os.WriteFile(filepath.Join(vmDir(m, name), "running"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	vm := m.vms[name]
	vm.State = "running"
	vm.Suspendable = suspendable
	vm.StartedAt = time.Now().Add(-3 * time.Hour)
	vm.StopAt = time.Now().Add(-time.Minute)
	m.mu.Unlock()
}

func stateOf(m *Manager, name string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.vms[name].State
}

func TestTickSuspendsExpiredSuspendableVMsAndStopsTheOthers(t *testing.T) {
	m := newTickManager(t, true)
	addRunning(t, m, "a", true)
	addRunning(t, m, "b", false) // not started suspendable: its state could never be restored
	m.schedulerRunHook = func(string) {}

	m.tick()

	if got := stateOf(m, "a"); got != "suspended" {
		t.Errorf("a (suspendable) = %q, want suspended", got)
	}
	if got := stateOf(m, "b"); got != "stopped" {
		t.Errorf("b (not suspendable) = %q, want stopped", got)
	}
}

func TestTickStopsEverythingWhenSchedulerSuspendIsOff(t *testing.T) {
	m := newTickManager(t, false)
	addRunning(t, m, "a", true)
	m.schedulerRunHook = func(string) {}

	m.tick()

	if got := stateOf(m, "a"); got != "stopped" {
		t.Fatalf("a = %q, want stopped: the VM's own setting must not make the scheduler suspend", got)
	}
}

func TestTickResumesSuspendedVMsOnlyWhenSchedulerSuspendIsOn(t *testing.T) {
	for _, on := range []bool{true, false} {
		m := newTickManager(t, on)
		addVM(t, m, "web", "aa:aa:aa:aa:aa:aa", "d")
		if err := os.WriteFile(filepath.Join(vmDir(m, "web"), "state.vzvmsave"), []byte("ram"), 0o644); err != nil {
			t.Fatal(err)
		}
		var started []string
		m.schedulerRunHook = func(name string) { started = append(started, name) }

		m.tick()

		if on && (len(started) != 1 || started[0] != "web") {
			t.Errorf("scheduler suspend on: started %v, want [web]", started)
		}
		if !on && len(started) != 0 {
			t.Errorf("scheduler suspend off: started %v, a parked VM must be left alone", started)
		}
	}
}

// A snapshot with memory suspends the original, copies it, then resumes it. In
// between the original is "suspended" and not busy: the tick must leave it be.
func TestTickLeavesAVMHeldForASnapshotAlone(t *testing.T) {
	m := newTickManager(t, true)
	addVM(t, m, "web", "aa:aa:aa:aa:aa:aa", "d")
	if err := os.WriteFile(filepath.Join(vmDir(m, "web"), "state.vzvmsave"), []byte("ram"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.snapshotHold = map[string]bool{"web": true}
	m.schedulerRunHook = func(name string) { t.Errorf("the scheduler started %s while its snapshot was being copied", name) }

	m.tick()
}

// tart suspend failing is the one case where an expired VM is stopped instead.
func TestSuspendOrStopStopsTheVMWhenTartSuspendFails(t *testing.T) {
	m := newTickManager(t, true)
	addRunning(t, m, "a", true)
	if err := os.WriteFile(filepath.Join(m.cfg.VMStoragePath, "suspend-fails"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	m.suspendOrStop("a")

	if got := stateOf(m, "a"); got != "stopped" {
		t.Fatalf("a = %q, want stopped: an expired VM must not keep a host slot because the suspend failed", got)
	}
}

// The tick decides under the lock but acts later, one VM at a time, and a
// suspend can take minutes. A VM that someone suspended meanwhile must keep its
// saved memory: doStop on it would run `tart stop` and throw the memory away.
func TestSuspendOrStopLeavesAVMThatIsNoLongerRunning(t *testing.T) {
	m := newTickManager(t, true)
	addRunning(t, m, "a", true)
	// Suspended by the user after the tick built its list.
	if err := os.Remove(filepath.Join(vmDir(m, "a"), "running")); err != nil {
		t.Fatal(err)
	}
	saved := filepath.Join(vmDir(m, "a"), "state.vzvmsave")
	if err := os.WriteFile(saved, []byte("ram"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.vms["a"].State = "suspended"
	m.mu.Unlock()

	m.suspendOrStop("a")

	if _, err := os.Stat(saved); err != nil {
		t.Fatal("the saved memory of a suspended VM was thrown away")
	}
	if got := stateOf(m, "a"); got != "suspended" {
		t.Fatalf("a = %q, want suspended", got)
	}
}

func TestSuspendOrStopLeavesABusyOrHeldVMAlone(t *testing.T) {
	for _, how := range []string{"busy", "held"} {
		m := newTickManager(t, true)
		addRunning(t, m, "a", true)
		if how == "busy" {
			m.busy["a"] = true
		} else {
			m.snapshotHold = map[string]bool{"a": true}
		}
		m.suspendOrStop("a")
		if got := stateOf(m, "a"); got != "running" {
			t.Errorf("%s: a = %q, want it untouched (running)", how, got)
		}
	}
}

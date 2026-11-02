package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// snapshotFakeTart is fakeTart plus what snapshots need: a clone copies the
// directory but gets a new MAC (as the real tart does), `set --mac` writes one
// back, and `rename` moves the directory.
const snapshotFakeTart = `#!/bin/sh
V="$TART_HOME/vms"
case "$1" in
  list)
    printf '['; sep=''
    for d in "$V"/*/; do
      [ -d "$d" ] || continue
      st=stopped; [ -f "$d/state.vzvmsave" ] && st=suspended
      printf '%s{"Source":"local","Name":"%s","Disk":50,"Size":10,"Accessed":"now","State":"%s","Running":false}' "$sep" "$(basename "$d")" "$st"
      sep=','
    done
    printf ']\n';;
  suspend) echo "state" > "$V/$2/state.vzvmsave";;
  stop) rm -f "$V/$2/state.vzvmsave";;
  clone)
    [ -d "$V/$2" ] || { echo "no such VM $2" >&2; exit 1; }
    cp -R "$V/$2" "$V/$3"
    [ -f "$V/$2/state.vzvmsave" ] || printf '{"macAddress":"ff:ff:ff:00:00:01"}' > "$V/$3/config.json";;
  rename) mv "$V/$2" "$V/$3";;
  delete) rm -rf "$V/$2";;
  set)
    name=$2; shift 2
    while [ $# -gt 0 ]; do
      case "$1" in --mac) printf '{"macAddress":"%s"}' "$2" > "$V/$name/config.json"; shift;; esac
      shift
    done;;
  *) echo "fake tart: unsupported: $*" >&2; exit 1;;
esac
`

func newSnapshotTestManager(t *testing.T) *Manager {
	t.Helper()
	m := newTestManager(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "tart")
	if err := os.WriteFile(bin, []byte(snapshotFakeTart), 0o755); err != nil {
		t.Fatal(err)
	}
	m.cfg.TartAppPath = bin
	m.cfg.VMStoragePath = filepath.Join(dir, "home")
	m.cfg.SharedDir = filepath.Join(dir, "shared")
	m.tartJSON = true
	return m
}

// addVM creates a stopped VM on disk (config.json with mac, plus a disk marker)
// and in the manager.
func addVM(t *testing.T, m *Manager, name, mac, disk string) {
	t.Helper()
	dir := filepath.Join(m.cfg.VMStoragePath, "vms", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"macAddress":"`+mac+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "disk.img"), []byte(disk), 0o644); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.vms[name] = &VM{Name: name, State: "stopped"}
	m.mu.Unlock()
}

func vmDir(m *Manager, name string) string {
	return filepath.Join(m.cfg.VMStoragePath, "vms", name)
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func waitTasks(t *testing.T, m *Manager) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		running := 0
		for _, task := range m.tasks {
			if task.Status == "running" {
				running++
			}
		}
		busy := len(m.busy)
		m.mu.Unlock()
		if running == 0 && busy == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("tasks did not finish")
}

func takeSnapshot(t *testing.T, m *Manager, parent, comment string) string {
	t.Helper()
	name, err := m.startSnapshot(parent, comment, false)
	if err != nil {
		t.Fatal(err)
	}
	waitTasks(t, m)
	return name
}

func TestSnapshotKeepsMACAndSerialAndIsHidden(t *testing.T) {
	m := newSnapshotTestManager(t)
	addVM(t, m, "web", "c2:d1:8d:2d:41:9b", "disk-v1")
	m.vms["web"].SSHUser = "builder"
	m.vms["web"].Tags = []string{"demo"}

	name := takeSnapshot(t, m, "web", "before the upgrade")
	if name != "web-Snapshot-1" {
		t.Fatalf("snapshot name = %q", name)
	}
	if got := readFile(t, filepath.Join(vmDir(m, name), "config.json")); !strings.Contains(got, "c2:d1:8d:2d:41:9b") {
		t.Fatalf("snapshot MAC was not set back to the original's: %s", got)
	}
	if got := readFile(t, filepath.Join(vmDir(m, name), "disk.img")); got != "disk-v1" {
		t.Fatalf("snapshot disk = %q", got)
	}

	m.mu.Lock()
	parent, snap := m.vms["web"], m.vms[name]
	if len(parent.Snapshots) != 1 || parent.Snapshots[0].Name != name || parent.Snapshots[0].Comment != "before the upgrade" || parent.Snapshots[0].CreatedAt.IsZero() {
		t.Fatalf("parent snapshots = %+v", parent.Snapshots)
	}
	if snap == nil || snap.SnapshotOf != "web" || snap.SSHUser != "builder" || len(snap.Tags) != 0 {
		t.Fatalf("snapshot record = %+v", snap)
	}
	m.mu.Unlock()

	// A snapshot is not a regular VM: the dashboard list leaves it out and
	// reports it under its original instead.
	view := m.snapshot()
	if len(view.VMs) != 1 || view.VMs[0].Name != "web" || len(view.VMs[0].Snapshots) != 1 || view.VMs[0].Snapshots[0].State != "stopped" {
		t.Fatalf("snapshot view = %+v", view.VMs)
	}

	// reconcile must keep the link even though tart lists the snapshot as a VM.
	m.reconcile()
	if m.vms[name].SnapshotOf != "web" || len(m.vms["web"].Snapshots) != 1 {
		t.Fatal("reconcile lost the snapshot link")
	}
}

func TestSnapshotNumbersFillGaps(t *testing.T) {
	m := newSnapshotTestManager(t)
	addVM(t, m, "web", "aa:aa:aa:aa:aa:aa", "d")
	a := takeSnapshot(t, m, "web", "")
	b := takeSnapshot(t, m, "web", "")
	if a != "web-Snapshot-1" || b != "web-Snapshot-2" {
		t.Fatalf("names %q %q", a, b)
	}
	if err := m.deleteSnapshot(a); err != nil {
		t.Fatal(err)
	}
	if c := takeSnapshot(t, m, "web", ""); c != "web-Snapshot-1" {
		t.Fatalf("freed number not reused: %q", c)
	}
}

func TestSnapshotRefusals(t *testing.T) {
	m := newSnapshotTestManager(t)
	addVM(t, m, "web", "aa:aa:aa:aa:aa:aa", "d")
	if _, err := m.startSnapshot("ghost", "", false); err == nil {
		t.Fatal("snapshot of an unknown VM was accepted")
	}
	snap := takeSnapshot(t, m, "web", "")
	if _, err := m.startSnapshot(snap, "", false); err == nil {
		t.Fatal("snapshot of a snapshot was accepted")
	}
	// Taking a snapshot of a running VM is allowed.
	m.vms["web"].State = "running"
	if _, err := m.startSnapshot("web", "live", false); err != nil {
		t.Fatalf("snapshot of a running VM: %v", err)
	}
	waitTasks(t, m)
}

func TestFamilyRunsOneAtATime(t *testing.T) {
	m := newSnapshotTestManager(t)
	addVM(t, m, "web", "aa:aa:aa:aa:aa:aa", "d")
	addVM(t, m, "other", "bb:bb:bb:bb:bb:bb", "d")
	s1 := takeSnapshot(t, m, "web", "")
	s2 := takeSnapshot(t, m, "web", "")

	m.mu.Lock()
	defer m.mu.Unlock()
	if got := m.familyConflictLocked(s1); got != "" {
		t.Fatalf("nothing runs, conflict = %q", got)
	}
	m.vms["web"].State = "running"
	if got := m.familyConflictLocked(s1); got != "web" {
		t.Fatalf("snapshot while the original runs: conflict = %q", got)
	}
	m.vms["web"].State = "stopped"
	m.vms[s1].State = "starting"
	if got := m.familyConflictLocked(s2); got != s1 {
		t.Fatalf("second snapshot: conflict = %q", got)
	}
	if got := m.familyConflictLocked("web"); got != s1 {
		t.Fatalf("original while a snapshot runs: conflict = %q", got)
	}
	// Other VMs have their own MAC and are unaffected.
	if got := m.familyConflictLocked("other"); got != "" {
		t.Fatalf("unrelated VM: conflict = %q", got)
	}
}

func TestDoRunRefusesWhileFamilyRuns(t *testing.T) {
	m := newSnapshotTestManager(t)
	addVM(t, m, "web", "aa:aa:aa:aa:aa:aa", "d")
	snap := takeSnapshot(t, m, "web", "")
	m.vms["web"].State = "running"

	m.doRun(snap, "manual", runOptions{})

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.vms[snap].State != "stopped" || !strings.Contains(m.vms[snap].LastError, "web") {
		t.Fatalf("snapshot state=%q lastError=%q", m.vms[snap].State, m.vms[snap].LastError)
	}
}

func TestSchedulerNeverPicksSnapshots(t *testing.T) {
	vm := &VM{Name: "web-Snapshot-1", State: "stopped", SnapshotOf: "web"}
	if eligibleForScheduler(vm, false, map[string]bool{}, false) {
		t.Fatal("scheduler would run a snapshot")
	}
}

func TestRestoreReplacesOriginalWithSnapshot(t *testing.T) {
	m := newSnapshotTestManager(t)
	addVM(t, m, "web", "c2:d1:8d:2d:41:9b", "disk-v1")
	m.vms["web"].Tags = []string{"demo"}
	m.vms["web"].Notes = "my notes"
	snap := takeSnapshot(t, m, "web", "v1")
	other := takeSnapshot(t, m, "web", "also v1")
	// The original moves on after the snapshot.
	if err := os.WriteFile(filepath.Join(vmDir(m, "web"), "disk.img"), []byte("disk-v2"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := m.restoreSnapshot(snap); err != nil {
		t.Fatal(err)
	}
	waitTasks(t, m)

	if got := readFile(t, filepath.Join(vmDir(m, "web"), "disk.img")); got != "disk-v1" {
		t.Fatalf("restored disk = %q", got)
	}
	if got := readFile(t, filepath.Join(vmDir(m, "web"), "config.json")); !strings.Contains(got, "c2:d1:8d:2d:41:9b") {
		t.Fatalf("restored VM changed MAC: %s", got)
	}
	if _, err := os.Stat(vmDir(m, snap)); !os.IsNotExist(err) {
		t.Fatal("the restored snapshot should be gone (it is the main VM now)")
	}
	entries, _ := os.ReadDir(filepath.Join(m.cfg.VMStoragePath, "vms"))
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 2 {
		t.Fatalf("VMs left on disk: %v", names)
	}
	m.reconcile()
	m.mu.Lock()
	defer m.mu.Unlock()
	web := m.vms["web"]
	if web == nil || web.Notes != "my notes" || len(web.Tags) != 1 {
		t.Fatalf("original's settings were lost: %+v", web)
	}
	if len(web.Snapshots) != 1 || web.Snapshots[0].Name != other {
		t.Fatalf("snapshots after restore = %+v", web.Snapshots)
	}
	if m.vms[other].SnapshotOf != "web" || m.vms[snap] != nil {
		t.Fatal("snapshot records are inconsistent after restore")
	}
}

func TestRestoreRefusedWhileRunning(t *testing.T) {
	m := newSnapshotTestManager(t)
	addVM(t, m, "web", "aa:aa:aa:aa:aa:aa", "d")
	snap := takeSnapshot(t, m, "web", "")
	m.vms["web"].State = "running"
	if err := m.restoreSnapshot(snap); err == nil {
		t.Fatal("restore ran while the original was running")
	}
	m.vms["web"].State = "stopped"
	m.vms[snap].State = "running"
	if err := m.restoreSnapshot(snap); err == nil {
		t.Fatal("restore ran while the snapshot was running")
	}
}

func TestDeleteVMWithSnapshots(t *testing.T) {
	t.Run("must choose", func(t *testing.T) {
		m := newSnapshotTestManager(t)
		addVM(t, m, "web", "aa:aa:aa:aa:aa:aa", "d")
		takeSnapshot(t, m, "web", "")
		if _, err := m.deleteVM("web", "", ""); err == nil {
			t.Fatal("deleted a VM with snapshots without being told what to do with them")
		}
		if _, err := os.Stat(vmDir(m, "web")); err != nil {
			t.Fatal("VM was deleted anyway")
		}
	})
	t.Run("delete snapshots too", func(t *testing.T) {
		m := newSnapshotTestManager(t)
		addVM(t, m, "web", "aa:aa:aa:aa:aa:aa", "d")
		takeSnapshot(t, m, "web", "")
		takeSnapshot(t, m, "web", "")
		if _, err := m.deleteVM("web", "delete", ""); err != nil {
			t.Fatal(err)
		}
		entries, _ := os.ReadDir(filepath.Join(m.cfg.VMStoragePath, "vms"))
		if len(entries) != 0 {
			t.Fatalf("%d VMs left", len(entries))
		}
		if len(m.vms) != 0 {
			t.Fatalf("records left: %v", m.vms)
		}
	})
	t.Run("use a snapshot as the main VM", func(t *testing.T) {
		m := newSnapshotTestManager(t)
		addVM(t, m, "web", "aa:aa:aa:aa:aa:aa", "disk-v1")
		snap := takeSnapshot(t, m, "web", "")
		if err := os.WriteFile(filepath.Join(vmDir(m, "web"), "disk.img"), []byte("disk-v2"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := m.deleteVM("web", "promote", snap); err != nil {
			t.Fatal(err)
		}
		waitTasks(t, m)
		if got := readFile(t, filepath.Join(vmDir(m, "web"), "disk.img")); got != "disk-v1" {
			t.Fatalf("main VM disk = %q", got)
		}
		if _, err := os.Stat(vmDir(m, snap)); !os.IsNotExist(err) {
			t.Fatal("promoted snapshot still exists under its old name")
		}
	})
	t.Run("running snapshot blocks", func(t *testing.T) {
		m := newSnapshotTestManager(t)
		addVM(t, m, "web", "aa:aa:aa:aa:aa:aa", "d")
		snap := takeSnapshot(t, m, "web", "")
		m.vms[snap].State = "running"
		if _, err := m.deleteVM("web", "delete", ""); err == nil {
			t.Fatal("deleted a VM whose snapshot is running")
		}
	})
}

func TestRenameCarriesSnapshots(t *testing.T) {
	m := newSnapshotTestManager(t)
	addVM(t, m, "web", "aa:aa:aa:aa:aa:aa", "d")
	snap := takeSnapshot(t, m, "web", "keep me")

	if out, err := m.tartCmd(m.cfg.VMStoragePath, "rename", "web", "api").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	m.mu.Lock()
	moved := *m.vms["web"]
	moved.Name = "api"
	m.vms["api"] = &moved
	delete(m.vms, "web")
	m.mu.Unlock()
	m.followRename("web", "api")

	m.mu.Lock()
	defer m.mu.Unlock()
	api := m.vms["api"]
	if len(api.Snapshots) != 1 || api.Snapshots[0].Name != "api-Snapshot-1" || api.Snapshots[0].Comment != "keep me" {
		t.Fatalf("snapshots after rename = %+v", api.Snapshots)
	}
	if m.vms["api-Snapshot-1"] == nil || m.vms["api-Snapshot-1"].SnapshotOf != "api" || m.vms[snap] != nil {
		t.Fatalf("snapshot records after rename: %v", m.vms)
	}
	if _, err := os.Stat(vmDir(m, "api-Snapshot-1")); err != nil {
		t.Fatal("snapshot VM was not renamed in tart")
	}
}

func TestReconcileReleasesOrphansAndForgetsMissingSnapshots(t *testing.T) {
	m := newSnapshotTestManager(t)
	addVM(t, m, "web", "aa:aa:aa:aa:aa:aa", "d")
	addVM(t, m, "db", "bb:bb:bb:bb:bb:bb", "d")
	s1 := takeSnapshot(t, m, "web", "")
	s2 := takeSnapshot(t, m, "db", "")

	// The original is deleted behind our back: its snapshot becomes a regular VM.
	os.RemoveAll(vmDir(m, "web"))
	// A snapshot is deleted behind our back: the entry goes away.
	os.RemoveAll(vmDir(m, s2))
	m.reconcile()

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.vms[s1] == nil || m.vms[s1].SnapshotOf != "" {
		t.Fatalf("orphaned snapshot = %+v", m.vms[s1])
	}
	if len(m.vms["db"].Snapshots) != 0 {
		t.Fatalf("stale snapshot entry kept: %+v", m.vms["db"].Snapshots)
	}
}

func TestSnapshotCommentCanBeEdited(t *testing.T) {
	m := newSnapshotTestManager(t)
	addVM(t, m, "web", "aa:aa:aa:aa:aa:aa", "d")
	snap := takeSnapshot(t, m, "web", "first")
	if err := m.setSnapshotComment(snap, "  second  "); err != nil {
		t.Fatal(err)
	}
	if got := m.vms["web"].Snapshots[0].Comment; got != "second" {
		t.Fatalf("comment = %q", got)
	}
	if err := m.setSnapshotComment("nope", "x"); err == nil {
		t.Fatal("comment on an unknown snapshot was accepted")
	}
}

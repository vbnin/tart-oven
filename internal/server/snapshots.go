package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// VM snapshots
//
// A snapshot is a full copy of a VM: a tart VM of its own (named
// "<original>-Snapshot-<n>") with the original's serial number and MAC address.
// APFS makes the copy nearly free until it diverges. Snapshots are not regular
// VMs as far as Tart Oven is concerned: they are recorded under their original
// (VM.Snapshots), flagged on their own record (VM.SnapshotOf), left out of the
// dashboard list and never started by the scheduler.
//
// Because a snapshot shares its original's MAC address, a VM and its snapshots
// (a "family") never run at the same time.
// ---------------------------------------------------------------------------

const snapshotInfix = "-Snapshot-"

const maxSnapshotCommentLen = 500

// Snapshot is the record of one snapshot, kept on the original VM.
type Snapshot struct {
	Name      string    `json:"name"` // the tart VM holding the copy
	CreatedAt time.Time `json:"createdAt"`
	Comment   string    `json:"comment,omitempty"`
	Memory    bool      `json:"memory,omitempty"` // taken from a suspended VM: running it resumes exactly where the original was

	// Computed for the UI in snapshot() (not stored).
	State string `json:"state,omitempty"`
	IP    string `json:"ip,omitempty"`
	Busy  bool   `json:"busy,omitempty"`
}

// vmConfigMAC reads the MAC address tart stored for a VM.
func vmConfigMAC(home, name string) (string, error) {
	if name == "" || filepath.Base(name) != name || name == "." || name == ".." {
		return "", fmt.Errorf("invalid VM name %q", name)
	}
	data, err := os.ReadFile(filepath.Join(home, "vms", name, "config.json"))
	if err != nil {
		data, err = os.ReadFile(filepath.Join(home, name, "config.json"))
		if err != nil {
			return "", err
		}
	}
	var c struct {
		MACAddress string `json:"macAddress"`
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return "", err
	}
	if strings.TrimSpace(c.MACAddress) == "" {
		return "", errors.New("config.json has no macAddress")
	}
	return c.MACAddress, nil
}

// familyRootLocked is the original VM a name belongs to: the name itself unless
// it is a snapshot. Caller holds m.mu.
func (m *Manager) familyRootLocked(name string) string {
	if vm := m.vms[name]; vm != nil && vm.SnapshotOf != "" {
		return vm.SnapshotOf
	}
	return name
}

// familyConflictLocked returns the name of another running (or starting)
// member of name's family, or "" when name is free to start. Caller holds m.mu.
func (m *Manager) familyConflictLocked(name string) string {
	root := m.familyRootLocked(name)
	var found []string
	for n, v := range m.vms {
		if n == name || (n != root && v.SnapshotOf != root) {
			continue
		}
		if v.State == "running" || v.State == "starting" {
			found = append(found, n)
		}
	}
	sort.Strings(found)
	if len(found) == 0 {
		return ""
	}
	return found[0]
}

// familyInUseLocked returns a member of root's family that is running, starting
// or busy, or "". Caller holds m.mu.
func (m *Manager) familyInUseLocked(root string) string {
	var found []string
	for n, v := range m.vms {
		if n != root && v.SnapshotOf != root {
			continue
		}
		if v.State == "running" || v.State == "starting" || m.busy[n] {
			found = append(found, n)
		}
	}
	sort.Strings(found)
	if len(found) == 0 {
		return ""
	}
	return found[0]
}

// runConflict explains why name can't be started right now because of its
// snapshots (or its original), or returns "".
func (m *Manager) runConflict(name string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return runConflictText(name, m.familyConflictLocked(name))
}

func runConflictText(name, other string) string {
	if other == "" {
		return ""
	}
	return fmt.Sprintf("%s can't start while %s is running: they share a MAC address. Stop %s first.", name, other, other)
}

// startSnapshot validates the request, reserves the snapshot's name and starts
// the copy in the background. It returns the new snapshot's VM name. A
// suspended original is copied with its saved memory. withMemory does the same
// for a running VM that was started suspendable: it is suspended for the copy
// and resumed straight after.
func (m *Manager) startSnapshot(parent, comment string, withMemory bool) (string, error) {
	comment = cleanComment(comment)
	if strings.Contains(parent, "$") {
		return "", errors.New("snapshots of VMs with a $ in the name are not supported")
	}
	m.mu.Lock()
	vm := m.vms[parent]
	var err error
	switch {
	case vm == nil:
		err = fmt.Errorf("VM %q does not exist", parent)
	case vm.SnapshotOf != "":
		err = errors.New("a snapshot can't be snapshotted")
	case vm.Lease != nil:
		err = errors.New("agent VMs can't be snapshotted")
	case m.busy[parent] && vm.State != "running":
		err = fmt.Errorf("%s is busy; try again in a moment", parent)
	case withMemory && (vm.State != "running" || !vm.Suspendable):
		err = fmt.Errorf("a snapshot with memory needs %s to be running and started with Suspendable", parent)
	}
	var resumeOpts runOptions
	if vm != nil {
		resumeOpts = runOptions{Headless: vm.Headless, Override: vm.RunOverride}
	}
	home := m.cfg.VMStoragePath
	m.mu.Unlock()
	if err != nil {
		return "", err
	}
	mac, err := vmConfigMAC(home, parent)
	if err != nil {
		return "", fmt.Errorf("can't read the MAC address of %s: %v", parent, err)
	}
	name, err := m.allocVMName(newVMNamer(parent+snapshotInfix+varAutoNum, nil))
	if err != nil {
		return "", err
	}

	m.mu.Lock()
	vm = m.vms[parent]
	if vm == nil {
		m.mu.Unlock()
		m.releaseVMName(name)
		return "", fmt.Errorf("VM %q does not exist", parent)
	}
	// Register the snapshot's VM now, busy and flagged, so reconcile and the
	// scheduler never treat the half-made copy as a regular VM.
	m.vms[name] = &VM{Name: name, State: "stopped", SnapshotOf: parent, SSHUser: vm.SSHUser, SSHPassword: vm.SSHPassword}
	m.setBusy(name, true)
	m.mu.Unlock()

	t := m.newTask("snapshot", name)
	m.broadcast()
	go m.finishSnapshot(t, parent, name, mac, comment, withMemory, resumeOpts)
	return name, nil
}

func (m *Manager) finishSnapshot(t *Task, parent, name, mac, comment string, withMemory bool, resumeOpts runOptions) {
	var err error
	suspended := false
	if withMemory {
		if err = m.doSuspend(parent); err == nil {
			suspended = true
		}
	}
	if err == nil {
		// NO_AUTO_PRUNE: a snapshot must never make tart delete other VMs or
		// cached images to make room.
		err = m.runIntoEnv(t, []string{"TART_NO_AUTO_PRUNE=1"}, "clone", parent, name)
	}
	if err == nil {
		// tart gives a clone a new MAC address, except for a suspended VM
		// (its saved state needs the same one). Put the original's back.
		if cur, merr := vmConfigMAC(m.storage(), name); merr != nil || !strings.EqualFold(cur, mac) {
			err = m.runInto(t, "set", name, "--mac", mac)
		}
	}
	if err != nil {
		m.tartCmd(m.storage(), "delete", name).Run() // drop a half-made copy
	}
	_, memErr := os.Stat(filepath.Join(m.storage(), "vms", name, "state.vzvmsave"))
	m.mu.Lock()
	if err == nil {
		if p := m.vms[parent]; p != nil {
			p.Snapshots = append(p.Snapshots, Snapshot{Name: name, CreatedAt: time.Now(), Comment: comment, Memory: memErr == nil})
		} else {
			err = errors.New("the original VM was deleted while the snapshot was being made")
		}
	}
	if err != nil {
		delete(m.vms, name)
	}
	m.setBusy(name, false)
	m.save()
	m.mu.Unlock()
	if err != nil {
		m.tartCmd(m.storage(), "delete", name).Run()
	}
	m.finishTask(t, err)
	m.reconcile()
	m.releaseVMName(name)
	m.broadcast()
	if suspended {
		// The original was only paused for the copy.
		if m.resumeHook != nil {
			m.resumeHook(parent, resumeOpts)
		} else {
			m.doRun(parent, "manual", resumeOpts)
		}
	}
}

func cleanComment(s string) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > maxSnapshotCommentLen {
		s = string([]rune(s)[:maxSnapshotCommentLen])
	}
	return s
}

// setSnapshotComment edits the reason noted for a snapshot.
func (m *Manager) setSnapshotComment(snap, comment string) error {
	comment = cleanComment(comment)
	m.mu.Lock()
	defer m.mu.Unlock()
	sv := m.vms[snap]
	if sv == nil || sv.SnapshotOf == "" {
		return fmt.Errorf("%q is not a snapshot", snap)
	}
	p := m.vms[sv.SnapshotOf]
	if p == nil {
		return errors.New("the original VM no longer exists")
	}
	for i := range p.Snapshots {
		if p.Snapshots[i].Name == snap {
			p.Snapshots[i].Comment = comment
			m.save()
			go m.broadcast()
			return nil
		}
	}
	return fmt.Errorf("%q is not a snapshot of %s", snap, p.Name)
}

// deleteSnapshot removes a snapshot's VM and its record.
func (m *Manager) deleteSnapshot(snap string) error {
	m.mu.Lock()
	sv := m.vms[snap]
	switch {
	case sv == nil || sv.SnapshotOf == "":
		m.mu.Unlock()
		return fmt.Errorf("%q is not a snapshot", snap)
	case sv.State == "running" || sv.State == "starting" || m.busy[snap]:
		m.mu.Unlock()
		return fmt.Errorf("stop %s before deleting it", snap)
	}
	parent := sv.SnapshotOf
	m.setBusy(snap, true)
	home := m.cfg.VMStoragePath
	m.mu.Unlock()

	out, err := m.tartCmd(home, "delete", snap).CombinedOutput()
	m.mu.Lock()
	if err == nil {
		delete(m.vms, snap)
		m.forgetSnapshotLocked(parent, snap)
		m.save()
	}
	m.setBusy(snap, false)
	m.mu.Unlock()
	m.reconcile()
	m.broadcast()
	if err != nil {
		return fmt.Errorf("tart delete %s: %v: %s", snap, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// forgetSnapshotLocked drops snap's entry from its original. Caller holds m.mu.
func (m *Manager) forgetSnapshotLocked(parent, snap string) {
	p := m.vms[parent]
	if p == nil {
		return
	}
	kept := p.Snapshots[:0:0]
	for _, s := range p.Snapshots {
		if s.Name != snap {
			kept = append(kept, s)
		}
	}
	p.Snapshots = kept
}

// restoreSnapshot replaces the original VM with the snapshot: the snapshot
// becomes the VM, under the original's name and with the original's settings
// (tags, notes, credentials). The current state of the original is discarded.
func (m *Manager) restoreSnapshot(snap string) error {
	return m.replaceWithSnapshot(snap)
}

func (m *Manager) replaceWithSnapshot(snap string) error {
	m.mu.Lock()
	sv := m.vms[snap]
	if sv == nil || sv.SnapshotOf == "" {
		m.mu.Unlock()
		return fmt.Errorf("%q is not a snapshot", snap)
	}
	parent := sv.SnapshotOf
	if m.vms[parent] == nil {
		m.mu.Unlock()
		return errors.New("the original VM no longer exists")
	}
	if who := m.familyInUseLocked(parent); who != "" {
		m.mu.Unlock()
		return fmt.Errorf("stop %s first", who)
	}
	m.setBusy(parent, true)
	m.setBusy(snap, true)
	home := m.cfg.VMStoragePath
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.setBusy(parent, false)
		m.setBusy(snap, false)
		m.mu.Unlock()
	}()

	rename := func(from, to string) error {
		out, err := m.tartCmd(home, "rename", from, to).CombinedOutput()
		if err != nil {
			return fmt.Errorf("tart rename %s %s: %v: %s", from, to, err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	// Move the original aside first so it can be put back if anything fails.
	aside := parent + ".replaced-" + shortID()
	m.logln("restore: replacing %s with %s", parent, snap)
	if err := rename(parent, aside); err != nil {
		return err
	}
	if err := rename(snap, parent); err != nil {
		if rerr := rename(aside, parent); rerr != nil {
			m.logln("restore: could not put %s back: %v (it is now called %s)", parent, rerr, aside)
		}
		return err
	}
	if out, err := m.tartCmd(home, "delete", aside).CombinedOutput(); err != nil {
		m.logln("restore: could not delete the replaced VM %s: %v: %s", aside, err, strings.TrimSpace(string(out)))
	}

	m.mu.Lock()
	if p := m.vms[parent]; p != nil {
		p.State = "stopped"
		p.LastError = ""
		p.BootFailed = false
	}
	m.forgetSnapshotLocked(parent, snap)
	delete(m.vms, snap)
	m.save()
	m.mu.Unlock()
	m.reconcile()
	m.broadcast()
	return nil
}

// deleteVM deletes a VM. One with snapshots needs a decision: snapMode
// "delete" removes the snapshots too, "promote" makes the snapshot called
// promote the VM (it takes the original's name) instead of deleting anything.
func (m *Manager) deleteVM(name, snapMode, promote string) (string, error) {
	m.mu.Lock()
	vm := m.vms[name]
	var snaps []string
	isSnapshot := false
	if vm != nil {
		isSnapshot = vm.SnapshotOf != ""
		for _, s := range vm.Snapshots {
			snaps = append(snaps, s.Name)
		}
	}
	root := name
	if vm != nil && vm.SnapshotOf != "" {
		root = vm.SnapshotOf
	}
	var inUse string
	if len(snaps) > 0 {
		inUse = m.familyInUseLocked(root)
	}
	m.mu.Unlock()

	if isSnapshot {
		return "", m.deleteSnapshot(name)
	}
	if inUse != "" {
		return "", fmt.Errorf("stop %s before deleting %s", inUse, name)
	}
	if len(snaps) > 0 {
		switch snapMode {
		case "delete":
			for _, s := range snaps {
				if err := m.deleteSnapshot(s); err != nil {
					return "", err
				}
			}
		case "promote":
			found := false
			for _, s := range snaps {
				found = found || s == promote
			}
			if !found {
				return "", fmt.Errorf("%q is not a snapshot of %s", promote, name)
			}
			return "", m.replaceWithSnapshot(promote)
		default:
			return "", fmt.Errorf("%s has %d snapshot(s): choose to delete them too, or to use one as the VM", name, len(snaps))
		}
	}
	out, err := m.tartCmd(m.storage(), "delete", name).CombinedOutput()
	m.reconcile()
	m.broadcast()
	if err != nil {
		return string(out), fmt.Errorf("%v: %s", err, out)
	}
	return string(out), nil
}

// followRename keeps snapshots with their original after it was renamed from
// oldName to newName: the records are re-pointed and the snapshot VMs renamed
// to match ("<new>-Snapshot-<n>"). The original's record is already under
// newName.
func (m *Manager) followRename(oldName, newName string) {
	m.mu.Lock()
	vm := m.vms[newName]
	if vm == nil || len(vm.Snapshots) == 0 {
		m.mu.Unlock()
		return
	}
	var olds []string
	for _, s := range vm.Snapshots {
		olds = append(olds, s.Name)
	}
	home := m.cfg.VMStoragePath
	m.mu.Unlock()

	for _, sn := range olds {
		target := sn
		if rest := strings.TrimPrefix(sn, oldName+snapshotInfix); rest != sn {
			if _, err := strconv.Atoi(rest); err == nil {
				target = newName + snapshotInfix + rest
			}
		}
		m.mu.Lock()
		_, taken := m.vms[target]
		m.setBusy(sn, true)
		m.setBusy(target, true)
		m.mu.Unlock()
		if target != sn && !taken {
			if out, err := m.tartCmd(home, "rename", sn, target).CombinedOutput(); err != nil {
				m.logln("rename: could not rename snapshot %s to %s: %v: %s", sn, target, err, strings.TrimSpace(string(out)))
				target = sn
			}
		} else {
			target = sn
		}
		m.mu.Lock()
		if rec := m.vms[sn]; rec != nil {
			rec.SnapshotOf = newName
			if target != sn {
				moved := *rec
				moved.Name = target
				m.vms[target] = &moved
				delete(m.vms, sn)
			}
		}
		if p := m.vms[newName]; p != nil {
			for i := range p.Snapshots {
				if p.Snapshots[i].Name == sn {
					p.Snapshots[i].Name = target
				}
			}
		}
		m.setBusy(sn, false)
		m.setBusy(target, false)
		m.mu.Unlock()
	}
	m.mu.Lock()
	m.save()
	m.mu.Unlock()
}

// reconcileSnapshotsLocked keeps the snapshot records honest after a reconcile
// pass: a snapshot whose original is gone becomes a regular VM, an entry whose
// VM is gone is dropped, and a flagged VM missing from its original's list is
// added back. seen holds the VM names tart listed. Caller holds m.mu.
func (m *Manager) reconcileSnapshotsLocked(seen map[string]bool) {
	for name, vm := range m.vms {
		if m.busy[name] {
			continue
		}
		if vm.SnapshotOf != "" {
			p := m.vms[vm.SnapshotOf]
			if p == nil {
				if !m.busy[vm.SnapshotOf] {
					log.Printf("snapshot %s: its original %s is gone, so it is a regular VM now", name, vm.SnapshotOf)
					vm.SnapshotOf = ""
				}
				continue
			}
			listed := false
			for _, s := range p.Snapshots {
				listed = listed || s.Name == name
			}
			if !listed {
				p.Snapshots = append(p.Snapshots, Snapshot{Name: name, CreatedAt: time.Now(), Comment: "recovered"})
			}
		}
		if len(vm.Snapshots) > 0 {
			kept := vm.Snapshots[:0:0]
			for _, s := range vm.Snapshots {
				if seen[s.Name] || m.busy[s.Name] {
					kept = append(kept, s)
				}
			}
			vm.Snapshots = kept
		}
	}
}

// snapshotsForUILocked returns vm's snapshots with live state filled in, oldest
// first. Caller holds m.mu.
func (m *Manager) snapshotsForUILocked(vm *VM) []Snapshot {
	if len(vm.Snapshots) == 0 {
		return nil
	}
	out := make([]Snapshot, len(vm.Snapshots))
	copy(out, vm.Snapshots)
	for i := range out {
		if sv := m.vms[out[i].Name]; sv != nil {
			out[i].State = sv.State
			out[i].IP = sv.IP
		}
		out[i].Busy = m.busy[out[i].Name]
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

func (m *Manager) registerSnapshotRoutes(mux *http.ServeMux) {
	type nameBody struct {
		Name    string `json:"name"`
		Comment string `json:"comment"`
		Memory  bool   `json:"memory"` // also save the running VM's memory
	}
	decode := func(w http.ResponseWriter, r *http.Request) (nameBody, bool) {
		var b nameBody
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return b, false
		}
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil || strings.TrimSpace(b.Name) == "" {
			http.Error(w, "name required", http.StatusBadRequest)
			return b, false
		}
		return b, true
	}
	reply := func(w http.ResponseWriter, err error, extra map[string]any) {
		if err != nil {
			writeJSON(w, map[string]any{"error": err.Error()})
			return
		}
		res := map[string]any{"ok": true}
		for k, v := range extra {
			res[k] = v
		}
		writeJSON(w, res)
	}

	mux.HandleFunc("/api/vm/snapshot", func(w http.ResponseWriter, r *http.Request) {
		b, ok := decode(w, r)
		if !ok {
			return
		}
		name, err := m.startSnapshot(b.Name, b.Comment, b.Memory)
		reply(w, err, map[string]any{"name": name})
	})
	mux.HandleFunc("/api/vm/snapshot/restore", func(w http.ResponseWriter, r *http.Request) {
		if b, ok := decode(w, r); ok {
			reply(w, m.restoreSnapshot(b.Name), nil)
		}
	})
	mux.HandleFunc("/api/vm/snapshot/delete", func(w http.ResponseWriter, r *http.Request) {
		if b, ok := decode(w, r); ok {
			reply(w, m.deleteSnapshot(b.Name), nil)
		}
	})
	mux.HandleFunc("/api/vm/snapshot/comment", func(w http.ResponseWriter, r *http.Request) {
		if b, ok := decode(w, r); ok {
			reply(w, m.setSnapshotComment(b.Name, b.Comment), nil)
		}
	})
}

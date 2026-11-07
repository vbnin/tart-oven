package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func postNotes(t *testing.T, m *Manager, body string) {
	t.Helper()
	rec := httptest.NewRecorder()
	m.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/vm/notes", strings.NewReader(body)))
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "error") {
		t.Fatalf("notes → %d %s", rec.Code, rec.Body.String())
	}
}

func TestNotesEndpointSetsAndKeepsStartSuspendable(t *testing.T) {
	m := newTestManager(t)
	m.vms["vm1"] = &VM{Name: "vm1", State: "stopped"}

	postNotes(t, m, `{"name":"vm1","notes":"","tags":[],"startSuspendable":true}`)
	if !m.vms["vm1"].StartSuspendable {
		t.Fatal("startSuspendable:true was not saved")
	}
	// The notes-only editor omits the field: that must not switch it off.
	postNotes(t, m, `{"name":"vm1","notes":"a note","tags":[]}`)
	if !m.vms["vm1"].StartSuspendable {
		t.Fatal("saving a note cleared startSuspendable")
	}
	postNotes(t, m, `{"name":"vm1","notes":"","tags":[],"startSuspendable":false}`)
	if m.vms["vm1"].StartSuspendable {
		t.Fatal("startSuspendable:false was not saved")
	}
}

func TestCloneAppliesStartSuspendableToEveryCopy(t *testing.T) {
	m := newSnapshotTestManager(t)
	addVM(t, m, "base", "aa:aa:aa:aa:aa:aa", "d")
	m.createVMs(createReq{Mode: "clone", Source: "base", NameTemplate: "copy-$AUTONUM", Count: 2, StartSuspendable: true}, "batch1")
	waitTasks(t, m)
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for name, vm := range m.vms {
		if name == "base" {
			if vm.StartSuspendable {
				t.Error("the source VM must not change")
			}
			continue
		}
		n++
		if !vm.StartSuspendable {
			t.Errorf("clone %s is not startSuspendable", name)
		}
	}
	if n != 2 {
		t.Fatalf("expected 2 clones, got %d", n)
	}
}

func TestCloneWithoutTheOptionStaysOff(t *testing.T) {
	m := newSnapshotTestManager(t)
	addVM(t, m, "base", "aa:aa:aa:aa:aa:aa", "d")
	m.createVMs(createReq{Mode: "clone", Source: "base", NameTemplate: "copy-$AUTONUM", Count: 1}, "batch2")
	waitTasks(t, m)
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, vm := range m.vms {
		if vm.StartSuspendable {
			t.Errorf("%s started suspendable without being asked", name)
		}
	}
}

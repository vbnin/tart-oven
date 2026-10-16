package server

import (
	"encoding/json"
	"testing"
)

func TestVMCustomCredentialsCanBeRemoved(t *testing.T) {
	m, h := newAgentTestManager(t)
	m.cfg.SSHUser = "admin"
	m.cfg.SSHPassword = "default-pw"
	m.vms["vm1"] = &VM{Name: "vm1", State: "stopped"}

	post := func(body map[string]any) {
		t.Helper()
		body["name"] = "vm1"
		b, _ := json.Marshal(body)
		rec := do(h, "POST", "/api/vm/notes", string(b), nil)
		var res map[string]any
		decode(t, rec, &res)
		if res["error"] != nil {
			t.Fatalf("notes: %v", res)
		}
	}
	pw := func() (string, string) { return m.savedSudoPassword("vm1") }
	view := func() *VM {
		for _, v := range m.snapshot().VMs {
			if v.Name == "vm1" {
				return v
			}
		}
		t.Fatal("vm1 missing from the view")
		return nil
	}

	// Without a custom password the default is used.
	if got, from := pw(); got != "default-pw" || from != "default" {
		t.Fatalf("got %q from %q", got, from)
	}
	// A custom one wins, and the dashboard is told one is set (never its value).
	post(map[string]any{"sshPassword": "vm-pw", "sshUser": "recovery"})
	if got, from := pw(); got != "vm-pw" || from != "VM setting" {
		t.Fatalf("got %q from %q", got, from)
	}
	if v := view(); !v.SSHPasswordSet || v.SSHPassword != "" {
		t.Fatalf("view: set=%v password=%q", v.SSHPasswordSet, v.SSHPassword)
	}
	// A blank save keeps it, as before.
	post(map[string]any{"sshPassword": ""})
	if got, _ := pw(); got != "vm-pw" {
		t.Fatalf("a blank save changed the password to %q", got)
	}
	// Removing it falls back to the default; so does the user.
	post(map[string]any{"clearSshPassword": true, "clearSshUser": true})
	if got, from := pw(); got != "default-pw" || from != "default" {
		t.Fatalf("after removal: %q from %q", got, from)
	}
	if user, _ := effectiveSSHCredentials(m.cfg, m.vms["vm1"]); user != "admin" {
		t.Fatalf("after removal the user is %q, want the default", user)
	}
	if view().SSHPasswordSet {
		t.Fatal("still reported as set")
	}
	// Removing and setting in one save sets the new one.
	post(map[string]any{"clearSshPassword": true, "sshPassword": "again"})
	if got, _ := pw(); got != "again" {
		t.Fatalf("got %q", got)
	}
}

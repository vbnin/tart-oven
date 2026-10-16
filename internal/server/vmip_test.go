package server

import (
	"os"
	"testing"
	"time"
)

// A running VM's IP changing after a DHCP renewal logs the change and saves it.
// This once deadlocked: the update logged while holding m.mu, and logln takes
// m.mu itself, which froze the whole server.
func TestApplyVMIPRecordsChangeWithoutDeadlock(t *testing.T) {
	m := newTestManager(t)
	m.vms["vm1"] = &VM{Name: "vm1", State: "running", IP: "192.168.1.34"}

	done := make(chan struct{})
	go func() {
		m.applyVMIP("vm1", "192.168.103.181")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("applyVMIP deadlocked on m.mu")
	}

	m.mu.Lock()
	got := m.vms["vm1"].IP
	m.mu.Unlock()
	if got != "192.168.103.181" {
		t.Fatalf("IP = %q, want the new address", got)
	}
	if _, err := os.Stat(m.statePath); err != nil {
		t.Fatalf("state was not saved: %v", err)
	}
}

// A VM that stopped, or whose IP is unchanged, is left alone.
func TestApplyVMIPIgnoresStoppedAndUnchanged(t *testing.T) {
	m := newTestManager(t)
	m.vms["stopped"] = &VM{Name: "stopped", State: "stopped", IP: "10.0.0.1"}
	m.vms["same"] = &VM{Name: "same", State: "running", IP: "10.0.0.2"}

	m.applyVMIP("stopped", "10.0.0.9")
	m.applyVMIP("same", "10.0.0.2")
	m.applyVMIP("ghost", "10.0.0.3")

	if got := m.vms["stopped"].IP; got != "10.0.0.1" {
		t.Fatalf("stopped VM IP changed to %q", got)
	}
	if _, err := os.Stat(m.statePath); err == nil {
		t.Fatal("state was saved although nothing changed")
	}
}

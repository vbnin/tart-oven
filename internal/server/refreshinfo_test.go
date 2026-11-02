package server

import (
	"net/http"
	"testing"
	"time"
)

// Refresh info on a VM Tart Oven doesn't know must answer quickly with an
// error result instead of hanging or panicking.
func TestInfoEndpointUnknownVMReturns(t *testing.T) {
	m, h := newAuthTestManager(t)
	m.cfg.TartAppPath = "/nonexistent/tart"
	m.cfg.StatusCommand = "sw_vers"
	m.agentProbedSinceBoot = map[string]bool{}
	m.subs = map[chan []byte]struct{}{}

	done := make(chan int, 1)
	go func() { done <- do(h, "GET", "/api/info?name=ghost", "", nil).Code }()
	select {
	case code := <-done:
		if code != http.StatusOK {
			t.Fatalf("status %d", code)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Refresh info hung on an unknown VM")
	}
}

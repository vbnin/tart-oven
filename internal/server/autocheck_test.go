package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestClampAutoCheckSeconds(t *testing.T) {
	for in, want := range map[int]int{
		-5: 0, 0: 0, 1: minAutoCheckSeconds, 9: minAutoCheckSeconds, 10: 10, 120: 120,
		86400: 86400, 86401: 86400, 1000000: 86400,
	} {
		if got := clampAutoCheckSeconds(in); got != want {
			t.Errorf("clamp(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestAutoCheckDefaultsOnForNewAndOlderConfigsButKeepsAnExplicitOff(t *testing.T) {
	if defaultConfig().AutoCheckSeconds != defaultAutoCheckSeconds || defaultAutoCheckSeconds == 0 {
		t.Fatal("a new config should auto-check")
	}
	for state, want := range map[string]int{
		`{"config":{"listen":"127.0.0.1:9000"}}`:                           defaultAutoCheckSeconds, // older state: no key
		`{"config":{"listen":"127.0.0.1:9000","autoCheckSeconds":0}}`:      0,                       // explicit off
		`{"config":{"listen":"127.0.0.1:9000","autoCheckSeconds":90}}`:     90,
		`{"config":{"listen":"127.0.0.1:9000","autoCheckSeconds":999999}}`: maxAutoCheckSeconds, // hand-edited file
	} {
		m := newTestManager(t)
		if err := os.WriteFile(m.statePath, []byte(state), 0o600); err != nil {
			t.Fatal(err)
		}
		m.load()
		if m.cfg.AutoCheckSeconds != want {
			t.Errorf("%s → %d, want %d", state, m.cfg.AutoCheckSeconds, want)
		}
	}
}

func TestConfigEndpointClampsAutoCheckSeconds(t *testing.T) {
	m := newTestManager(t)
	// In order: the last case checks that a non-number leaves the value alone.
	for _, c := range []struct {
		body string
		want int
	}{
		{`{"autoCheckSeconds":0}`, 0},
		{`{"autoCheckSeconds":3}`, minAutoCheckSeconds},
		{`{"autoCheckSeconds":120}`, 120},
		{`{"autoCheckSeconds":500000}`, maxAutoCheckSeconds},
		{`{"autoCheckSeconds":"x"}`, maxAutoCheckSeconds},
	} {
		m.handleConfig(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(c.body)))
		if m.cfg.AutoCheckSeconds != c.want {
			t.Errorf("%s → %d, want %d", c.body, m.cfg.AutoCheckSeconds, c.want)
		}
	}
}

// Saving other settings must not keep restarting the auto-check timer, or a
// client that saves more often than the interval would postpone it forever.
func TestConfigSaveRestartsTheAutoCheckTimerOnlyWhenTheIntervalChanges(t *testing.T) {
	m := newTestManager(t)
	m.autoCheckReload = make(chan struct{}, 1)
	post := func(body string) bool {
		m.handleConfig(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(body)))
		select {
		case <-m.autoCheckReload:
			return true
		default:
			return false
		}
	}
	if post(`{"windowMinutes":30}`) {
		t.Error("an unrelated setting restarted the auto-check timer")
	}
	if post(`{"autoCheckSeconds":` + strconv.Itoa(m.cfg.AutoCheckSeconds) + `}`) {
		t.Error("saving the same interval restarted the timer")
	}
	if !post(`{"autoCheckSeconds":60}`) {
		t.Error("a new interval must take effect at once")
	}
}

func TestAutoCheckTargetsOnlyIdleRunningVMs(t *testing.T) {
	m := newTestManager(t)
	m.vms["run"] = &VM{Name: "run", State: "running"}
	m.vms["busy"] = &VM{Name: "busy", State: "running"}
	m.vms["stopped"] = &VM{Name: "stopped", State: "stopped"}
	m.vms["susp"] = &VM{Name: "susp", State: "suspended"}
	m.vms["starting"] = &VM{Name: "starting", State: "starting"}
	m.busy["busy"] = true

	got := m.autoCheckTargets()
	if len(got) != 1 || got[0] != "run" {
		t.Fatalf("targets = %v, want [run]", got)
	}
}

// A check that finishes after the VM began stopping must not overwrite the last
// known status with failures caused by the shutdown.
func TestLateCheckResultsDoNotOverwriteAStoppingVM(t *testing.T) {
	m := newTestManager(t)
	m.vms["web"] = &VM{Name: "web", State: "stopping", SSHOK: true, AgentOK: true}
	m.setAgentOK("web", false)
	if !m.vms["web"].AgentOK {
		t.Error("agent result recorded on a stopping VM")
	}
	m.vms["web"].State = "running"
	m.setAgentOK("web", false)
	if m.vms["web"].AgentOK {
		t.Error("agent result not recorded on a running VM")
	}
	if m.liveVMLocked("missing") != nil {
		t.Error("a missing VM is not live")
	}
}

// The check re-decides whether agent exec goes through the agent or SSH, and
// repeats the whole post-boot sequence: not something to do to a VM an agent
// holds, one that booted a moment ago, or one being enrolled.
func TestAutoCheckLeavesLeasedFreshlyBootedAndEnrollingVMsAlone(t *testing.T) {
	m := newTestManager(t)
	old := time.Now().Add(-autoCheckBootGrace - time.Minute)
	m.vms["ok"] = &VM{Name: "ok", State: "running", StartedAt: old}
	m.vms["leased"] = &VM{Name: "leased", State: "running", StartedAt: old, Lease: &Lease{Owner: "agent"}}
	m.vms["fresh"] = &VM{Name: "fresh", State: "running", StartedAt: time.Now().Add(-30 * time.Second)}
	m.vms["enrolling"] = &VM{Name: "enrolling", State: "running", StartedAt: old}
	m.vms["enrolled"] = &VM{Name: "enrolled", State: "running", StartedAt: old}
	m.tasks = []*Task{
		{Kind: "enroll", Target: "enrolling", Status: "running"},
		{Kind: "enroll", Target: "enrolled", Status: "success"}, // finished: checked again
		{Kind: "clone", Target: "ok", Status: "running"},        // only enroll tasks count
	}

	got := m.autoCheckTargets()
	sort.Strings(got)
	if want := []string{"enrolled", "ok"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("targets = %v, want %v", got, want)
	}
}

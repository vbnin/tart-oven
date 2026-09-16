package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestNormalizeJamfBaseURL(t *testing.T) {
	tests := []struct {
		name, input, want string
		wantErr           bool
	}{
		{"empty before setup", "  ", "", false},
		{"trim HTTPS", " https://example.jamfcloud.com/// ", "https://example.jamfcloud.com", false},
		{"allow HTTP", "http://jamf.test/", "http://jamf.test", false},
		{"missing scheme", "example.jamfcloud.com", "", true},
		{"wrong scheme", "ftp://example.test", "", true},
		{"missing host", "https:///enroll", "", true},
		{"port without hostname", "https://:443", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeJamfBaseURL(tt.input)
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Fatalf("got %q, %v; want %q, error=%v", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

func TestEffectiveSSHCredentials(t *testing.T) {
	cfg := Config{SSHUser: "admin", SSHPassword: "admin"}
	if user, pass := effectiveSSHCredentials(cfg, &VM{}); user != "admin" || pass != "admin" {
		t.Fatalf("global credentials = %q/%q", user, pass)
	}
	vm := &VM{SSHUser: "builder", SSHPassword: "builder-pass"}
	if user, pass := effectiveSSHCredentials(cfg, vm); user != "builder" || pass != "builder-pass" {
		t.Fatalf("per-VM credentials = %q/%q", user, pass)
	}
}

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	return &Manager{
		cfg:                  defaultConfig(),
		vms:                  make(map[string]*VM),
		busy:                 make(map[string]bool),
		opStart:              make(map[string]time.Time),
		runningCmds:          make(map[string]*exec.Cmd),
		agentProbedSinceBoot: make(map[string]bool),
		subs:                 make(map[chan []byte]struct{}),
		statePath:            filepath.Join(t.TempDir(), "state.json"),
		reload:               make(chan struct{}, 1),
	}
}

func TestNewConfigView(t *testing.T) {
	view := newConfigView(Config{SSHPassword: "saved-password", JamfInvitationCode: "saved-invite"})
	data, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "saved-password") || strings.Contains(string(data), "saved-invite") {
		t.Fatalf("client view leaked secrets: %s", data)
	}
	if !view.SSHPasswordSet || !view.JamfInvitationCodeSet {
		t.Fatal("missing saved flags")
	}
}

func TestHandleConfigBlankSecrets(t *testing.T) {
	m := newTestManager(t)
	m.cfg.JamfInvitationCode = "saved-invite"
	m.cfg.SSHPassword = "saved-password"

	body, err := json.Marshal(Config{JamfBaseURL: "https://new.example/", Paused: true})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/config", bytes.NewReader(body))
	res := httptest.NewRecorder()
	m.handleConfig(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", res.Code, http.StatusOK, res.Body.String())
	}
	if m.cfg.JamfInvitationCode != "saved-invite" || m.cfg.SSHPassword != "saved-password" {
		t.Fatalf("blank secrets overwrote saved values: %#v", m.cfg)
	}
	if m.cfg.JamfBaseURL != "https://new.example" {
		t.Fatalf("JamfBaseURL = %q, want normalized URL", m.cfg.JamfBaseURL)
	}
}

func TestHandleConfigPreservesOCIExclusionWhenFieldIsOmitted(t *testing.T) {
	m := newTestManager(t)
	m.cfg.ExcludeOCIFromScheduler = true
	req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(`{"listen":"127.0.0.1:9000","intervalMinutes":5,"windowMinutes":120,"maxConcurrent":1}`))
	res := httptest.NewRecorder()

	m.handleConfig(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", res.Code, http.StatusOK, res.Body.String())
	}
	if !m.cfg.ExcludeOCIFromScheduler {
		t.Fatal("an older client omitting excludeOciFromScheduler disabled the safe default")
	}
}

func TestHandleConfigAppliesExplicitOCIExclusionValues(t *testing.T) {
	for _, want := range []bool{false, true} {
		t.Run(strings.ToUpper(strconv.FormatBool(want)), func(t *testing.T) {
			m := newTestManager(t)
			m.cfg.ExcludeOCIFromScheduler = !want
			body := fmt.Sprintf(`{"listen":"127.0.0.1:9000","intervalMinutes":5,"windowMinutes":120,"maxConcurrent":1,"excludeOciFromScheduler":%t}`, want)
			req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(body))
			res := httptest.NewRecorder()

			m.handleConfig(res, req)

			if res.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d: %s", res.Code, http.StatusOK, res.Body.String())
			}
			if m.cfg.ExcludeOCIFromScheduler != want {
				t.Fatalf("ExcludeOCIFromScheduler = %v, want %v", m.cfg.ExcludeOCIFromScheduler, want)
			}
		})
	}
}

func TestHandleConfigJamfPartialSavePreservesAllSchedulerSettings(t *testing.T) {
	m := newTestManager(t)
	m.cfg.WindowMinutes = 90
	m.cfg.IntervalMinutes = 15
	m.cfg.DailyEnabled = true
	m.cfg.DailyStart = "09:00"
	m.cfg.DailyStop = "18:00"
	m.cfg.Paused = true
	m.cfg.ExcludeOCIFromScheduler = true
	m.cfg.SSHUser = "admin"
	m.cfg.SSHPassword = "initial-pass"

	body := `{"jamfBaseUrl":"https://company.jamfcloud.com","jamfInvitationCode":"inv-999","sshUser":"admin","sshPassword":"new-pass"}`
	req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(body))
	res := httptest.NewRecorder()

	m.handleConfig(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", res.Code, http.StatusOK, res.Body.String())
	}
	if m.cfg.JamfBaseURL != "https://company.jamfcloud.com" {
		t.Errorf("JamfBaseURL = %q, want %q", m.cfg.JamfBaseURL, "https://company.jamfcloud.com")
	}
	if m.cfg.JamfInvitationCode != "inv-999" {
		t.Errorf("JamfInvitationCode = %q, want %q", m.cfg.JamfInvitationCode, "inv-999")
	}
	if m.cfg.SSHPassword != "new-pass" {
		t.Errorf("SSHPassword = %q, want %q", m.cfg.SSHPassword, "new-pass")
	}
	// Verify scheduler settings were NOT wiped or reset
	if m.cfg.WindowMinutes != 90 {
		t.Errorf("WindowMinutes = %d, want 90", m.cfg.WindowMinutes)
	}
	if m.cfg.IntervalMinutes != 15 {
		t.Errorf("IntervalMinutes = %d, want 15", m.cfg.IntervalMinutes)
	}
	if !m.cfg.Paused {
		t.Error("Paused changed to false, expected true")
	}
	if !m.cfg.DailyEnabled {
		t.Error("DailyEnabled changed to false, expected true")
	}
	if m.cfg.DailyStart != "09:00" || m.cfg.DailyStop != "18:00" {
		t.Errorf("Daily window = %s-%s, want 09:00-18:00", m.cfg.DailyStart, m.cfg.DailyStop)
	}
}

func TestHandleConfigJamfProfiles(t *testing.T) {
	m := newTestManager(t)
	// Initial save with two profiles
	body := `{"jamfProfiles":[
		{"id":"p1","name":"Production","baseUrl":"https://prod.jamfcloud.com","invitationCode":"secret-prod-123"},
		{"id":"p2","name":"Staging","baseUrl":"https://staging.jamfcloud.com/","invitationCode":"secret-staging-456"}
	]}`
	req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(body))
	res := httptest.NewRecorder()
	m.handleConfig(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", res.Code, http.StatusOK, res.Body.String())
	}
	if len(m.cfg.JamfProfiles) != 2 {
		t.Fatalf("len(JamfProfiles) = %d, want 2", len(m.cfg.JamfProfiles))
	}
	if m.cfg.JamfProfiles[1].BaseURL != "https://staging.jamfcloud.com" {
		t.Errorf("BaseURL not normalized: %q", m.cfg.JamfProfiles[1].BaseURL)
	}

	// Update profiles omitting invitation code for p1 (should preserve secret)
	bodyUpdate := `{"jamfProfiles":[
		{"id":"p1","name":"Production Main","baseUrl":"https://prod.jamfcloud.com","invitationCode":""},
		{"id":"p3","name":"Sandbox","baseUrl":"https://sandbox.jamfcloud.com","invitationCode":"secret-sb-789"}
	]}`
	req2 := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(bodyUpdate))
	res2 := httptest.NewRecorder()
	m.handleConfig(res2, req2)

	if res2.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", res2.Code, http.StatusOK, res2.Body.String())
	}
	if len(m.cfg.JamfProfiles) != 2 {
		t.Fatalf("len(JamfProfiles) = %d, want 2", len(m.cfg.JamfProfiles))
	}
	if m.cfg.JamfProfiles[0].Name != "Production Main" {
		t.Errorf("Name = %q, want Production Main", m.cfg.JamfProfiles[0].Name)
	}
	if m.cfg.JamfProfiles[0].InvitationCode != "secret-prod-123" {
		t.Errorf("InvitationCode was not preserved: %q", m.cfg.JamfProfiles[0].InvitationCode)
	}
}

func TestSSHKeyPathRejectsBlankAndRelative(t *testing.T) {
	for _, bad := range []string{"", "   ", "tart-oven", "./keys/id", "keys/id"} {
		if validSSHKeyPath(bad) {
			t.Fatalf("%q must be rejected", bad)
		}
	}
	for _, good := range []string{"~/.ssh/tart-oven", "/Users/rob/.ssh/id_ed25519"} {
		if !validSSHKeyPath(good) {
			t.Fatalf("%q must be accepted", good)
		}
	}
}

func TestConfigMergeRejectsARelativeSSHKey(t *testing.T) {
	m := &Manager{cfg: defaultConfig(), vms: map[string]*VM{}, busy: map[string]bool{},
		statePath: filepath.Join(t.TempDir(), "state.json"), reload: make(chan struct{}, 1)}
	m.handleConfig(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(`{"sshKey":"tart-oven"}`)))
	if m.cfg.SSHKey != "~/.ssh/tart-oven" {
		t.Fatalf("sshKey = %q, want the default preserved", m.cfg.SSHKey)
	}
}

func TestNotesUpdateKeepsPerVMSSHUserWhenOmitted(t *testing.T) {
	m := &Manager{cfg: defaultConfig(), busy: map[string]bool{},
		vms:       map[string]*VM{"vm1": {Name: "vm1", SSHUser: "tester"}},
		statePath: filepath.Join(t.TempDir(), "state.json")}
	req := httptest.NewRequest(http.MethodPost, "/api/vm/notes",
		strings.NewReader(`{"name":"vm1","notes":"a note","tags":[]}`))
	m.routes().ServeHTTP(httptest.NewRecorder(), req)
	if got := m.vms["vm1"].SSHUser; got != "tester" {
		t.Fatalf("sshUser = %q, want it preserved", got)
	}
}

func TestLoadRetiresTheLegacyJamfUserStatusCommand(t *testing.T) {
	m := newTestManager(t)
	body := `{"config":{"listen":"127.0.0.1:9000","statusCommand":` +
		strconv.Quote(legacyJamfUserStatusCommand) + `},"vms":{},"history":[]}`
	if err := os.WriteFile(m.statePath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	m.load()
	if strings.Contains(m.cfg.StatusCommand, "no Jamf user") {
		t.Fatalf("legacy status command survived load: %q", m.cfg.StatusCommand)
	}
	if m.cfg.StatusCommand != defaultConfig().StatusCommand {
		t.Fatalf("status command = %q, want the new default", m.cfg.StatusCommand)
	}
}

func TestLoadPreservesACustomisedStatusCommand(t *testing.T) {
	m := newTestManager(t)
	custom := `hostname; echo mine`
	body := `{"config":{"listen":"127.0.0.1:9000","statusCommand":` +
		strconv.Quote(custom) + `},"vms":{},"history":[]}`
	if err := os.WriteFile(m.statePath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	m.load()
	if m.cfg.StatusCommand != custom {
		t.Fatalf("customised status command was overwritten: %q", m.cfg.StatusCommand)
	}
}

func TestHandleConfigAppliesNoGraphicsAndNoAudio(t *testing.T) {
	m := newTestManager(t)
	body := `{"noGraphics":true,"noAudio":true}`
	req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(body))
	res := httptest.NewRecorder()
	m.handleConfig(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", res.Code, http.StatusOK, res.Body.String())
	}
	if !m.cfg.NoGraphics || !m.cfg.NoAudio {
		t.Fatalf("NoGraphics = %v, NoAudio = %v; want both true", m.cfg.NoGraphics, m.cfg.NoAudio)
	}
}

func TestHasArg(t *testing.T) {
	args := []string{"--net-bridged=en0", "--no-graphics", "--dir=host_resources:/shared"}
	if !hasArg(args, "--no-graphics") {
		t.Error("expected --no-graphics to be found")
	}
	if !hasArg(args, "--net-bridged") {
		t.Error("expected --net-bridged prefix to be found")
	}
	if hasArg(args, "--no-audio") {
		t.Error("did not expect --no-audio to be found")
	}
}

func TestFirstRunCompletedConfig(t *testing.T) {
	m := newTestManager(t)
	mux := m.routes()

	// Initial default should be false and empty string
	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var view struct {
		FirstRunCompleted bool   `json:"firstRunCompleted"`
		OperatorRole      string `json:"operatorRole"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&view); err != nil {
		t.Fatalf("failed to decode config view: %v", err)
	}
	if view.FirstRunCompleted {
		t.Fatalf("expected FirstRunCompleted to default to false")
	}
	if view.OperatorRole != "" {
		t.Fatalf("expected OperatorRole to default to empty string")
	}

	// Update firstRunCompleted and operatorRole via POST /api/config
	updateBody := `{"firstRunCompleted": true, "operatorRole": "jamf"}`
	req = httptest.NewRequest(http.MethodPost, "/api/config", bytes.NewBufferString(updateBody))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}

	m.mu.Lock()
	savedCompleted := m.cfg.FirstRunCompleted
	savedRole := m.cfg.OperatorRole
	m.mu.Unlock()

	if !savedCompleted || savedRole != "jamf" {
		t.Fatalf("failed to persist firstRunCompleted/operatorRole: got completed=%v role=%s", savedCompleted, savedRole)
	}
}

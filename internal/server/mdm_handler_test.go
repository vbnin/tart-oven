package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"tart-oven/internal/mdm"
	"testing"
	"time"
)

const (
	testJamfInvitationCode  = "invite-code-secret"
	testGlobalSSHUser       = "global-user-secret"
	testGlobalSSHPassword   = "global-password-secret"
	testVMSSHUser           = "vm-user-secret"
	testVMSSHPassword       = "vm-password-secret"
	testResolverErrorSecret = "resolver-underlying-secret"
	testCopierErrorSecret   = "copier-underlying-secret"
)

type recordingMDMProfileCopier struct {
	target      mdm.TransferTarget
	profile     []byte
	payloadUUID string
	err         error
	called      bool
	checkLock   func() bool
}

func (f *recordingMDMProfileCopier) CopyAndVerify(_ context.Context, target mdm.TransferTarget, profile []byte, payloadUUID string) error {
	f.called = true
	f.target = target
	f.profile = append([]byte(nil), profile...)
	f.payloadUUID = payloadUUID
	if f.checkLock != nil && !f.checkLock() {
		return errors.New("manager lock held during profile transfer")
	}
	return f.err
}

func newMDMHandlerManager() *Manager {
	return &Manager{
		cfg: Config{
			JamfBaseURL:        "https://jamf.example",
			JamfInvitationCode: testJamfInvitationCode,
			SSHUser:            testGlobalSSHUser,
			SSHPassword:        testGlobalSSHPassword,
			SSHTimeoutSec:      15,
		},
		vms: map[string]*VM{
			"base": {Name: "base", State: "running", IP: "192.0.2.10"},
		},
	}
}

func performMDMProfileRequest(t *testing.T, m *Manager, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/api/vm/mdm-profile", strings.NewReader(body))
	rr := httptest.NewRecorder()
	m.handleMDMProfile(rr, req)
	return rr
}

func decodeMDMProfileResponse(t *testing.T, rr *httptest.ResponseRecorder, wantKeys ...string) mdmProfileResponse {
	t.Helper()
	if got := rr.Result().Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type=%q, want application/json; body=%s", got, rr.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw response %q: %v", rr.Body.String(), err)
	}
	if len(raw) != len(wantKeys) {
		t.Fatalf("response keys=%v, want exactly %v; body=%s", rawJSONKeys(raw), wantKeys, rr.Body.String())
	}
	for _, key := range wantKeys {
		if _, ok := raw[key]; !ok {
			t.Fatalf("response keys=%v, missing %q; body=%s", rawJSONKeys(raw), key, rr.Body.String())
		}
	}
	var response mdmProfileResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response %q: %v", rr.Body.String(), err)
	}
	return response
}

func rawJSONKeys(raw map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(raw))
	for key := range raw {
		keys = append(keys, key)
	}
	return keys
}

func assertMDMSecretSafe(t *testing.T, output string) {
	t.Helper()
	for _, secret := range []string{
		testJamfInvitationCode,
		testGlobalSSHUser,
		testGlobalSSHPassword,
		testVMSSHUser,
		testVMSSHPassword,
		testResolverErrorSecret,
		testCopierErrorSecret,
		`"password"`,
		"<plist",
	} {
		if strings.Contains(output, secret) {
			t.Fatalf("output leaked secret %q: %s", secret, output)
		}
	}
}

func captureMDMLog(t *testing.T, run func()) string {
	t.Helper()
	var captured bytes.Buffer
	previousWriter := log.Writer()
	previousFlags := log.Flags()
	previousPrefix := log.Prefix()
	log.SetOutput(&captured)
	log.SetFlags(0)
	log.SetPrefix("")
	defer func() {
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
		log.SetPrefix(previousPrefix)
	}()
	run()
	return captured.String()
}

func TestHandleMDMProfileSuccess(t *testing.T) {
	m := newMDMHandlerManager()
	fake := &recordingMDMProfileCopier{}
	m.mdmCopier = fake
	m.mdmResolveIP = func(context.Context, string, string) (string, error) {
		t.Fatal("resolver called despite cached VM IP")
		return "", nil
	}

	rr := performMDMProfileRequest(t, m, http.MethodPost, `{"name":"base"}`)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if fake.target.Address != "192.0.2.10:22" || fake.target.Username != testGlobalSSHUser || fake.target.Password != testGlobalSSHPassword {
		t.Fatalf("target=%#v", fake.target)
	}
	if fake.target.Timeout != 15*time.Second {
		t.Fatalf("timeout=%s, want 15s", fake.target.Timeout)
	}
	if !fake.called || fake.payloadUUID == "" {
		t.Fatalf("copy arguments missing generated profile: called=%v uuid=%q profile=%q", fake.called, fake.payloadUUID, fake.profile)
	}
	if err := mdm.ValidateProfile(fake.profile, mdm.ProfileInput{
		BaseURL:        "https://jamf.example",
		InvitationCode: testJamfInvitationCode,
	}, fake.payloadUUID); err != nil {
		t.Fatalf("copied profile is not the generated Jamf profile: %v; profile=%q", err, fake.profile)
	}
	response := decodeMDMProfileResponse(t, rr, "ok", "name", "path", "payloadUUID")
	if !response.OK || response.Name != "base" || response.Path != mdm.ProfileDisplayPath || response.PayloadUUID != fake.payloadUUID {
		t.Fatalf("response=%#v", response)
	}
	assertMDMSecretSafe(t, rr.Body.String())
}

func TestHandleMDMProfileRouteIsRegistered(t *testing.T) {
	m := newMDMHandlerManager()
	m.mdmCopier = &recordingMDMProfileCopier{}
	req := httptest.NewRequest(http.MethodPost, "/api/vm/mdm-profile", strings.NewReader(`{"name":"base"}`))
	rr := httptest.NewRecorder()

	m.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestHandleMDMProfileRejectsInvalidRequests(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		body       string
		mutate     func(*Manager)
		wantStatus int
		wantStage  mdm.Stage
	}{
		{name: "GET", method: http.MethodGet, body: `{"name":"base"}`, wantStatus: http.StatusMethodNotAllowed},
		{name: "malformed JSON", method: http.MethodPost, body: `{`, wantStatus: http.StatusBadRequest},
		{name: "missing name", method: http.MethodPost, body: `{}`, wantStatus: http.StatusBadRequest},
		{name: "blank name", method: http.MethodPost, body: `{"name":"  "}`, wantStatus: http.StatusBadRequest},
		{
			name:       "missing Jamf URL",
			method:     http.MethodPost,
			body:       `{"name":"base"}`,
			mutate:     func(m *Manager) { m.cfg.JamfBaseURL = "" },
			wantStatus: http.StatusBadRequest,
			wantStage:  mdm.StageConfiguration,
		},
		{
			name:       "missing invitation code",
			method:     http.MethodPost,
			body:       `{"name":"base"}`,
			mutate:     func(m *Manager) { m.cfg.JamfInvitationCode = "" },
			wantStatus: http.StatusBadRequest,
			wantStage:  mdm.StageConfiguration,
		},
		{
			name:       "missing SSH user",
			method:     http.MethodPost,
			body:       `{"name":"base"}`,
			mutate:     func(m *Manager) { m.cfg.SSHUser = "" },
			wantStatus: http.StatusBadRequest,
			wantStage:  mdm.StageConfiguration,
		},
		{
			name:       "missing SSH password",
			method:     http.MethodPost,
			body:       `{"name":"base"}`,
			mutate:     func(m *Manager) { m.cfg.SSHPassword = "" },
			wantStatus: http.StatusBadRequest,
			wantStage:  mdm.StageConfiguration,
		},
		{
			name:       "missing VM",
			method:     http.MethodPost,
			body:       `{"name":"missing"}`,
			wantStatus: http.StatusBadRequest,
			wantStage:  mdm.StageVM,
		},
		{
			name:       "stopped VM",
			method:     http.MethodPost,
			body:       `{"name":"base"}`,
			mutate:     func(m *Manager) { m.vms["base"].State = "stopped" },
			wantStatus: http.StatusBadRequest,
			wantStage:  mdm.StageVM,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newMDMHandlerManager()
			fake := &recordingMDMProfileCopier{}
			m.mdmCopier = fake
			if tt.mutate != nil {
				tt.mutate(m)
			}
			rr := performMDMProfileRequest(t, m, tt.method, tt.body)
			if rr.Code != tt.wantStatus {
				t.Fatalf("status=%d body=%s, want %d", rr.Code, rr.Body.String(), tt.wantStatus)
			}
			wantKeys := []string{"ok", "error"}
			if tt.wantStage != "" {
				wantKeys = []string{"ok", "name", "stage", "error"}
			}
			response := decodeMDMProfileResponse(t, rr, wantKeys...)
			if response.OK || response.Error == "" || response.Stage != tt.wantStage {
				t.Fatalf("response=%#v, want failed stage %q", response, tt.wantStage)
			}
			if fake.called {
				t.Fatal("copier called for invalid request")
			}
			assertMDMSecretSafe(t, rr.Body.String())
		})
	}
}

func TestHandleMDMProfileResolvesMissingIPWithoutHoldingManagerLock(t *testing.T) {
	m := newMDMHandlerManager()
	m.vms["base"].IP = ""
	fake := &recordingMDMProfileCopier{}
	m.mdmCopier = fake
	var gotName, gotHome string
	m.mdmResolveIP = func(_ context.Context, name, home string) (string, error) {
		gotName, gotHome = name, home
		if !m.mu.TryLock() {
			t.Fatal("manager lock held during IP resolution")
		}
		m.mu.Unlock()
		return "198.51.100.12", nil
	}
	m.cfg.VMStoragePath = "/tmp/tart-home"

	rr := performMDMProfileRequest(t, m, http.MethodPost, `{"name":" base "}`)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if gotName != "base" || gotHome != "/tmp/tart-home" {
		t.Fatalf("resolver arguments=(%q, %q)", gotName, gotHome)
	}
	if fake.target.Address != "198.51.100.12:22" {
		t.Fatalf("address=%q", fake.target.Address)
	}
}

func TestHandleMDMProfileReportsInjectedIPFailure(t *testing.T) {
	m := newMDMHandlerManager()
	m.vms["base"].IP = ""
	m.mdmCopier = &recordingMDMProfileCopier{}
	m.mdmResolveIP = func(context.Context, string, string) (string, error) {
		return "", errors.New(testResolverErrorSecret)
	}

	var rr *httptest.ResponseRecorder
	logs := captureMDMLog(t, func() {
		rr = performMDMProfileRequest(t, m, http.MethodPost, `{"name":"base"}`)
	})

	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	response := decodeMDMProfileResponse(t, rr, "ok", "name", "stage", "error")
	if response.Stage != mdm.StageIP || response.Error == "" {
		t.Fatalf("response=%#v", response)
	}
	assertMDMSecretSafe(t, rr.Body.String())
	assertMDMSecretSafe(t, logs)
}

func TestHandleMDMProfileUsesPerVMCredentialsWithoutHoldingManagerLock(t *testing.T) {
	m := newMDMHandlerManager()
	m.vms["base"].SSHUser = testVMSSHUser
	m.vms["base"].SSHPassword = testVMSSHPassword
	fake := &recordingMDMProfileCopier{
		checkLock: func() bool {
			if !m.mu.TryLock() {
				return false
			}
			m.mu.Unlock()
			return true
		},
	}
	m.mdmCopier = fake

	rr := performMDMProfileRequest(t, m, http.MethodPost, `{"name":"base"}`)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if fake.target.Username != testVMSSHUser || fake.target.Password != testVMSSHPassword {
		t.Fatalf("target=%#v", fake.target)
	}
	assertMDMSecretSafe(t, rr.Body.String())
}

func TestHandleMDMProfileMapsTypedStageErrorsSafely(t *testing.T) {
	tests := []struct {
		stage      mdm.Stage
		wantStatus int
		wantError  string
	}{
		{stage: mdm.StageConfiguration, wantStatus: http.StatusBadRequest, wantError: "profile configuration is incomplete"},
		{stage: mdm.StageVM, wantStatus: http.StatusBadRequest, wantError: "VM is not available"},
		{stage: mdm.StageIP, wantStatus: http.StatusBadGateway, wantError: "could not resolve VM IP"},
		{stage: mdm.StageAuthentication, wantStatus: http.StatusBadGateway, wantError: "SSH authentication failed"},
		{stage: mdm.StageSFTP, wantStatus: http.StatusBadGateway, wantError: "SFTP upload failed"},
		{stage: mdm.StageVerification, wantStatus: http.StatusBadGateway, wantError: "uploaded profile verification failed"},
	}

	for _, tt := range tests {
		t.Run(string(tt.stage), func(t *testing.T) {
			m := newMDMHandlerManager()
			m.mdmCopier = &recordingMDMProfileCopier{
				err: &mdm.StageError{Stage: tt.stage, Err: errors.New(testCopierErrorSecret)},
			}

			var rr *httptest.ResponseRecorder
			logs := captureMDMLog(t, func() {
				rr = performMDMProfileRequest(t, m, http.MethodPost, `{"name":"base"}`)
			})

			if rr.Code != tt.wantStatus {
				t.Fatalf("status=%d body=%s, want %d", rr.Code, rr.Body.String(), tt.wantStatus)
			}
			response := decodeMDMProfileResponse(t, rr, "ok", "name", "stage", "error")
			if response.Stage != tt.stage || response.Error != tt.wantError || response.OK {
				t.Fatalf("response=%#v", response)
			}
			assertMDMSecretSafe(t, rr.Body.String())
			assertMDMSecretSafe(t, logs)
		})
	}
}

func TestHandleMDMProfileMissingDependenciesFailSafely(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Manager)
	}{
		{name: "copier", mutate: func(m *Manager) { m.mdmCopier = nil }},
		{name: "resolver", mutate: func(m *Manager) {
			m.vms["base"].IP = ""
			m.mdmResolveIP = nil
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newMDMHandlerManager()
			m.mdmCopier = &recordingMDMProfileCopier{}
			m.mdmResolveIP = func(context.Context, string, string) (string, error) { return "192.0.2.10", nil }
			tt.mutate(m)
			rr := performMDMProfileRequest(t, m, http.MethodPost, `{"name":"base"}`)
			if rr.Code < 400 {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
			response := decodeMDMProfileResponse(t, rr, "ok", "name", "stage", "error")
			if response.OK || response.Error == "" {
				t.Fatalf("response=%#v", response)
			}
			assertMDMSecretSafe(t, rr.Body.String())
		})
	}
}

func TestResolveMDMIPWithTartHonorsRequestCancellation(t *testing.T) {
	m := &Manager{cfg: Config{TartAppPath: "/usr/bin/false"}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := m.resolveMDMIPWithTart(ctx, "base", "/tmp/tart-home")

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v, want context cancellation", err)
	}
}

func TestHandleMDMProfileWithNamedProfileAndCredentialOverrides(t *testing.T) {
	m := newMDMHandlerManager()
	m.cfg.JamfProfiles = []JamfProfile{
		{ID: "prod", Name: "Production", BaseURL: "https://prod.jamfcloud.com", InvitationCode: "prod-invite-999"},
		{ID: "sandbox", Name: "Sandbox", BaseURL: "https://sandbox.jamfcloud.com", InvitationCode: "sandbox-invite-111"},
	}
	fake := &recordingMDMProfileCopier{}
	m.mdmCopier = fake

	// Request deployment with Sandbox profile and custom SSH credentials
	reqBody := `{"name":"base","profileId":"sandbox","sshUser":"deploy-admin","sshPassword":"deploy-secret-password"}`
	rr := performMDMProfileRequest(t, m, http.MethodPost, reqBody)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if fake.target.Username != "deploy-admin" || fake.target.Password != "deploy-secret-password" {
		t.Fatalf("target=%#v, want deploy credentials", fake.target)
	}
	if err := mdm.ValidateProfile(fake.profile, mdm.ProfileInput{
		BaseURL:        "https://sandbox.jamfcloud.com",
		InvitationCode: "sandbox-invite-111",
	}, fake.payloadUUID); err != nil {
		t.Fatalf("profile was not generated from Sandbox configuration: %v", err)
	}
	response := decodeMDMProfileResponse(t, rr, "ok", "name", "path", "payloadUUID")
	if !response.OK || response.Name != "base" {
		t.Fatalf("response=%#v", response)
	}
}

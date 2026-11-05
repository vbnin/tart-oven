package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

	// Jamf School stores its network and location as write-only secrets, so they must
	// never come back out — not in a response, a log line, or the SSE snapshot. These
	// are deliberately obvious fakes: a real tenant's host, network or location id must
	// never land in this repo.
	testSchoolNetwork        = "school-network-write-only-0001"
	testSchoolLocation       = "school-location-write-only-0002"
	testSchoolFetchSecret    = "school-fetch-underlying-secret"
	testSchoolProfileSecret  = "school-profile-pkcs12-password-secret"
	testSchoolProfileService = "Profile Service"
)

// testSchoolProfileBytes imitates the real download: DER-encoded PKCS#7, which is why
// the handler cannot parse a payload UUID out of it, carrying something secret-shaped to
// prove the bytes themselves never reach output.
func testSchoolProfileBytes() []byte {
	return []byte("\x30\x82\x25\xd4 signed school profile " + testSchoolProfileSecret)
}

type recordingSchoolFetcher struct {
	in        mdm.SchoolInput
	profile   []byte
	err       error
	called    int
	lockHeld  bool
	checkLock func() bool
}

func (f *recordingSchoolFetcher) FetchProfile(_ context.Context, in mdm.SchoolInput) ([]byte, error) {
	f.called++
	f.in = in
	if f.checkLock != nil && !f.checkLock() {
		f.lockHeld = true
		return nil, errors.New("manager lock held during Jamf School profile fetch")
	}
	if f.err != nil {
		return nil, f.err
	}
	return append([]byte(nil), f.profile...), nil
}

// newSchoolMDMHandlerManager returns a Manager whose only configured Jamf server is a
// School one, so profile selection lands on it without a profileId in the request.
func newSchoolMDMHandlerManager() (*Manager, *recordingMDMProfileCopier, *recordingSchoolFetcher) {
	m := newMDMHandlerManager()
	m.cfg.JamfProfiles = []JamfProfile{{
		ID:   "school-1",
		Kind: JamfKindSchool,
		Name: "School Tenant",
		// Stored un-normalised on purpose: load() normalises only Kind, never the URL
		// or the secrets, so the handler is the only thing standing between a stored
		// value and the request it builds. A fixture that is already clean could not
		// tell whether the handler normalises or just forwards the raw field.
		BaseURL:  "  https://school.example/  ",
		Network:  "  " + testSchoolNetwork + "  ",
		Location: "\t" + testSchoolLocation + "\n",
	}}
	copier := &recordingMDMProfileCopier{}
	fetcher := &recordingSchoolFetcher{profile: testSchoolProfileBytes()}
	m.mdmCopier = copier
	m.schoolFetcher = fetcher
	return m, copier, fetcher
}

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
		agentProbedSinceBoot: map[string]bool{"base": true}, // no guest agent: SFTP path
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
		testSchoolNetwork,
		testSchoolLocation,
		testSchoolFetchSecret,
		testSchoolProfileSecret,
		`"password"`,
		"<plist",
	} {
		if strings.Contains(output, secret) {
			t.Fatalf("output leaked secret %q: %s", secret, output)
		}
	}
}

// assertSchoolSnapshotSafe is deliberately narrower than assertMDMSecretSafe. The SSE
// frame is a full-state snapshot, and configView publishes the SSH *username* by design
// — only the password is write-only — so the frame legitimately contains it. What must
// never appear there is the Jamf material: the write-only network and location, the
// invitation code, profile bytes, or the text of a fetch error.
func assertSchoolSnapshotSafe(t *testing.T, output string) {
	t.Helper()
	for _, secret := range []string{
		testSchoolNetwork,
		testSchoolLocation,
		testSchoolFetchSecret,
		testSchoolProfileSecret,
		testJamfInvitationCode,
		testGlobalSSHPassword,
		testVMSSHPassword,
	} {
		if strings.Contains(output, secret) {
			t.Fatalf("SSE snapshot leaked secret %q: %s", secret, output)
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

// TestSafeMDMStageErrorNamesTheFailedJamfSchoolStep is the whole diagnostic on the
// School path. Fetching a School profile is a scrape of three sequential requests, so
// when Jamf changes the enrollment markup the only actionable thing a user can be told
// is which of the three steps stopped working. Before this, all three stages fell into
// the default arm and reported "profile transfer failed".
func TestSafeMDMStageErrorNamesTheFailedJamfSchoolStep(t *testing.T) {
	tests := []struct {
		stage      mdm.Stage
		wantStatus int
		wantError  string
	}{
		{stage: mdm.StageLanding, wantStatus: http.StatusBadGateway, wantError: "could not reach the Jamf School enrollment page"},
		{stage: mdm.StageSession, wantStatus: http.StatusBadGateway, wantError: "Jamf School refused to start enrollment"},
		{stage: mdm.StageDownload, wantStatus: http.StatusBadGateway, wantError: "could not download the enrollment profile from Jamf School"},
	}

	seen := map[string]bool{}
	for _, tt := range tests {
		t.Run(string(tt.stage), func(t *testing.T) {
			status, message := safeMDMStageError(tt.stage)
			if status != tt.wantStatus || message != tt.wantError {
				t.Fatalf("safeMDMStageError(%q) = (%d, %q), want (%d, %q)",
					tt.stage, status, message, tt.wantStatus, tt.wantError)
			}
			if _, fallback := safeMDMStageError(mdm.Stage("no-such-stage")); message == fallback {
				t.Fatalf("stage %q still reports the generic fallback %q", tt.stage, fallback)
			}
			if seen[message] {
				t.Fatalf("stage %q reuses message %q from another stage", tt.stage, message)
			}
			seen[message] = true
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

// TestHandleMDMProfileFetchesSchoolProfileWithoutHoldingManagerLock is the central
// Phase 3 claim: a School server routes profile production to the fetcher instead of the
// local generator, the fetched bytes reach the copier unchanged, and the three HTTPS
// round-trips happen with m.mu released (tart.go:24 forbids network IO under the lock).
func TestHandleMDMProfileFetchesSchoolProfileWithoutHoldingManagerLock(t *testing.T) {
	m, copier, fetcher := newSchoolMDMHandlerManager()
	fetcher.checkLock = func() bool {
		if !m.mu.TryLock() {
			return false
		}
		m.mu.Unlock()
		return true
	}
	m.mdmResolveIP = func(context.Context, string, string) (string, error) {
		t.Fatal("resolver called despite cached VM IP")
		return "", nil
	}

	rr := performMDMProfileRequest(t, m, http.MethodPost, `{"name":"base"}`)

	if fetcher.lockHeld {
		t.Fatal("manager lock was held across the Jamf School fetch")
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if fetcher.called != 1 {
		t.Fatalf("fetcher called %d times, want exactly 1 (a fetch mints a fresh identity)", fetcher.called)
	}
	wantIn := mdm.SchoolInput{BaseURL: "https://school.example", Network: testSchoolNetwork, Location: testSchoolLocation}
	if fetcher.in != wantIn {
		t.Fatalf("fetcher input=%#v, want %#v", fetcher.in, wantIn)
	}
	if !copier.called || !bytes.Equal(copier.profile, testSchoolProfileBytes()) {
		t.Fatalf("copier got profile %q, want the fetched bytes verbatim", copier.profile)
	}
	// The server minted this profile and it is signed DER we do not parse, so there is
	// no UUID to assert; CopyAndVerify's byte comparison is the verification.
	if copier.payloadUUID != "" {
		t.Fatalf("payloadUUID=%q, want empty for a School profile", copier.payloadUUID)
	}
	response := decodeMDMProfileResponse(t, rr, "ok", "name", "path")
	if !response.OK || response.Name != "base" || response.Path != mdm.ProfileDisplayPath {
		t.Fatalf("response=%#v", response)
	}
	assertMDMSecretSafe(t, rr.Body.String())
}

// TestHandleMDMProfileKeepsProProfilesOnTheGenerator pins the other half of the branch:
// adding the School path must not divert a Pro server to the network.
func TestHandleMDMProfileKeepsProProfilesOnTheGenerator(t *testing.T) {
	m, copier, fetcher := newSchoolMDMHandlerManager()
	m.cfg.JamfProfiles[0] = JamfProfile{
		ID: "pro-1", Kind: JamfKindPro, Name: "Pro Tenant",
		BaseURL: "https://pro.example", InvitationCode: testJamfInvitationCode,
	}

	rr := performMDMProfileRequest(t, m, http.MethodPost, `{"name":"base"}`)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if fetcher.called != 0 {
		t.Fatalf("School fetcher called %d times for a Pro profile", fetcher.called)
	}
	if copier.payloadUUID == "" {
		t.Fatal("Pro profile copied with no payloadUUID")
	}
	if err := mdm.ValidateProfile(copier.profile, mdm.ProfileInput{
		BaseURL:        "https://pro.example",
		InvitationCode: testJamfInvitationCode,
	}, copier.payloadUUID); err != nil {
		t.Fatalf("Pro profile was not locally generated: %v", err)
	}
}

// TestHandleMDMProfileRejectsIncompleteSchoolConfiguration covers the validation branch.
// A School profile needs a network and a location where Pro needs an invitation code,
// and an invitation code must not satisfy School (Phase 1 wipes the other kind's secrets
// on a kind switch, so one being present is a stale-data signal, not a configuration).
func TestHandleMDMProfileRejectsIncompleteSchoolConfiguration(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*JamfProfile)
	}{
		{name: "missing network", mutate: func(p *JamfProfile) { p.Network = "" }},
		{name: "blank network", mutate: func(p *JamfProfile) { p.Network = "   " }},
		{name: "missing location", mutate: func(p *JamfProfile) { p.Location = "" }},
		{name: "blank location", mutate: func(p *JamfProfile) { p.Location = "  " }},
		{name: "missing base URL", mutate: func(p *JamfProfile) { p.BaseURL = "" }},
		{
			name: "an invitation code does not substitute for network and location",
			mutate: func(p *JamfProfile) {
				p.Network, p.Location = "", ""
				p.InvitationCode = testJamfInvitationCode
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, copier, fetcher := newSchoolMDMHandlerManager()
			tt.mutate(&m.cfg.JamfProfiles[0])

			rr := performMDMProfileRequest(t, m, http.MethodPost, `{"name":"base"}`)

			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s, want 400", rr.Code, rr.Body.String())
			}
			response := decodeMDMProfileResponse(t, rr, "ok", "name", "stage", "error")
			if response.OK || response.Stage != mdm.StageConfiguration {
				t.Fatalf("response=%#v, want a configuration-stage failure", response)
			}
			if fetcher.called != 0 {
				t.Fatal("fetched a School profile despite incomplete configuration")
			}
			if copier.called {
				t.Fatal("copier called despite incomplete configuration")
			}
			assertMDMSecretSafe(t, rr.Body.String())
		})
	}
}

// TestHandleMDMProfileMapsSchoolFetchStageErrors is the end-to-end counterpart of
// TestSafeMDMStageErrorNamesTheFailedJamfSchoolStep: it proves the stage a School error
// carries survives the handler and that nothing from the error reaches the client.
func TestHandleMDMProfileMapsSchoolFetchStageErrors(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantStage  mdm.Stage
		wantError  string
	}{
		{
			name:       "landing",
			err:        &mdm.StageError{Stage: mdm.StageLanding, Err: errors.New(testSchoolFetchSecret)},
			wantStatus: http.StatusBadGateway,
			wantStage:  mdm.StageLanding,
			wantError:  "could not reach the Jamf School enrollment page",
		},
		{
			name:       "session",
			err:        &mdm.StageError{Stage: mdm.StageSession, Err: errors.New(testSchoolFetchSecret)},
			wantStatus: http.StatusBadGateway,
			wantStage:  mdm.StageSession,
			wantError:  "Jamf School refused to start enrollment",
		},
		{
			name:       "download",
			err:        &mdm.StageError{Stage: mdm.StageDownload, Err: errors.New(testSchoolFetchSecret)},
			wantStatus: http.StatusBadGateway,
			wantStage:  mdm.StageDownload,
			wantError:  "could not download the enrollment profile from Jamf School",
		},
		{
			name:       "configuration",
			err:        &mdm.StageError{Stage: mdm.StageConfiguration, Err: errors.New(testSchoolFetchSecret)},
			wantStatus: http.StatusBadRequest,
			wantStage:  mdm.StageConfiguration,
			wantError:  "profile configuration is incomplete",
		},
		{
			// internal/mdm types every error on this path, so this is a floor rather
			// than a diagnosis: an untyped error must still fail closed, not 200.
			name:       "untyped error still fails closed",
			err:        errors.New(testSchoolFetchSecret),
			wantStatus: http.StatusBadGateway,
			wantStage:  mdm.StageDownload,
			wantError:  "could not download the enrollment profile from Jamf School",
		},
		{
			// A wrapped error must still be recognised: school.go returns wrapped
			// stage errors and errors.As has to see through the wrapping.
			name:       "wrapped stage error",
			err:        fmt.Errorf("fetch profile: %w", &mdm.StageError{Stage: mdm.StageSession, Err: errors.New(testSchoolFetchSecret)}),
			wantStatus: http.StatusBadGateway,
			wantStage:  mdm.StageSession,
			wantError:  "Jamf School refused to start enrollment",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, copier, fetcher := newSchoolMDMHandlerManager()
			fetcher.err = tt.err

			var rr *httptest.ResponseRecorder
			logs := captureMDMLog(t, func() {
				rr = performMDMProfileRequest(t, m, http.MethodPost, `{"name":"base"}`)
			})

			if rr.Code != tt.wantStatus {
				t.Fatalf("status=%d body=%s, want %d", rr.Code, rr.Body.String(), tt.wantStatus)
			}
			response := decodeMDMProfileResponse(t, rr, "ok", "name", "stage", "error")
			if response.OK || response.Stage != tt.wantStage || response.Error != tt.wantError {
				t.Fatalf("response=%#v, want stage %q error %q", response, tt.wantStage, tt.wantError)
			}
			if copier.called {
				t.Fatal("copier called after a failed School fetch")
			}
			assertMDMSecretSafe(t, rr.Body.String())
			assertMDMSecretSafe(t, logs)
		})
	}
}

// TestHandleMDMProfileFailsSafelyWithoutASchoolFetcher mirrors the nil-copier guard: a
// Manager built without run.go's wiring must refuse a School request rather than panic.
func TestHandleMDMProfileFailsSafelyWithoutASchoolFetcher(t *testing.T) {
	m, copier, _ := newSchoolMDMHandlerManager()
	m.schoolFetcher = nil

	rr := performMDMProfileRequest(t, m, http.MethodPost, `{"name":"base"}`)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s, want 500", rr.Code, rr.Body.String())
	}
	response := decodeMDMProfileResponse(t, rr, "ok", "name", "stage", "error")
	if response.OK || response.Error == "" {
		t.Fatalf("response=%#v", response)
	}
	if copier.called {
		t.Fatal("copier called with no School fetcher")
	}
	assertMDMSecretSafe(t, rr.Body.String())
}

// TestHandleMDMProfileDoesNotMintASchoolProfileItCannotDeliver guards the ordering of the
// copier-availability check against profile production. Every School fetch asks the
// tenant to mint a fresh PKCS#12 identity, which becomes a device record in Jamf School,
// so a request that has no way to deliver the result must fail BEFORE it asks. Checking
// afterwards would orphan an identity in the tenant on every attempt.
//
// Status alone does not test this — it is 500 under either ordering. fetcher.called is
// the assertion that matters.
func TestHandleMDMProfileDoesNotMintASchoolProfileItCannotDeliver(t *testing.T) {
	m, _, fetcher := newSchoolMDMHandlerManager()
	m.mdmCopier = nil // no SFTP copier, and this VM has no guest agent either

	rr := performMDMProfileRequest(t, m, http.MethodPost, `{"name":"base"}`)

	if fetcher.called != 0 {
		t.Fatalf("fetched (and so minted) a School profile %d time(s) with no way to deliver it", fetcher.called)
	}
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s, want 500", rr.Code, rr.Body.String())
	}
	response := decodeMDMProfileResponse(t, rr, "ok", "name", "stage", "error")
	if response.OK || response.Stage != mdm.StageSFTP {
		t.Fatalf("response=%#v, want a failure at stage %q", response, mdm.StageSFTP)
	}
	assertMDMSecretSafe(t, rr.Body.String())
}

// TestHandleMDMProfileRoutesSchoolThroughTheGuestAgentPath covers the viaAgent × School
// combination, which every other School test here excludes: the shared fixture marks the
// guest agent unavailable, forcing SFTP. For an agent-capable VM the agent path is the
// DEFAULT, so this is the common case in production, not an edge.
//
// What it pins: a School profile is still fetched on this path, the SFTP copier is
// bypassed entirely, and a missing SFTP copier does not block an agent-capable VM (the
// guard is `m.mdmCopier == nil && !viaAgent`). The copy itself then runs through
// copyProfileViaAgent, which shells out to `tart exec`; TartAppPath points at nothing
// here, so it fails at the agent stage, which is what lets this test assert the routing
// without a running VM.
//
// NOTE: this does NOT prove a School profile survives the agent round-trip.
// copyProfileViaAgent verifies with `res.Stdout != string(profile)` over `tart exec`
// stdout, and a School profile is DER with NUL bytes and invalid UTF-8 where a Pro
// profile is XML. Whether that channel is byte-transparent is unverified and needs a
// live guest; see the plan's "Phase 3 — unresolved" note.
func TestHandleMDMProfileRoutesSchoolThroughTheGuestAgentPath(t *testing.T) {
	m, copier, fetcher := newSchoolMDMHandlerManager()
	// agentProbedSinceBoot records UNavailability, so false = the agent answered.
	m.agentProbedSinceBoot = map[string]bool{"base": false}
	m.mdmCopier = nil // must not be required when the guest agent is in play
	m.cfg.TartAppPath = "/nonexistent/tart"

	var rr *httptest.ResponseRecorder
	logs := captureMDMLog(t, func() {
		rr = performMDMProfileRequest(t, m, http.MethodPost, `{"name":"base"}`)
	})

	if fetcher.called != 1 {
		t.Fatalf("fetcher called %d times on the guest-agent path, want 1", fetcher.called)
	}
	if copier.called {
		t.Fatal("SFTP copier used despite an available guest agent")
	}
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s, want 502 from the unavailable agent", rr.Code, rr.Body.String())
	}
	response := decodeMDMProfileResponse(t, rr, "ok", "name", "stage", "error")
	if response.OK || response.Stage != mdm.StageAgent {
		t.Fatalf("response=%#v, want a failure at stage %q", response, mdm.StageAgent)
	}
	assertMDMSecretSafe(t, rr.Body.String())
	assertMDMSecretSafe(t, logs)
	assertSchoolSnapshotSafe(t, string(m.snapshotJSON()))
}

// TestHandleMDMProfileSchoolKeepsSecretsOutOfTaskOutputAndLogs is the Task.Output / m.logs
// half of the secret-hygiene invariant; internal/mdm's own tests cover only the errors it
// returns, not what internal/server does with them.
//
// A School profile is a credential: it embeds a PKCS#12 identity and that identity's
// cleartext password, and the network and location that produced it are stored write-only.
// The SSE frame is a full-state snapshot that carries m.logs and every Task verbatim to
// every connected browser, so a single logln of an error or a profile body would publish
// all of it. Nothing on this path writes to either today; this test is what makes that a
// decision rather than an accident.
func TestHandleMDMProfileSchoolKeepsSecretsOutOfTaskOutputAndLogs(t *testing.T) {
	cases := []struct {
		name     string
		fetchErr error
	}{
		{name: "successful fetch"},
		{name: "failed fetch", fetchErr: &mdm.StageError{Stage: mdm.StageSession, Err: errors.New(testSchoolFetchSecret)}},
		{name: "failed fetch naming a Jamf Pro profile", fetchErr: &mdm.StageError{
			Stage: mdm.StageDownload,
			Err:   errors.New("downloaded profile is a " + testSchoolProfileService + " profile: " + testSchoolFetchSecret),
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, _, fetcher := newSchoolMDMHandlerManager()
			fetcher.err = tc.fetchErr
			m.tasks = []*Task{{ID: "t1", Kind: "create", Target: "base", Status: "success"}}

			logs := captureMDMLog(t, func() {
				performMDMProfileRequest(t, m, http.MethodPost, `{"name":"base"}`)
			})

			assertMDMSecretSafe(t, logs)
			for _, line := range m.logs {
				assertMDMSecretSafe(t, line)
			}
			for _, task := range m.tasks {
				assertMDMSecretSafe(t, task.Output)
				assertMDMSecretSafe(t, task.Error)
			}
			// The SSE frame is what actually reaches a browser: assert the whole
			// serialized snapshot, which carries logs, tasks and the config view.
			assertSchoolSnapshotSafe(t, string(m.snapshotJSON()))
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

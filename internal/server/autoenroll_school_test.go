package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"tart-oven/internal/mdm"
	"testing"
)

// These tests cover Jamf School in the auto-enroll path (copyMDMProfileToVM), mirroring
// the handler tests in mdm_handler_test.go. The hazards are the same, but the sinks are
// not: handleMDMProfile writes only a canned message to the HTTP response, whereas
// auto-enroll's error text lands in Task.Error and m.logs (and so every browser's SSE
// frame) and in the process log.

// TestCopyMDMProfileToVMFetchesSchoolProfileWithoutHoldingManagerLock covers the routing,
// the trimmed fetch input, the verbatim delivery and the lock discipline in one pass.
// Both ways of selecting a profile are exercised: an explicit id, and the empty id that
// falls back to the first configured profile (which used to fail for a School server).
func TestCopyMDMProfileToVMFetchesSchoolProfileWithoutHoldingManagerLock(t *testing.T) {
	for _, profileID := range []string{"school-1", ""} {
		t.Run(fmt.Sprintf("profileID=%q", profileID), func(t *testing.T) {
			m, copier, fetcher := newSchoolMDMHandlerManager()
			fetcher.checkLock = func() bool {
				if !m.mu.TryLock() {
					return false
				}
				m.mu.Unlock()
				return true
			}

			path, uuid, err := m.copyMDMProfileToVM(context.Background(), "base", profileID, "", "")

			if fetcher.lockHeld {
				t.Fatal("manager lock was held across the Jamf School fetch")
			}
			if err != nil {
				t.Fatalf("copyMDMProfileToVM: %v", err)
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
			// The server minted this profile and it is signed DER we do not parse.
			if uuid != "" || copier.payloadUUID != "" {
				t.Fatalf("payloadUUID returned=%q copied=%q, want empty for a School profile", uuid, copier.payloadUUID)
			}
			if path != mdm.ProfileDisplayPath {
				t.Fatalf("path=%q, want %q", path, mdm.ProfileDisplayPath)
			}
		})
	}
}

// TestCopyMDMProfileToVMKeepsProProfilesOnTheGenerator pins the other half of the branch:
// adding the School path must not divert a Pro server to the network.
func TestCopyMDMProfileToVMKeepsProProfilesOnTheGenerator(t *testing.T) {
	m, copier, fetcher := newSchoolMDMHandlerManager()
	m.cfg.JamfProfiles[0] = JamfProfile{
		ID: "pro-1", Kind: JamfKindPro, Name: "Pro Tenant",
		BaseURL: "https://pro.example", InvitationCode: testJamfInvitationCode,
	}

	_, uuid, err := m.copyMDMProfileToVM(context.Background(), "base", "pro-1", "", "")

	if err != nil {
		t.Fatalf("copyMDMProfileToVM: %v", err)
	}
	if fetcher.called != 0 {
		t.Fatalf("School fetcher called %d times for a Pro profile", fetcher.called)
	}
	if uuid == "" || copier.payloadUUID != uuid {
		t.Fatalf("Pro profile copied with payloadUUID %q (returned %q)", copier.payloadUUID, uuid)
	}
	if err := mdm.ValidateProfile(copier.profile, mdm.ProfileInput{
		BaseURL:        "https://pro.example",
		InvitationCode: testJamfInvitationCode,
	}, uuid); err != nil {
		t.Fatalf("Pro profile was not locally generated: %v", err)
	}
}

// TestCopyMDMProfileToVMRejectsIncompleteConfigurationByKind: the configuration
// gate asks each kind for its own secrets, and its message names them. A School profile
// must never be told to supply an invitation code it has no field for.
func TestCopyMDMProfileToVMRejectsIncompleteConfigurationByKind(t *testing.T) {
	school := []struct {
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
	for _, tt := range school {
		t.Run("school/"+tt.name, func(t *testing.T) {
			m, copier, fetcher := newSchoolMDMHandlerManager()
			tt.mutate(&m.cfg.JamfProfiles[0])

			_, _, err := m.copyMDMProfileToVM(context.Background(), "base", "school-1", "", "")

			if err == nil {
				t.Fatal("expected a configuration error")
			}
			msg := strings.ToLower(err.Error())
			if !strings.Contains(msg, "incomplete") || !strings.Contains(msg, "network") || !strings.Contains(msg, "location") {
				t.Fatalf("error %q should say the configuration is incomplete and name network and location", err)
			}
			if strings.Contains(msg, "invitation") {
				t.Fatalf("error %q sends a School profile looking for an invitation code", err)
			}
			if fetcher.called != 0 || copier.called {
				t.Fatalf("fetched=%d copied=%v despite incomplete configuration", fetcher.called, copier.called)
			}
			assertMDMSecretSafe(t, err.Error())
		})
	}

	t.Run("pro still names the invitation code", func(t *testing.T) {
		m, _, fetcher := newSchoolMDMHandlerManager()
		m.cfg.JamfProfiles[0] = JamfProfile{ID: "pro-1", Kind: JamfKindPro, BaseURL: "https://pro.example"}

		_, _, err := m.copyMDMProfileToVM(context.Background(), "base", "pro-1", "", "")

		if err == nil {
			t.Fatal("expected a configuration error")
		}
		msg := strings.ToLower(err.Error())
		if !strings.Contains(msg, "invitation code") || strings.Contains(msg, "network") {
			t.Fatalf("error %q should name the invitation code, not School's secrets", err)
		}
		if fetcher.called != 0 {
			t.Fatal("School fetcher called for a Pro profile")
		}
	})
}

// TestCopyMDMProfileToVMFailsSafelyWithoutASchoolFetcher: a Manager built without
// run.go's wiring must refuse a School profile rather than panic on a nil interface.
func TestCopyMDMProfileToVMFailsSafelyWithoutASchoolFetcher(t *testing.T) {
	m, copier, _ := newSchoolMDMHandlerManager()
	m.schoolFetcher = nil

	_, _, err := m.copyMDMProfileToVM(context.Background(), "base", "school-1", "", "")

	if err == nil {
		t.Fatal("expected an error with no School fetcher")
	}
	if copier.called {
		t.Fatal("copier called with no School fetcher")
	}
	assertMDMSecretSafe(t, err.Error())
}

// TestCopyMDMProfileToVMDoesNotMintASchoolProfileItCannotDeliver: every School fetch asks
// the tenant to mint a fresh PKCS#12 identity, which becomes a device record. Anything
// that makes delivery impossible has to be discovered BEFORE asking.
//
// Each case is only meaningful next to the positive control above (the same fixture with
// delivery possible does fetch exactly once); on its own "never fetched" is also what a
// School profile dying early at the configuration gate looks like.
func TestCopyMDMProfileToVMDoesNotMintASchoolProfileItCannotDeliver(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Manager)
	}{
		{name: "no SFTP copier and no guest agent", mutate: func(m *Manager) { m.mdmCopier = nil }},
		{name: "VM not running", mutate: func(m *Manager) { m.vms["base"].State = "stopped" }},
		{name: "VM IP cannot be resolved", mutate: func(m *Manager) {
			m.vms["base"].IP = ""
			m.mdmResolveIP = func(context.Context, string, string) (string, error) {
				return "", errors.New(testResolverErrorSecret)
			}
		}},
		{name: "no IP and no resolver", mutate: func(m *Manager) { m.vms["base"].IP = "" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, copier, fetcher := newSchoolMDMHandlerManager()
			tt.mutate(m)

			_, _, err := m.copyMDMProfileToVM(context.Background(), "base", "school-1", "", "")

			if err == nil {
				t.Fatal("expected a delivery error")
			}
			if fetcher.called != 0 {
				t.Fatalf("fetched (and so minted) a School profile %d time(s) with no way to deliver it", fetcher.called)
			}
			if copier.called {
				t.Fatal("copier called")
			}
		})
	}
}

// TestCopyMDMProfileToVMMapsSchoolFailuresToCannedMessages: err.Error() from here becomes
// Task.Error, an m.logs line and the process log, so for School it must be built from the
// failed stage alone — never from the error's own text.
func TestCopyMDMProfileToVMMapsSchoolFailuresToCannedMessages(t *testing.T) {
	stageErr := func(stage mdm.Stage) error {
		return &mdm.StageError{Stage: stage, Err: errors.New(testSchoolFetchSecret)}
	}
	fetchFailures := []struct {
		name string
		err  error
		want string
	}{
		{"landing", stageErr(mdm.StageLanding), "could not reach the Jamf School enrollment page"},
		{"session", stageErr(mdm.StageSession), "Jamf School refused to start enrollment"},
		{"download", stageErr(mdm.StageDownload), "could not download the enrollment profile from Jamf School"},
		{"untyped error fails closed at download", errors.New(testSchoolFetchSecret), "could not download the enrollment profile from Jamf School"},
		{"wrapped stage error is still recognised", fmt.Errorf("fetch profile: %w", stageErr(mdm.StageSession)), "Jamf School refused to start enrollment"},
	}
	for _, tt := range fetchFailures {
		t.Run("fetch/"+tt.name, func(t *testing.T) {
			m, copier, fetcher := newSchoolMDMHandlerManager()
			fetcher.err = tt.err

			_, _, err := m.copyMDMProfileToVM(context.Background(), "base", "school-1", "", "")

			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error=%v, want it to say %q", err, tt.want)
			}
			if copier.called {
				t.Fatal("copier called after a failed School fetch")
			}
			assertMDMSecretSafe(t, err.Error())
		})
	}

	// The copier's own text (an SSH address, a remote path, whatever the transport said)
	// gets the same treatment: the profile it was handed is a credential.
	t.Run("copy failure over SFTP", func(t *testing.T) {
		m, copier, _ := newSchoolMDMHandlerManager()
		copier.err = &mdm.StageError{Stage: mdm.StageVerification, Err: errors.New(testCopierErrorSecret)}

		_, _, err := m.copyMDMProfileToVM(context.Background(), "base", "school-1", "", "")

		if err == nil || !strings.Contains(err.Error(), "uploaded profile verification failed") {
			t.Fatalf("error=%v, want the canned verification message", err)
		}
		assertMDMSecretSafe(t, err.Error())
	})
	t.Run("untyped copy failure over SFTP", func(t *testing.T) {
		m, copier, _ := newSchoolMDMHandlerManager()
		copier.err = errors.New(testCopierErrorSecret)

		_, _, err := m.copyMDMProfileToVM(context.Background(), "base", "school-1", "", "")

		if err == nil || !strings.Contains(err.Error(), "SFTP upload failed") {
			t.Fatalf("error=%v, want the canned SFTP message", err)
		}
		assertMDMSecretSafe(t, err.Error())
	})
}

// TestAutoEnrollSchoolFailuresKeepSecretsOutOfTaskAndLogs drives the real task runner, so
// it covers the sinks that matter: Task.Error, Task.Output and m.logs, plus the whole
// serialized SSE frame that carries them to every connected browser.
//
// The second case is the one a fetch-only test would miss: the guest's own stderr, which
// copyProfileViaAgent folds into its error. The base64 of the profile is on that command
// line, so a shell that echoes a failed command puts the credential in the error text.
func TestAutoEnrollSchoolFailuresKeepSecretsOutOfTaskAndLogs(t *testing.T) {
	profileB64 := base64.StdEncoding.EncodeToString(testSchoolProfileBytes())
	cases := []struct {
		name      string
		tart      string
		fetchErr  error
		wantInErr string
	}{
		{
			name:      "failed fetch",
			tart:      `exit 0`,
			fetchErr:  &mdm.StageError{Stage: mdm.StageSession, Err: errors.New(testSchoolFetchSecret)},
			wantInErr: "Jamf School refused to start enrollment",
		},
		{
			name: "guest write failure that echoes the command",
			tart: `for a; do last=$a; done
case "$last" in *base64*) echo "sh: failed: $last" >&2; exit 1 ;; esac
exit 0`,
			wantInErr: "guest agent upload failed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newAgentOnlyManager(t, tc.tart)
			fetcher := &recordingSchoolFetcher{profile: testSchoolProfileBytes(), err: tc.fetchErr}
			m.schoolFetcher = fetcher
			m.cfg.JamfProfiles = []JamfProfile{{
				ID: "school-1", Kind: JamfKindSchool, Name: "School Tenant",
				BaseURL: "https://school.example", Network: testSchoolNetwork, Location: testSchoolLocation,
			}}

			logs := captureMDMLog(t, func() { m.runAutoEnrollTask("vm", "test", "school-1") })

			if len(m.tasks) != 1 || m.tasks[0].Status != "error" {
				t.Fatalf("tasks=%+v, want one failed task", m.tasks)
			}
			task := m.tasks[0]
			if !strings.Contains(task.Error, tc.wantInErr) {
				t.Fatalf("task error=%q, want it to say %q", task.Error, tc.wantInErr)
			}
			sinks := map[string]string{
				"Task.Error":  task.Error,
				"Task.Output": task.Output,
				"process log": logs,
				"m.logs":      strings.Join(m.logs, "\n"),
				"SSE frame":   string(m.snapshotJSON()),
			}
			for sink, text := range sinks {
				if strings.Contains(text, profileB64) || strings.Contains(text, testSchoolProfileSecret) {
					t.Fatalf("%s leaked the profile: %s", sink, text)
				}
				if sink == "SSE frame" {
					assertSchoolSnapshotSafe(t, text)
				} else {
					assertMDMSecretSafe(t, text)
				}
			}
		})
	}
}

// TestSchoolProfilePushThroughTheGuestAgentWritesTheFetchedBytes is the integration view
// of the guest-agent route, which is the default for an agent-capable VM: no SFTP copier
// and no SSH credentials, the DER blob written byte-for-byte, no UUID to check.
func TestSchoolProfilePushThroughTheGuestAgentWritesTheFetchedBytes(t *testing.T) {
	home := t.TempDir()
	m := newAgentOnlyManager(t, fakeAgentTart(home))
	m.cfg.SSHUser, m.cfg.SSHPassword = "", ""
	m.cfg.JamfProfiles = []JamfProfile{{
		ID: "school-1", Kind: JamfKindSchool, Name: "School Tenant",
		BaseURL: "https://school.example", Network: testSchoolNetwork, Location: testSchoolLocation,
	}}
	fetcher := &recordingSchoolFetcher{profile: testSchoolProfileBytes()}
	m.schoolFetcher = fetcher

	path, uuid, err := m.copyMDMProfileToVM(t.Context(), "vm", "school-1", "", "")

	if err != nil {
		t.Fatalf("copyMDMProfileToVM: %v", err)
	}
	if fetcher.called != 1 {
		t.Fatalf("fetcher called %d times, want 1", fetcher.called)
	}
	if uuid != "" || path != mdm.ProfileDisplayPath {
		t.Fatalf("path=%q uuid=%q", path, uuid)
	}
	data, err := os.ReadFile(filepath.Join(home, "Desktop", "mdm_enroll.mobileconfig"))
	if err != nil {
		t.Fatalf("profile not written: %v", err)
	}
	if !bytes.Equal(data, testSchoolProfileBytes()) {
		t.Fatalf("guest holds %q, want the fetched bytes verbatim", data)
	}
}

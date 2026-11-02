package server

import (
	"os"
	"path/filepath"
	"strings"
	"tart-oven/internal/mdm"
	"testing"
	"time"
)

// newAgentOnlyManager returns a Manager whose only way into the guest is a fake
// `tart exec` (the guest agent): the SSH fallback is off, so any code path that
// still insisted on SSH would fail instead of silently working.
func newAgentOnlyManager(t *testing.T, tartScript string) *Manager {
	t.Helper()
	m := newTestManager(t)
	bin := filepath.Join(t.TempDir(), "tart")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+tartScript), 0o755); err != nil {
		t.Fatal(err)
	}
	m.cfg.TartAppPath = bin
	m.cfg.SSHFallbackEnabled = false
	m.vms["vm"] = &VM{Name: "vm", State: "running"}
	return m
}

func TestEnrollReadinessWorksThroughTheGuestAgentWithoutSSH(t *testing.T) {
	m := newAgentOnlyManager(t, `case "$*" in
  *autoLoginUser*) echo admin ;;
  *osascript*) exit 0 ;;
  *mdm_enroll*) echo yes ;;
esac
exit 0`)
	got := m.checkEnrollReadiness(t.Context(), "vm")
	want := enrollReadiness{Autologin: "enabled", Accessibility: "granted", Profile: "present"}
	if got != want {
		t.Fatalf("readiness = %+v, want %+v", got, want)
	}
}

func TestPrepGoldenImageRunsThroughTheGuestAgentWithoutSSH(t *testing.T) {
	m := newAgentOnlyManager(t, `case "$*" in
  *prep_golden_image*) echo RESULT=READY ;;
esac
exit 0`)
	m.runPrepGoldenImageTask("vm")
	if len(m.tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(m.tasks))
	}
	for _, task := range m.tasks {
		if task.Status != "success" {
			t.Fatalf("task status = %q (%s), want success", task.Status, task.Error)
		}
	}
}

func TestWaitForGuestReadyUsesTheGuestAgent(t *testing.T) {
	m := newAgentOnlyManager(t, `exit 0`)
	if !m.waitForGuestReady(t.Context(), "vm", 5*time.Second) {
		t.Fatal("a guest with a working agent and no SSH should count as ready")
	}
}

// fakeAgentTart runs whatever command `tart exec` is given on the host with
// HOME pointed at home, standing in for a guest whose Desktop is home/Desktop.
func fakeAgentTart(home string) string {
	return `for a; do last=$a; done
HOME="` + home + `" exec /bin/sh -c "$last"`
}

func TestProfilePushThroughTheGuestAgentNeedsNoSSHOrPassword(t *testing.T) {
	home := t.TempDir()
	m := newAgentOnlyManager(t, fakeAgentTart(home))
	m.cfg.SSHUser, m.cfg.SSHPassword = "", "" // nothing SFTP could use
	m.cfg.JamfBaseURL, m.cfg.JamfInvitationCode = "https://jamf.example", "invite-123"
	m.mdmCopier = &recordingMDMProfileCopier{} // would be used (and recorded) on the SFTP path

	path, uuid, err := m.copyMDMProfileToVM(t.Context(), "vm", "", "", "")
	if err != nil {
		t.Fatalf("copyMDMProfileToVM: %v", err)
	}
	if m.mdmCopier.(*recordingMDMProfileCopier).called {
		t.Fatal("SFTP copier used although the guest agent answers")
	}
	data, err := os.ReadFile(filepath.Join(home, "Desktop", "mdm_enroll.mobileconfig"))
	if err != nil {
		t.Fatalf("profile not written: %v", err)
	}
	if err := mdm.ValidateProfile(data, mdm.ProfileInput{BaseURL: "https://jamf.example", InvitationCode: "invite-123"}, uuid); err != nil {
		t.Fatalf("written profile invalid: %v", err)
	}
	if path != mdm.ProfileDisplayPath {
		t.Fatalf("path = %q", path)
	}
	info, _ := os.Stat(filepath.Join(home, "Desktop", "mdm_enroll.mobileconfig"))
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestProfilePushReportsAgentWriteFailure(t *testing.T) {
	// The probe ("true") succeeds, but writing to the Desktop fails.
	m := newAgentOnlyManager(t, `for a; do last=$a; done
case "$last" in *base64*) echo "disk full" >&2; exit 1 ;; esac
exec /bin/sh -c "$last"`)
	m.cfg.JamfBaseURL, m.cfg.JamfInvitationCode = "https://jamf.example", "invite-123"
	_, _, err := m.copyMDMProfileToVM(t.Context(), "vm", "", "", "")
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err = %v, want the guest's write error", err)
	}
}

func TestHandleMDMProfileThroughTheGuestAgent(t *testing.T) {
	home := t.TempDir()
	m := newAgentOnlyManager(t, fakeAgentTart(home))
	m.cfg.SSHUser, m.cfg.SSHPassword = "", ""
	m.cfg.JamfBaseURL, m.cfg.JamfInvitationCode = "https://jamf.example", "invite-123"

	rr := performMDMProfileRequest(t, m, "POST", `{"name":"vm"}`)
	if rr.Code != 200 {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if _, err := os.Stat(filepath.Join(home, "Desktop", "mdm_enroll.mobileconfig")); err != nil {
		t.Fatalf("profile not written: %v", err)
	}
}

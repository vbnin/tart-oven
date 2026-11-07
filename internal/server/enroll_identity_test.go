package server

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDesktopProfileDecision(t *testing.T) {
	const mine = "aaaa"
	for _, c := range []struct {
		name, probe, recorded string
		reuse, foreign        bool
	}{
		{"nothing there", "no\n", "", false, false},
		{"a profile without an identity (Jamf Pro)", "yes\n", "", true, false},
		{"a profile without an identity, even if a School one is recorded", "yes\n", mine, true, false},
		{"this VM's own School identity", "identity aaaa\n", mine, true, false},
		{"a School identity that was copied from a base VM", "identity bbbb\n", "", false, true},
		{"a School identity that is not the recorded one", "identity bbbb\n", mine, false, true},
		{"noise before the answer", "Last login: today\nidentity bbbb\n", mine, false, true},
		{"an identity that could not be fingerprinted", "identity\n", mine, false, true},
		{"an unreadable answer is treated as nothing usable", "banana\n", mine, false, false},
		{"an empty answer", "", mine, false, false},
	} {
		reuse, foreign := desktopProfileDecision(c.probe, c.recorded)
		if reuse != c.reuse || foreign != c.foreign {
			t.Errorf("%s: got reuse=%v foreign=%v, want reuse=%v foreign=%v", c.name, reuse, foreign, c.reuse, c.foreign)
		}
	}
}

// fakeEnrollGuest runs only the commands that touch the enrollment profile (probe,
// push, checksum) against a temp HOME. Everything else, including the real
// enrollment script that drives the UI, is answered without being executed.
func fakeEnrollGuest(home string) string {
	return `for a; do last=$a; done
case "$last" in
  *auto_enroll.log*) echo RESULT=ENROLLED; exit 0 ;;
  *mdm_enroll.mobileconfig*) HOME="` + home + `" exec /bin/sh -c "$last" ;;
esac
exit 0`
}

func newEnrollIdentityManager(t *testing.T, desktopProfile []byte, recorded string) (*Manager, *recordingSchoolFetcher, string) {
	t.Helper()
	home := t.TempDir()
	profilePath := filepath.Join(home, "Desktop", "mdm_enroll.mobileconfig")
	if desktopProfile != nil {
		if err := os.MkdirAll(filepath.Dir(profilePath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(profilePath, desktopProfile, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	m := newAgentOnlyManager(t, fakeEnrollGuest(home))
	m.cfg.SSHUser, m.cfg.SSHPassword = "", ""
	m.cfg.JamfProfiles = []JamfProfile{{
		ID: "school-1", Kind: JamfKindSchool, Name: "School Tenant",
		BaseURL: "https://school.example", Network: testSchoolNetwork, Location: testSchoolLocation,
	}}
	m.vms["vm"].EnrollIdentitySHA256 = recorded
	fetcher := &recordingSchoolFetcher{profile: testSchoolProfileBytes()}
	m.schoolFetcher = fetcher
	return m, fetcher, profilePath
}

func runEnroll(t *testing.T, m *Manager) *Task {
	t.Helper()
	m.runAutoEnrollTask("vm", "test", "") // "None": use whatever is on the Desktop
	if len(m.tasks) != 1 {
		t.Fatalf("tasks = %+v", m.tasks)
	}
	return m.tasks[0]
}

const foreignSchoolProfile = "\x30\x82 signed copy of another VM's profile com.apple.security.pkcs12 foreign-secret"

// The reason for the whole check: a base VM holding a School profile is cloned,
// and every clone enrolled with "None" would present the same device identity.
// The file is not replaced behind the user's back (it may be a profile placed on
// purpose, and the first configured server may be a different MDM); the task
// stops and says what to do.
func TestAutoEnrollRefusesADeviceIdentityItDidNotIssueToThisVM(t *testing.T) {
	m, fetcher, profilePath := newEnrollIdentityManager(t, []byte(foreignSchoolProfile), "")

	task := runEnroll(t, m)

	if task.Status != "error" {
		t.Fatalf("task = %s, want it stopped\n%s", task.Status, task.Output)
	}
	if !strings.Contains(task.Error, "Choose a Jamf server profile") {
		t.Fatalf("the error should say what to do: %q", task.Error)
	}
	if fetcher.called != 0 {
		t.Fatalf("a profile was fetched (%d) although the user chose none: it would mint an identity nobody asked for", fetcher.called)
	}
	if got, _ := os.ReadFile(profilePath); string(got) != foreignSchoolProfile {
		t.Fatalf("the Desktop profile was replaced: %q", got)
	}
}

// Whether grep can read the PKCS#12 payload type inside a signed profile is
// unconfirmed, so a signed (binary) file without the marker counts as carrying an
// identity too.
func TestAutoEnrollTreatsAnUnreadableSignedProfileAsCarryingAnIdentity(t *testing.T) {
	signedWithoutMarker := []byte("\x30\x82\x25\xd4\x06\x09 an envelope grep cannot see into")
	m, fetcher, _ := newEnrollIdentityManager(t, signedWithoutMarker, "")

	task := runEnroll(t, m)

	if task.Status != "error" || fetcher.called != 0 {
		t.Fatalf("task = %s, fetches = %d, want a refusal\n%s", task.Status, fetcher.called, task.Output)
	}
}

// The probe is shell that runs in the guest, so run it for real against files.
func TestDesktopProfileProbeClassifiesFilesFailingClosed(t *testing.T) {
	probe := func(t *testing.T, content []byte, mode os.FileMode) string {
		t.Helper()
		home := t.TempDir()
		if content != nil {
			path := filepath.Join(home, "Desktop", "mdm_enroll.mobileconfig")
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, content, mode); err != nil {
				t.Fatal(err)
			}
		}
		cmd := exec.Command("/bin/sh", "-c", desktopProfileProbe)
		cmd.Env = append(os.Environ(), "HOME="+home)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		return strings.TrimSpace(string(out))
	}
	plain := []byte(`<?xml version="1.0"?><plist><dict><key>PayloadType</key><string>Profile Service</string></dict></plist>`)

	if got := probe(t, nil, 0); got != "no" {
		t.Errorf("no file: %q", got)
	}
	if got := probe(t, plain, 0o600); got != "yes" {
		t.Errorf("plain XML plist: %q", got)
	}
	withIdentity := []byte(`<?xml version="1.0"?><plist><dict><key>PayloadType</key><string>com.apple.security.pkcs12</string></dict></plist>`)
	if got := probe(t, withIdentity, 0o600); !strings.HasPrefix(got, "identity ") || len(got) != len("identity ")+64 {
		t.Errorf("XML plist with an identity payload: %q", got)
	}
	if got := probe(t, []byte("\x30\x82 signed, marker not visible"), 0o600); !strings.HasPrefix(got, "identity ") {
		t.Errorf("binary file: %q (must fail closed)", got)
	}
	if os.Getuid() != 0 {
		if got := probe(t, plain, 0o000); !strings.HasPrefix(got, "identity") {
			t.Errorf("unreadable file: %q (must fail closed)", got)
		}
	}
}

func TestAutoEnrollReusesTheDeviceIdentityItIssuedToThisVM(t *testing.T) {
	own := []byte(foreignSchoolProfile)
	m, fetcher, profilePath := newEnrollIdentityManager(t, own, identitySHA256(own))

	task := runEnroll(t, m)

	if task.Status != "success" {
		t.Fatalf("task = %s %q\n%s", task.Status, task.Error, task.Output)
	}
	if fetcher.called != 0 {
		t.Fatalf("a second identity was minted (%d fetches) for a VM that already holds its own", fetcher.called)
	}
	if got, _ := os.ReadFile(profilePath); string(got) != string(own) {
		t.Fatal("the VM's own profile was replaced")
	}
}

func TestAutoEnrollStillReusesAProfileWithoutAnIdentity(t *testing.T) {
	proLike := []byte(`<?xml version="1.0"?><plist><dict><key>PayloadType</key><string>Profile Service</string></dict></plist>`)
	m, fetcher, profilePath := newEnrollIdentityManager(t, proLike, "")

	task := runEnroll(t, m)

	if task.Status != "success" || fetcher.called != 0 {
		t.Fatalf("task = %s, fetches = %d, want a reuse\n%s", task.Status, fetcher.called, task.Output)
	}
	if got, _ := os.ReadFile(profilePath); string(got) != string(proLike) {
		t.Fatal("a hand-placed profile was overwritten")
	}
	if !strings.Contains(task.Output, "Reusing") {
		t.Fatalf("output:\n%s", task.Output)
	}
}

func TestAutoEnrollPushesAProfileWhenTheDesktopHasNone(t *testing.T) {
	m, fetcher, profilePath := newEnrollIdentityManager(t, nil, "")

	task := runEnroll(t, m)

	if task.Status != "success" || fetcher.called != 1 {
		t.Fatalf("task = %s, fetches = %d\n%s", task.Status, fetcher.called, task.Output)
	}
	if _, err := os.Stat(profilePath); err != nil {
		t.Fatal("no profile was pushed")
	}
}

// A Pro profile that replaces a School one means the identity is gone from the
// Desktop; the record must not keep vouching for it.
func TestPushingAProProfileForgetsTheRecordedSchoolIdentity(t *testing.T) {
	m, _, _ := newEnrollIdentityManager(t, nil, "stale")
	m.cfg.JamfProfiles = []JamfProfile{{ID: "pro-1", Kind: JamfKindPro, Name: "Pro", BaseURL: "https://pro.example", InvitationCode: "invite-1"}}

	if _, _, err := m.copyMDMProfileToVM(t.Context(), "vm", "pro-1", "", ""); err != nil {
		t.Fatal(err)
	}
	if got := m.vms["vm"].EnrollIdentitySHA256; got != "" {
		t.Fatalf("recorded identity = %q after a Jamf Pro profile replaced it", got)
	}
}

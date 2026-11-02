package server

import "testing"

func TestParseEnrollmentStatus(t *testing.T) {
	enrolled := "Enrolled via DEP: No\nMDM enrollment: Yes (User Approved)\nMDM server: https://emeia.jamfce.com/mdm/ServerURL\n"
	got, ok := parseEnrollmentStatus(enrolled)
	if !ok || !got.Enrolled {
		t.Fatalf("enrolled parse = %+v ok=%v", got, ok)
	}
	if got.Server != "https://emeia.jamfce.com/mdm/ServerURL" {
		t.Fatalf("server = %q", got.Server)
	}
	if got.Detail != "Yes (User Approved)" {
		t.Fatalf("detail = %q", got.Detail)
	}

	plain, ok := parseEnrollmentStatus("Enrolled via DEP: No\nMDM enrollment: Yes\n")
	if !ok || !plain.Enrolled || plain.Server != "" {
		t.Fatalf("enrolled-without-server parse = %+v ok=%v", plain, ok)
	}

	no, ok := parseEnrollmentStatus("Enrolled via DEP: No\nMDM enrollment: No\n")
	if !ok || no.Enrolled {
		t.Fatalf("unenrolled parse = %+v ok=%v", no, ok)
	}

	// Unrecognisable output must report not-ok so the UI can show "unknown" rather
	// than claiming the VM is unenrolled.
	for _, bad := range []string{"", "   ", "command not found", "zsh: permission denied"} {
		if _, ok := parseEnrollmentStatus(bad); ok {
			t.Fatalf("%q should not parse as a known status", bad)
		}
	}
}

func TestJamfConsoleURL(t *testing.T) {
	for raw, want := range map[string]string{
		"https://emeia.jamfce.com/mdm/ServerURL":  "https://emeia.jamfce.com",
		"https://emeia.jamfce.com/mdm/ServerURL/": "https://emeia.jamfce.com",
		"https://tenant.jamfcloud.com":            "https://tenant.jamfcloud.com",
		"":                                        "",
	} {
		if got := jamfConsoleURL(raw); got != want {
			t.Fatalf("jamfConsoleURL(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestRefreshMDMStatusLeavesTheVMUnknownWhenTheProbeFails(t *testing.T) {
	m := newTestManager(t)
	m.cfg.TartAppPath = "/nonexistent/tart"
	m.cfg.SSHFallbackEnabled = false
	m.vms["vm1"] = &VM{Name: "vm1", State: "running", IP: "10.0.0.9"}

	m.refreshMDMStatus("vm1")

	vm := m.vms["vm1"]
	if vm.MDMEnrolled {
		t.Fatal("a failed probe must not report the VM as enrolled")
	}
	if !vm.MDMCheckedAt.IsZero() {
		t.Fatal("a failed probe must leave MDMCheckedAt zero so the UI shows unknown, not red")
	}
}

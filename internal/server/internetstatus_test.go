package server

import "testing"

func TestParseInternetStatus(t *testing.T) {
	online, ok := parseInternetStatus("<HTML><HEAD><TITLE>Success</TITLE></HEAD><BODY>Success</BODY></HTML>\nCURL_EXIT=0\n")
	if !ok || !online.Online {
		t.Fatalf("online parse = %+v ok=%v", online, ok)
	}

	offline, ok := parseInternetStatus("CURL_EXIT=6\n")
	if !ok || offline.Online || offline.Detail == "" {
		t.Fatalf("offline parse = %+v ok=%v", offline, ok)
	}

	// A captive portal answers curl successfully but not with Apple's page.
	portal, ok := parseInternetStatus("<html>Please sign in to the guest Wi-Fi</html>\nCURL_EXIT=0\n")
	if !ok || portal.Online || portal.Detail == "" {
		t.Fatalf("captive portal parse = %+v ok=%v", portal, ok)
	}

	// Unrecognisable output must report not-ok so the UI can show "unknown" rather
	// than claiming the VM is offline.
	for _, bad := range []string{"", "   ", "zsh: command not found: curl", "Success"} {
		if _, ok := parseInternetStatus(bad); ok {
			t.Fatalf("%q should not parse as a known status", bad)
		}
	}
}

func TestRefreshInternetStatusLeavesTheVMUnknownWhenTheProbeFails(t *testing.T) {
	m := newTestManager(t)
	m.cfg.TartAppPath = "/nonexistent/tart"
	m.cfg.SSHFallbackEnabled = false
	m.vms["vm1"] = &VM{Name: "vm1", State: "running", IP: "10.0.0.9"}

	m.refreshInternetStatus("vm1")

	vm := m.vms["vm1"]
	if vm.InternetOK {
		t.Fatal("a failed probe must not report the VM as online")
	}
	if !vm.InternetCheckedAt.IsZero() {
		t.Fatal("a failed probe must leave InternetCheckedAt zero so the UI shows unknown, not red")
	}
}

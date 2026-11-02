package server

import (
	"strings"
	"testing"
)

func TestDNSHostname(t *testing.T) {
	for in, want := range map[string]string{
		"My_VM-1":            "My-VM-1",
		"Vincent's Mac mini": "Vincent-s-Mac-mini",
		"  --demo--  ":       "demo",
		"Café":               "Caf",
		"!!!":                "",
	} {
		if got := dnsHostname(in); got != want {
			t.Errorf("dnsHostname(%q) = %q, want %q", in, got, want)
		}
	}
	if got := dnsHostname(strings.Repeat("a", 80)); len(got) != maxHostnameLen {
		t.Errorf("long name not truncated: %d", len(got))
	}
}

func TestValidateHostname(t *testing.T) {
	for _, ok := range []string{"", "Demo Mac", "My_VM-1"} {
		if err := validateHostname(ok); err != nil {
			t.Errorf("validateHostname(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"!!!", "a\nb", strings.Repeat("a", 64)} {
		if validateHostname(bad) == nil {
			t.Errorf("validateHostname(%q) accepted", bad)
		}
	}
}

func TestHostnameCommandQuotesAndUsesOneSudo(t *testing.T) {
	cmd := hostnameCommand("Bob's Mac")
	if strings.Count(cmd, "sudo") != 1 || !strings.HasPrefix(cmd, "sudo /bin/sh -c ") {
		t.Fatalf("command = %s", cmd)
	}
	for _, want := range []string{`ComputerName '\''Bob'\''\'\'''\''s Mac'\''`, `LocalHostName '\''Bob-s-Mac'\''`, `HostName '\''Bob-s-Mac'\''`} {
		if !strings.Contains(cmd, want) {
			t.Errorf("command missing %s:\n%s", want, cmd)
		}
	}
}

func TestCloneHostname(t *testing.T) {
	if got := cloneHostname("demo", "AB12CD34", 1); got != "demo" {
		t.Errorf("single clone = %q", got)
	}
	if got := cloneHostname("demo", "AB12CD34", 3); got != "demo-AB12CD34" {
		t.Errorf("batch clone = %q", got)
	}
	if got := cloneHostname(strings.Repeat("é", 40), "AB12CD34", 2); len(got) > maxHostnameLen || !strings.HasSuffix(got, "-AB12CD34") {
		t.Errorf("long batch clone = %q (%d)", got, len(got))
	}
}

func TestDesiredHostname(t *testing.T) {
	if got := desiredHostname(&VM{Name: "vm1", Hostname: "x", HostnameFromName: true}); got != "vm1" {
		t.Errorf("from name = %q", got)
	}
	if got := desiredHostname(&VM{Name: "vm1", Hostname: "x"}); got != "x" {
		t.Errorf("custom = %q", got)
	}
}

func TestApplyHostnameSkipsWhenAlreadyApplied(t *testing.T) {
	m := newTestManager(t)
	m.vms["vm1"] = &VM{Name: "vm1", State: "running", HostnameFromName: true, HostnameApplied: "vm1"}
	m.applyHostnameIfPending("vm1") // would try tart exec if it didn't skip
	if m.hostnameBusy["vm1"] {
		t.Fatal("left busy")
	}
}

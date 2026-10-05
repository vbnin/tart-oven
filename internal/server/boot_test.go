package server

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/net/route"
)

func TestPollVMIPRetriesTransientResolverErrors(t *testing.T) {
	attempts := 0
	probe := func(context.Context) (string, error) {
		attempts++
		if attempts < 3 {
			return "", errors.New("arp command yielded invalid output: empty output")
		}
		return "192.168.1.44\n", nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	ip, err := pollVMIP(ctx, time.Millisecond, probe)
	if err != nil {
		t.Fatalf("pollVMIP returned error: %v", err)
	}
	if ip != "192.168.1.44" {
		t.Fatalf("pollVMIP returned %q, want %q", ip, "192.168.1.44")
	}
	if attempts != 3 {
		t.Fatalf("probe attempts = %d, want 3", attempts)
	}
}

func TestPollVMIPDoesNotProbeCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	attempts := 0

	_, err := pollVMIP(ctx, time.Millisecond, func(context.Context) (string, error) {
		attempts++
		return "192.168.1.44", nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pollVMIP error = %v, want context.Canceled", err)
	}
	if attempts != 0 {
		t.Fatalf("probe attempts = %d, want 0", attempts)
	}
}

func TestPollVMIPRejectsResultAfterDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, err := pollVMIP(ctx, time.Millisecond, func(context.Context) (string, error) {
		cancel()
		return "192.168.1.44", nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pollVMIP error = %v, want context.Canceled", err)
	}
}

func TestResolveVMIPMatchesConfigMACAgainstNativeNeighbors(t *testing.T) {
	home := t.TempDir()
	vmDir := filepath.Join(home, "vms", "base-vm")
	if err := os.MkdirAll(vmDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vmDir, "config.json"), []byte(`{"macAddress":"ea:b7:75:5f:8f:ff"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	neighbors := func() ([]arpNeighbor, error) {
		return []arpNeighbor{
			{IP: net.ParseIP("192.168.1.8"), MAC: mustParseMAC(t, "00:11:22:33:44:55")},
			{IP: net.ParseIP("192.168.1.11"), MAC: mustParseMAC(t, "ea:b7:75:5f:8f:ff")},
		}, nil
	}

	ip, err := resolveVMIP(home, "base-vm", neighbors)
	if err != nil {
		t.Fatalf("resolveVMIP returned error: %v", err)
	}
	if ip != "192.168.1.11" {
		t.Fatalf("resolveVMIP returned %q, want %q", ip, "192.168.1.11")
	}
}

func TestResolveVMIPRejectsParentDirectoryName(t *testing.T) {
	called := false
	_, err := resolveVMIP(t.TempDir(), "..", func() ([]arpNeighbor, error) {
		called = true
		return nil, nil
	})
	if err == nil {
		t.Fatal("resolveVMIP accepted parent-directory VM name")
	}
	if called {
		t.Fatal("neighbor lookup ran for invalid VM name")
	}
}

func TestARPNeighborsFromRouteMessages(t *testing.T) {
	valid := make([]route.Addr, syscall.RTAX_GATEWAY+1)
	valid[syscall.RTAX_DST] = &route.Inet4Addr{IP: [4]byte{192, 168, 1, 11}}
	valid[syscall.RTAX_GATEWAY] = &route.LinkAddr{Addr: []byte{0xea, 0xb7, 0x75, 0x5f, 0x8f, 0xff}}
	incomplete := make([]route.Addr, syscall.RTAX_GATEWAY+1)
	incomplete[syscall.RTAX_DST] = &route.Inet4Addr{IP: [4]byte{192, 168, 1, 12}}
	nonEthernet := make([]route.Addr, syscall.RTAX_GATEWAY+1)
	nonEthernet[syscall.RTAX_DST] = &route.Inet4Addr{IP: [4]byte{192, 168, 1, 13}}
	nonEthernet[syscall.RTAX_GATEWAY] = &route.LinkAddr{Addr: []byte{1, 2, 3}}

	got := arpNeighborsFromRouteMessages([]route.Message{
		&route.RouteMessage{Addrs: incomplete},
		&route.RouteMessage{Addrs: nonEthernet},
		&route.RouteMessage{Addrs: valid},
	})
	if len(got) != 1 {
		t.Fatalf("parsed %d neighbors, want 1", len(got))
	}
	if got[0].IP.String() != "192.168.1.11" || got[0].MAC.String() != "ea:b7:75:5f:8f:ff" {
		t.Fatalf("parsed neighbor = %s/%s", got[0].IP, got[0].MAC)
	}
}

func TestParseFlexMAC(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"4e:4c:40:94:26:36", "4e:4c:40:94:26:36"},
		{"74:ac:b9:46:6b:4", "74:ac:b9:46:6b:04"},
		{"ba:6d:ed:c8:68:a", "ba:6d:ed:c8:68:0a"},
		{"0:e0:4c:0:2:15", "00:e0:4c:00:02:15"},
	}
	for _, tt := range tests {
		got, err := parseFlexMAC(tt.input)
		if err != nil {
			t.Fatalf("parseFlexMAC(%q) error: %v", tt.input, err)
		}
		if got.String() != tt.want {
			t.Errorf("parseFlexMAC(%q) = %q, want %q", tt.input, got.String(), tt.want)
		}
	}
}

func TestParseARPOutput(t *testing.T) {
	sample := `? (192.168.1.1) at 74:ac:b9:46:6b:4 on en8 ifscope [ethernet]
? (192.168.1.204) at 4e:4c:40:94:26:36 on en8 [ethernet]
? (192.168.1.103) at 0:e0:4c:0:2:15 on en8 ifscope permanent [ethernet]`

	entries := parseARPOutput(sample)
	if len(entries) != 3 {
		t.Fatalf("parseARPOutput parsed %d entries, want 3", len(entries))
	}
	if entries[1].IP.String() != "192.168.1.204" || entries[1].MAC.String() != "4e:4c:40:94:26:36" {
		t.Errorf("entry 1 = %s/%s, want 192.168.1.204/4e:4c:40:94:26:36", entries[1].IP, entries[1].MAC)
	}
	if entries[0].IP.String() != "192.168.1.1" || entries[0].MAC.String() != "74:ac:b9:46:6b:04" {
		t.Errorf("entry 0 = %s/%s, want 192.168.1.1/74:ac:b9:46:6b:04", entries[0].IP, entries[0].MAC)
	}
}

func TestHostARPNeighbors(t *testing.T) {
	neighbors, err := hostARPNeighbors()
	if err != nil {
		t.Fatalf("hostARPNeighbors returned error: %v", err)
	}
	t.Logf("hostARPNeighbors found %d active ARP neighbors", len(neighbors))
}

func mustParseMAC(t *testing.T, value string) net.HardwareAddr {
	t.Helper()
	mac, err := net.ParseMAC(value)
	if err != nil {
		t.Fatal(err)
	}
	return mac
}

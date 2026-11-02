package server

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestTartVMJSONIncludesSourceAndImageMetadata(t *testing.T) {
	var got []tartVM
	err := json.Unmarshal([]byte(`[{"Source":"OCI","Name":"ghcr.io/example/base@sha256:abc","Disk":80,"Size":22,"Accessed":"2026-08-23T12:00:00Z","State":"stopped","Running":false}]`), &got)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1", len(got))
	}
	vm := got[0]
	if vm.Source != "OCI" || vm.Name != "ghcr.io/example/base@sha256:abc" || vm.Disk != 80 || vm.Size != 22 || vm.Accessed != "2026-08-23T12:00:00Z" {
		t.Fatalf("metadata was not preserved: %#v", vm)
	}
}

func TestParseTartTableIncludesSourceAndImageMetadata(t *testing.T) {
	m := newTestManager(t)
	m.tartOutputOperation = func(_ context.Context, _ string, _ ...string) (string, error) {
		return strings.Join([]string{
			"Source Name Disk Size Accessed State",
			"local base-vm 80 22 3 hours ago stopped",
			"OCI ghcr.io/example/base:latest 80 22 1 day ago stopped",
		}, "\n"), nil
	}

	got, err := m.parseTartTable(m.cfg.VMStoragePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2: %#v", len(got), got)
	}
	if got[1].Source != "OCI" || got[1].Name != "ghcr.io/example/base:latest" || got[1].Disk != 80 || got[1].Size != 22 || got[1].Accessed != "1 day ago" {
		t.Fatalf("OCI table metadata was not preserved: %#v", got[1])
	}
}

func TestReconcilePreservesTartSourceAndImageMetadata(t *testing.T) {
	m := newTestManager(t)
	m.tartJSON = true
	m.tartOutputOperation = func(_ context.Context, _ string, _ ...string) (string, error) {
		return `[{"Source":"OCI","Name":"ghcr.io/example/base:latest","Disk":80,"Size":22,"Accessed":"2026-08-23T12:00:00Z","State":"stopped","Running":false}]`, nil
	}

	m.reconcile()
	got := m.vms["ghcr.io/example/base:latest"]
	if got == nil {
		t.Fatal("reconcile did not add OCI image")
	}
	if got.Source != "OCI" || got.Disk != 80 || got.Size != 22 || got.Accessed != "2026-08-23T12:00:00Z" {
		t.Fatalf("reconcile dropped Tart metadata: %#v", got)
	}
}

func TestLoadDefaultsOCIExclusionOnForUpgradesButPreservesExplicitFalse(t *testing.T) {
	for _, tt := range []struct {
		name string
		body string
		want bool
	}{
		{"old state without setting", `{"config":{"listen":"127.0.0.1:9000"},"vms":{},"history":[]}`, true},
		{"new state explicitly disabled", `{"config":{"listen":"127.0.0.1:9000","excludeOciFromScheduler":false},"vms":{},"history":[]}`, false},
		{"new state explicitly enabled", `{"config":{"listen":"127.0.0.1:9000","excludeOciFromScheduler":true},"vms":{},"history":[]}`, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestManager(t)
			if err := os.WriteFile(m.statePath, []byte(tt.body), 0o600); err != nil {
				t.Fatal(err)
			}
			m.load()
			if m.cfg.ExcludeOCIFromScheduler != tt.want {
				t.Fatalf("ExcludeOCIFromScheduler = %v, want %v", m.cfg.ExcludeOCIFromScheduler, tt.want)
			}
		})
	}
}

func TestSchedulerEligibilityHonoursOCIExclusion(t *testing.T) {
	local := &VM{Name: "local-base", Source: "local", State: "stopped"}
	oci := &VM{Name: "ghcr.io/example/base:latest", Source: "OCI", State: "stopped"}

	if !eligibleForScheduler(local, true, nil, false) {
		t.Fatal("local stopped VM should remain eligible")
	}
	if eligibleForScheduler(oci, true, nil, false) {
		t.Fatal("OCI image should be excluded when the setting is enabled")
	}
	if !eligibleForScheduler(oci, false, nil, false) {
		t.Fatal("OCI image should be eligible when the setting is explicitly disabled")
	}
	if !isOCI(" oCi ") {
		t.Fatal("OCI source matching should be case-insensitive and whitespace-tolerant")
	}
	for _, source := range []string{"", "   ", "registry-cache", "local"} {
		legacy := &VM{Name: "legacy", Source: source, State: "stopped"}
		if !eligibleForScheduler(legacy, true, nil, false) {
			t.Errorf("source %q should remain local-compatible", source)
		}
	}
}

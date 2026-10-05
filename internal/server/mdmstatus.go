package server

import (
	"context"
	"strings"
	"time"
)

// mdmEnrollmentProbe is fixed rather than configurable: its output shape is what the
// parser depends on, and StatusCommand is a free-text box the operator can edit.
const mdmEnrollmentProbe = "/usr/bin/profiles status -type enrollment"

type mdmStatus struct {
	Enrolled bool
	Server   string // raw MDM check-in URL as reported by the guest
	Detail   string // e.g. "Yes (User Approved)", kept for the tooltip
}

// parseEnrollmentStatus reads `profiles status -type enrollment` output. The second
// return value reports whether the output was recognisable; unrecognised output must
// be surfaced as unknown, never as "not enrolled" — a red light on a healthy VM is
// worse than a grey one.
func parseEnrollmentStatus(output string) (mdmStatus, bool) {
	var status mdmStatus
	recognised := false
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if value, found := strings.CutPrefix(line, "MDM enrollment:"); found {
			status.Detail = strings.TrimSpace(value)
			status.Enrolled = strings.HasPrefix(status.Detail, "Yes")
			recognised = true
			continue
		}
		if value, found := strings.CutPrefix(line, "MDM server:"); found {
			status.Server = strings.TrimSpace(value)
		}
	}
	return status, recognised
}

// jamfConsoleURL turns the MDM check-in endpoint into the console URL an operator
// would actually open. A value that does not carry the suffix is returned unchanged.
func jamfConsoleURL(raw string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(raw), "/")
	if base, found := strings.CutSuffix(trimmed, "/mdm/ServerURL"); found {
		return base
	}
	return trimmed
}

// refreshMDMStatus probes a running guest for its MDM enrollment and records the
// result. A failed or unrecognised probe deliberately leaves MDMCheckedAt zero, so the
// dashboard shows "unknown" instead of asserting the VM is unenrolled.
func (m *Manager) refreshMDMStatus(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	res := m.execInGuest(ctx, name, mdmEnrollmentProbe, "")
	if res.Error != "" || res.ExitCode != 0 {
		return
	}
	status, recognised := parseEnrollmentStatus(res.Stdout)
	if !recognised {
		return
	}

	m.mu.Lock()
	if vm := m.vms[name]; vm != nil {
		vm.MDMEnrolled = status.Enrolled
		vm.MDMServer = status.Server
		vm.MDMCheckedAt = time.Now()
	}
	m.save()
	m.mu.Unlock()
}

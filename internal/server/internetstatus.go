package server

import (
	"context"
	"strings"
	"time"
)

// internetProbe fetches Apple's captive-portal check page, the same URL macOS itself
// uses to decide whether it is online. The page says "Success" only when the request
// really reached Apple, so a captive portal or filtering proxy that answers in its
// place is not mistaken for working internet access. The trailing echo keeps the
// curl exit status in the output, so a guest with no internet is told apart from a
// guest that could not run the probe at all.
const internetProbe = "/usr/bin/curl -s -m 8 http://captive.apple.com/hotspot-detect.html; echo \"CURL_EXIT=$?\""

type internetStatus struct {
	Online bool
	Detail string // why the guest is offline, kept for the tooltip
}

// parseInternetStatus reads internetProbe output. The second return value reports
// whether the output was recognisable; unrecognised output must be surfaced as
// unknown, never as "no internet" — a red light on a healthy VM is worse than a grey
// one.
func parseInternetStatus(output string) (internetStatus, bool) {
	exit := ""
	body := make([]string, 0, 2)
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if value, found := strings.CutPrefix(line, "CURL_EXIT="); found {
			exit = strings.TrimSpace(value)
			continue
		}
		body = append(body, line)
	}
	switch {
	case exit == "":
		return internetStatus{}, false
	case exit != "0":
		return internetStatus{Detail: "no connection (curl exit " + exit + ")"}, true
	case strings.Contains(strings.Join(body, "\n"), "Success"):
		return internetStatus{Online: true}, true
	default:
		return internetStatus{Detail: "reply did not come from Apple (captive portal or proxy?)"}, true
	}
}

// refreshInternetStatus probes a running guest for internet access and records the
// result. A failed or unrecognised probe deliberately leaves InternetCheckedAt zero,
// so the dashboard shows "unknown" instead of asserting the VM is offline.
func (m *Manager) refreshInternetStatus(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res := m.execInGuest(ctx, name, internetProbe, "")
	if res.Error != "" || res.ExitCode != 0 {
		return
	}
	status, recognised := parseInternetStatus(res.Stdout)
	if !recognised {
		return
	}

	m.mu.Lock()
	if vm := m.vms[name]; vm != nil {
		vm.InternetOK = status.Online
		vm.InternetDetail = status.Detail
		vm.InternetCheckedAt = time.Now()
	}
	m.save()
	m.mu.Unlock()
}

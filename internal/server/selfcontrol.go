package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// ---------------------------------------------------------------------------
// Server self-control (restart / stop the tart-oven process itself)
// ---------------------------------------------------------------------------

// restartServer re-execs this binary in place (works whether launched manually
// or by launchd; same PID, so KeepAlive is unaffected). Falls back to exiting
// (KeepAlive then respawns) if re-exec isn't possible.
func (m *Manager) restartServer() {
	m.logln("server restart requested")
	time.Sleep(300 * time.Millisecond) // let the HTTP response flush
	if exe, err := os.Executable(); err == nil {
		if err := syscall.Exec(exe, os.Args, os.Environ()); err != nil {
			m.logln("re-exec failed: %v; exiting instead", err)
		}
	}
	os.Exit(0)
}

// stopServer stops the process for good. If we're a launchd agent, bootout so
// KeepAlive doesn't respawn us; otherwise a plain exit is enough.
func (m *Manager) stopServer() {
	m.logln("server stop requested")
	time.Sleep(300 * time.Millisecond) // let the HTTP response flush
	uid := os.Getuid()
	for _, label := range []string{"com.tartoven.agent", "com.user.tart-oven"} {
		exec.Command("launchctl", "bootout", fmt.Sprintf("gui/%d/%s", uid, label)).Run()
	}
	os.Exit(0)
}

const launchAgentLabel = "com.tartoven.agent"
const launchAgentPlist = "/Library/LaunchAgents/com.tartoven.agent.plist"

func (m *Manager) handleLaunchAgent(w http.ResponseWriter, r *http.Request) {
	uid := os.Getuid()
	domain := fmt.Sprintf("gui/%d", uid)

	if r.Method == http.MethodGet {
		installed := false
		if _, err := os.Stat(launchAgentPlist); err == nil {
			installed = true
		}
		enabled := m.launchAgentEnabled(domain)
		writeJSON(w, map[string]any{"installed": installed, "enabled": enabled})
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	action := "disable"
	if req.Enabled {
		action = "enable"
	}
	target := fmt.Sprintf("%s/%s", domain, launchAgentLabel)
	out, err := exec.Command("launchctl", action, target).CombinedOutput()
	if err != nil {
		m.logln("launchctl %s %s failed: %v: %s", action, target, err, string(out))
		http.Error(w, fmt.Sprintf("launchctl %s failed: %v", action, err), http.StatusInternalServerError)
		return
	}
	m.logln("launchagent %s: %s", action, target)
	writeJSON(w, map[string]bool{"ok": true})
}

func (m *Manager) launchAgentEnabled(domain string) bool {
	out, err := exec.Command("launchctl", "print-disabled", domain).CombinedOutput()
	if err != nil {
		return true // assume enabled if we can't check
	}
	// Output contains lines like: "com.tartoven.agent" => disabled
	// If not listed or listed without "disabled", it's enabled.
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, launchAgentLabel) && strings.Contains(line, "disabled") {
			return false
		}
	}
	return true
}

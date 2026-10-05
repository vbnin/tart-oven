package server

import (
	"encoding/json"
	"log"
	"os"
	"time"
)

// ---------------------------------------------------------------------------
// Persistence
// ---------------------------------------------------------------------------

func (m *Manager) load() {
	m.cfg = defaultConfig()
	data, err := os.ReadFile(m.statePath)
	if err != nil {
		return // first run
	}
	var p persisted
	if err := json.Unmarshal(data, &p); err != nil {
		log.Printf("state.json is corrupt, starting fresh: %v", err)
		return
	}
	// Overlay persisted config onto defaults, then repair any missing fields so
	// an old/partial file can't leave us with zero intervals etc.
	m.cfg = p.Config
	d := defaultConfig()
	// A plain bool cannot distinguish an older state file with no field from an
	// explicit false. Detect field presence so upgrades receive the safe default
	// while users can still opt OCI images back into scheduling.
	var presence struct {
		Config struct {
			ExcludeOCIFromScheduler *bool `json:"excludeOciFromScheduler"`
			SSHFallbackEnabled      *bool `json:"sshFallbackEnabled"`
		} `json:"config"`
	}
	if err := json.Unmarshal(data, &presence); err == nil {
		if presence.Config.ExcludeOCIFromScheduler == nil {
			m.cfg.ExcludeOCIFromScheduler = d.ExcludeOCIFromScheduler
		}
		if presence.Config.SSHFallbackEnabled == nil {
			m.cfg.SSHFallbackEnabled = d.SSHFallbackEnabled
		}
	}
	if m.cfg.Listen == "" {
		m.cfg.Listen = d.Listen
	}
	if m.cfg.VMStoragePath == "" {
		m.cfg.VMStoragePath = d.VMStoragePath
	}
	if m.cfg.SharedDir == "" {
		m.cfg.SharedDir = d.SharedDir
	}
	// Migrate the earlier directory-style value to a full binary path.
	if m.cfg.TartAppPath == "" || m.cfg.TartAppPath == "/Applications/" {
		m.cfg.TartAppPath = d.TartAppPath
	}
	if m.cfg.IntervalMinutes < 1 {
		m.cfg.IntervalMinutes = d.IntervalMinutes
	}
	if m.cfg.WindowMinutes < 1 {
		m.cfg.WindowMinutes = d.WindowMinutes
	}
	if m.cfg.MaxConcurrent < 1 {
		m.cfg.MaxConcurrent = d.MaxConcurrent
	}
	if m.cfg.MaxConcurrent > hardMaxConcurrent {
		m.cfg.MaxConcurrent = hardMaxConcurrent
	}
	if m.cfg.SchedulerMode != "sequential" {
		m.cfg.SchedulerMode = "random"
	}
	if m.cfg.SSHUser == "" {
		m.cfg.SSHUser = d.SSHUser
	}
	if m.cfg.SSHPassword == "" {
		m.cfg.SSHPassword = d.SSHPassword
	}
	if m.cfg.SSHTimeoutSec < 1 {
		m.cfg.SSHTimeoutSec = d.SSHTimeoutSec
	}
	if m.cfg.StatusCommand == "" {
		m.cfg.StatusCommand = d.StatusCommand
	}
	if m.cfg.StatusCommand == legacyJamfUserStatusCommand {
		m.cfg.StatusCommand = d.StatusCommand
	}
	if !validSSHKeyPath(m.cfg.SSHKey) {
		m.cfg.SSHKey = d.SSHKey
	}
	if m.cfg.Excluded == nil {
		m.cfg.Excluded = []string{}
	}
	if m.cfg.HistoryDays < 1 {
		m.cfg.HistoryDays = d.HistoryDays
	}
	if m.cfg.BootTimeoutSec < 10 {
		m.cfg.BootTimeoutSec = d.BootTimeoutSec
	}
	if m.cfg.NetPriority != "wifi" && m.cfg.NetPriority != "ethernet" && m.cfg.NetPriority != "shared" {
		m.cfg.NetPriority = "auto"
	}
	// Older state files predate the daily window; seed it (enabled) on upgrade.
	if m.cfg.DailyStart == "" || m.cfg.DailyStop == "" {
		m.cfg.DailyEnabled = true
		m.cfg.DailyStart = d.DailyStart
		m.cfg.DailyStop = d.DailyStop
	}
	// Older state files predate file logging; seed with default path.
	if m.cfg.LogPath == "" {
		m.cfg.LogPath = d.LogPath
	}
	if p.VMs != nil {
		m.vms = p.VMs
	}
	if p.History != nil {
		m.history = p.History
	}
	m.pruneHistory()
	// Seed the sequential cursor from the most recently run VM so sequential
	// scheduling continues from where it left off across restarts.
	var latest time.Time
	for name, vm := range m.vms {
		if vm.LastRun.After(latest) {
			latest = vm.LastRun
			m.lastSequential = name
		}
	}
}

// pruneHistory drops events older than the retention window. Caller MUST hold mu.
func (m *Manager) pruneHistory() {
	cutoff := time.Now().AddDate(0, 0, -m.cfg.HistoryDays)
	kept := m.history[:0]
	for _, ev := range m.history {
		if ev.StartedAt.After(cutoff) {
			kept = append(kept, ev)
		}
	}
	m.history = kept
}

// save writes state atomically. Caller MUST hold m.mu.
func (m *Manager) save() {
	p := persisted{Config: m.cfg, VMs: m.vms, History: m.history}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		log.Printf("marshal state: %v", err)
		return
	}
	tmp := m.statePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		log.Printf("write state: %v", err)
		return
	}
	if err := os.Rename(tmp, m.statePath); err != nil {
		log.Printf("rename state: %v", err)
	}
}

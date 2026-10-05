package server

import (
	"fmt"
	"log"
	"strings"
	"time"

	tartoven "tart-oven"
	"tart-oven/internal/update"
)

// updateCheckLoop checks for new Tart and Tart Oven releases at startup and
// then every 24 hours. Each check can be turned off on its own.
func (m *Manager) updateCheckLoop() {
	m.checkUpdates()
	t := time.NewTicker(update.CheckInterval)
	defer t.Stop()
	for range t.C {
		m.checkUpdates()
	}
}

func (m *Manager) checkUpdates() {
	m.mu.Lock()
	checkTart := !m.cfg.DisableTartUpdateCheck
	checkOven := !m.cfg.DisableOvenUpdateCheck
	fetch := m.releaseFetcher
	m.mu.Unlock()
	if !checkTart && !checkOven {
		return
	}
	if fetch == nil {
		fetch = update.FetchLatestRelease
	}
	if checkTart {
		if latest, err := fetch(update.TartReleaseAPI); err != nil {
			log.Printf("update check: tart: %v", err)
		} else {
			m.mu.Lock()
			m.tartLatest = latest
			m.mu.Unlock()
		}
	}
	if checkOven {
		if latest, err := fetch(update.OvenReleaseAPI); err != nil {
			log.Printf("update check: tart-oven: %v", err)
		} else {
			m.mu.Lock()
			m.ovenLatest = latest
			m.mu.Unlock()
		}
	}
	m.broadcast()
}

// dismissUpdate hides the banner for the currently known latest version of
// kind ("tart" or "oven") until a newer one is found or the server restarts.
func (m *Manager) dismissUpdate(kind string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.dismissedUpdates == nil {
		m.dismissedUpdates = map[string]string{}
	}
	switch kind {
	case "tart":
		m.dismissedUpdates[kind] = m.tartLatest
	case "oven":
		m.dismissedUpdates[kind] = m.ovenLatest
	default:
		return fmt.Errorf("unknown update kind %q", kind)
	}
	return nil
}

// updatesLocked builds the update entries for the snapshot. Caller holds m.mu.
func (m *Manager) updatesLocked(tartInstalled bool) update.Views {
	var v update.Views
	if m.tartLatest != "" && !m.cfg.DisableTartUpdateCheck {
		v.Tart.Latest = strings.TrimPrefix(m.tartLatest, "v")
		v.Tart.Available = tartInstalled && update.Newer(m.tartLatest, m.tartVersion) &&
			m.dismissedUpdates["tart"] != m.tartLatest
	}
	if m.ovenLatest != "" && !m.cfg.DisableOvenUpdateCheck {
		v.Oven.Latest = strings.TrimPrefix(m.ovenLatest, "v")
		v.Oven.URL = update.OvenReleasePage
		v.Oven.Available = update.Newer(m.ovenLatest, tartoven.Version) &&
			m.dismissedUpdates["oven"] != m.ovenLatest
	}
	return v
}

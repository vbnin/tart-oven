package server

import (
	"errors"
	"tart-oven/internal/update"
	"testing"
)

func updateTestManager(fetch update.ReleaseFetcher) *Manager {
	return &Manager{cfg: defaultConfig(), tartVersion: "2.35.0", releaseFetcher: fetch,
		subs: map[chan []byte]struct{}{}}
}

func TestCheckUpdatesReportsNewerReleases(t *testing.T) {
	m := updateTestManager(func(url string) (string, error) {
		if url == update.TartReleaseAPI {
			return "2.36.0", nil
		}
		return "v99.0", nil
	})
	m.checkUpdates()
	m.mu.Lock()
	v := m.updatesLocked(true)
	m.mu.Unlock()
	if !v.Tart.Available || v.Tart.Latest != "2.36.0" {
		t.Errorf("tart = %+v", v.Tart)
	}
	if !v.Oven.Available || v.Oven.Latest != "99.0" || v.Oven.URL != update.OvenReleasePage {
		t.Errorf("oven = %+v", v.Oven)
	}

	m.mu.Lock()
	if m.updatesLocked(false).Tart.Available {
		t.Error("tart update offered while tart is not installed")
	}
	m.mu.Unlock()
}

func TestCheckUpdatesKeepsLastResultOnError(t *testing.T) {
	fail := false
	m := updateTestManager(func(url string) (string, error) {
		if fail {
			return "", errors.New("offline")
		}
		return "v99.0", nil
	})
	m.checkUpdates()
	fail = true
	m.checkUpdates()
	if m.ovenLatest != "v99.0" {
		t.Fatalf("ovenLatest = %q after a failed check", m.ovenLatest)
	}
}

func TestDisabledUpdateCheckSkipsNetworkAndBanners(t *testing.T) {
	var fetched []string
	m := updateTestManager(func(url string) (string, error) {
		fetched = append(fetched, url)
		if url == update.TartReleaseAPI {
			return "2.99.0", nil
		}
		return "v99.0", nil
	})
	m.cfg.DisableTartUpdateCheck = true
	m.checkUpdates()
	if len(fetched) != 1 || fetched[0] != update.OvenReleaseAPI {
		t.Fatalf("fetched %v with the Tart check off", fetched)
	}
	m.tartLatest = "2.99.0"
	if v := m.updatesLocked(true); v.Tart.Available || !v.Oven.Available {
		t.Fatalf("tart check off: %+v", v)
	}

	fetched = nil
	m.cfg.DisableOvenUpdateCheck = true
	m.checkUpdates()
	if len(fetched) != 0 {
		t.Fatalf("fetched %v with both checks off", fetched)
	}
	if v := m.updatesLocked(true); v.Oven.Available || v.Tart.Available {
		t.Fatalf("updates shown while disabled: %+v", v)
	}
}

func TestDismissUpdateHidesUntilNewerVersion(t *testing.T) {
	m := updateTestManager(nil)
	m.ovenLatest = "v99.0"
	m.tartLatest = "2.36.0"
	if err := m.dismissUpdate("oven"); err != nil {
		t.Fatal(err)
	}
	if err := m.dismissUpdate("tart"); err != nil {
		t.Fatal(err)
	}
	if v := m.updatesLocked(true); v.Oven.Available || v.Tart.Available {
		t.Fatalf("dismissed updates still shown: %+v", v)
	}
	m.ovenLatest = "v99.1"
	if !m.updatesLocked(true).Oven.Available {
		t.Fatal("a newer release should show the banner again")
	}
	if err := m.dismissUpdate("bogus"); err == nil {
		t.Fatal("expected an error for an unknown kind")
	}
}

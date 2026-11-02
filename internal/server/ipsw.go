package server

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"tart-oven/internal/ipsw"
)

const (
	ipswCacheTTL = 24 * time.Hour
	// The feed is one ~5 MB file with every macOS release, so on a slow link a
	// download can outlast the browser request that started it.
	ipswFetchTimeout = 5 * time.Minute
	ipswCacheFile    = "ipsw-list.json"
)

// ipswFetchFunc returns the installable restore images; replaced in tests.
type ipswFetchFunc func(ctx context.Context) ([]ipsw.Entry, error)

func fetchIPSWFeed(ctx context.Context) ([]ipsw.Entry, error) {
	return ipsw.Fetch(ctx, ipsw.DefaultClient(), ipsw.FeedURL)
}

// ipswFlight is one download of the feed. Requests that arrive while it runs
// wait for it instead of starting another.
type ipswFlight struct {
	done chan struct{}
	err  error // set before done is closed
}

// ipswDiskCache is the parsed list kept beside state.json, so a restart (or a
// failed refresh) doesn't leave the pickers empty.
type ipswDiskCache struct {
	Fetched time.Time    `json:"fetched"`
	Entries []ipsw.Entry `json:"entries"`
}

func (m *Manager) ipswCachePath() string {
	return filepath.Join(filepath.Dir(m.statePath), ipswCacheFile)
}

// loadIPSWCache restores the list saved by an earlier run. A missing or damaged
// file just means starting empty.
func (m *Manager) loadIPSWCache() {
	data, err := os.ReadFile(m.ipswCachePath())
	if err != nil {
		return
	}
	var c ipswDiskCache
	if json.Unmarshal(data, &c) != nil || len(c.Entries) == 0 {
		return
	}
	m.ipswMu.Lock()
	m.ipswEntries, m.ipswFetched = c.Entries, c.Fetched
	m.ipswMu.Unlock()
}

// saveIPSWCache writes the list atomically. Entries are saved without their
// per-request "downloaded" marks. Caller holds ipswMu.
func (m *Manager) saveIPSWCache() {
	entries := make([]ipsw.Entry, len(m.ipswEntries))
	for i, e := range m.ipswEntries {
		e.Downloaded, e.Path = false, ""
		entries[i] = e
	}
	data, err := json.Marshal(ipswDiskCache{Fetched: m.ipswFetched, Entries: entries})
	if err != nil {
		return
	}
	tmp := m.ipswCachePath() + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		os.Rename(tmp, m.ipswCachePath())
	}
}

// runIPSWFetch downloads the feed on its own deadline, not the caller's, so a
// browser that gives up doesn't throw away a nearly finished download.
func (m *Manager) runIPSWFetch(fl *ipswFlight, fetch ipswFetchFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), ipswFetchTimeout)
	defer cancel()
	fresh, err := fetch(ctx)
	m.ipswMu.Lock()
	if err != nil {
		m.logln("ipsw list: %v", err)
	} else {
		m.ipswEntries, m.ipswFetched = fresh, time.Now()
		m.saveIPSWCache()
	}
	fl.err = err
	m.ipswFlight = nil
	m.ipswMu.Unlock()
	close(fl.done)
}

// ipswSources returns the restore image list, downloading it when the saved one
// is missing or older than a day. If ctx ends first the download carries on in
// the background and the stale list (if any) comes back with ctx's error. A
// failed refresh falls back to the stale list.
func (m *Manager) ipswSources(ctx context.Context) (entries []ipsw.Entry, fetched time.Time, err error) {
	m.ipswMu.Lock()
	if len(m.ipswEntries) > 0 && time.Since(m.ipswFetched) < ipswCacheTTL {
		entries, fetched = m.ipswEntries, m.ipswFetched
		m.ipswMu.Unlock()
		return entries, fetched, nil
	}
	fl := m.ipswFlight
	if fl == nil {
		fetch := m.ipswFetch
		if fetch == nil {
			fetch = fetchIPSWFeed
		}
		fl = &ipswFlight{done: make(chan struct{})}
		m.ipswFlight = fl
		go m.runIPSWFetch(fl, fetch)
	}
	m.ipswMu.Unlock()

	var waitErr error
	select {
	case <-fl.done:
	case <-ctx.Done():
		waitErr = ctx.Err()
	}
	m.ipswMu.Lock()
	defer m.ipswMu.Unlock()
	if waitErr == nil {
		waitErr = fl.err
	}
	return m.ipswEntries, m.ipswFetched, waitErr
}

// prefetchIPSW warms the list in the background. It runs on a first run, where
// the Setup Wizard is about to ask for it; an established install only fetches
// when someone opens a picker.
func (m *Manager) prefetchIPSW() {
	ctx, cancel := context.WithTimeout(context.Background(), ipswFetchTimeout+time.Minute)
	defer cancel()
	m.ipswSources(ctx)
}

// handleIPSWSources serves GET /api/ipsw/sources.
func (m *Manager) handleIPSWSources(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	entries, fetched, err := m.ipswSources(ctx)
	// Mark images Tart has already downloaded (checked on every request, since
	// a download can finish while the dashboard is open).
	entries = ipsw.Annotate(filepath.Join(m.storage(), "cache", "IPSWs"), entries)
	resp := map[string]any{"entries": entries}
	if entries == nil {
		resp["entries"] = []ipsw.Entry{}
	}
	if !fetched.IsZero() {
		resp["fetchedAt"] = fetched
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		resp["error"] = "The macOS list is still downloading on a slow connection. Try again in a minute, or choose a file from this Mac."
	case err != nil:
		resp["error"] = "Could not load the macOS list from AppleDB: " + err.Error()
	}
	writeJSON(w, resp)
}

// finderPickerScript asks for an .ipsw in a native Finder dialog.
const finderPickerScript = `tell current application
	activate
	set f to choose file with prompt "Choose a macOS restore image (.ipsw)" of type {"ipsw", "com.apple.itunes.ipsw"} default location (path to downloads folder)
end tell
return POSIX path of f`

// errPickerCancelled means the user closed the Finder dialog.
var errPickerCancelled = errors.New("cancelled")

func chooseIPSWFile(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "osascript", "-e", finderPickerScript).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && strings.Contains(string(exitErr.Stderr), "-128") {
			return "", errPickerCancelled
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func isLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// handleIPSWChooseFile serves POST /api/ipsw/choose-file: a Finder dialog on
// this Mac. Only answered for local requests, since the dialog opens on the
// host's screen, not the remote browser's.
func (m *Manager) handleIPSWChooseFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !isLoopback(r) {
		writeJSON(w, map[string]string{"error": "The Finder window only opens on the Mac running Tart Oven. Type the path instead."})
		return
	}
	choose := m.ipswPicker
	if choose == nil {
		choose = chooseIPSWFile
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	path, err := choose(ctx)
	switch {
	case errors.Is(err, errPickerCancelled):
		writeJSON(w, map[string]bool{"cancelled": true})
	case err != nil:
		if ctx.Err() == nil { // not just the browser going away mid-dialog
			m.logln("ipsw picker: %v", err)
		}
		writeJSON(w, map[string]string{"error": "Could not open the Finder window: " + err.Error()})
	default:
		writeJSON(w, map[string]string{"path": path})
	}
}

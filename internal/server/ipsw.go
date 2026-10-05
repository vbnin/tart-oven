package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"tart-oven/internal/ipsw"
)

const ipswCacheTTL = 24 * time.Hour

// ipswFetchFunc returns the installable restore images; replaced in tests.
type ipswFetchFunc func(ctx context.Context) ([]ipsw.Entry, error)

func fetchIPSWFeed(ctx context.Context) ([]ipsw.Entry, error) {
	return ipsw.Fetch(ctx, ipsw.DefaultClient(), ipsw.FeedURL)
}

// ipswSources returns the cached restore image list, refreshing it when older
// than a day. A failed refresh falls back to the stale list.
func (m *Manager) ipswSources(ctx context.Context) (entries []ipsw.Entry, fetched time.Time, err error) {
	m.ipswMu.Lock()
	defer m.ipswMu.Unlock()
	if len(m.ipswEntries) > 0 && time.Since(m.ipswFetched) < ipswCacheTTL {
		return m.ipswEntries, m.ipswFetched, nil
	}
	fetch := m.ipswFetch
	if fetch == nil {
		fetch = fetchIPSWFeed
	}
	fresh, err := fetch(ctx)
	if err != nil {
		m.logln("ipsw list: %v", err)
		return m.ipswEntries, m.ipswFetched, err
	}
	m.ipswEntries, m.ipswFetched = fresh, time.Now()
	return fresh, m.ipswFetched, nil
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
	if err != nil {
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

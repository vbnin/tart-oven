package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"tart-oven/internal/ipsw"
)

func getSources(t *testing.T, m *Manager) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	m.handleIPSWSources(rec, httptest.NewRequest(http.MethodGet, "/api/ipsw/sources", nil))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("bad json: %v\n%s", err, rec.Body.String())
	}
	return body
}

func TestIPSWSourcesCachesTheFeed(t *testing.T) {
	m := newTestManager(t)
	calls := 0
	m.ipswFetch = func(context.Context) ([]ipsw.Entry, error) {
		calls++
		return []ipsw.Entry{{Version: "27.0.1", Build: "26A434", URL: "https://x/a.ipsw", Size: 5}}, nil
	}
	for i := 0; i < 3; i++ {
		body := getSources(t, m)
		if entries, _ := body["entries"].([]any); len(entries) != 1 {
			t.Fatalf("entries = %v", body["entries"])
		}
		if _, hasErr := body["error"]; hasErr {
			t.Fatalf("unexpected error: %v", body["error"])
		}
	}
	if calls != 1 {
		t.Fatalf("feed fetched %d times, want 1", calls)
	}
}

func TestIPSWSourcesFallsBackToStaleListOnError(t *testing.T) {
	m := newTestManager(t)
	m.ipswEntries = []ipsw.Entry{{Version: "26.6.2", URL: "https://x/old.ipsw"}}
	m.ipswFetched = m.ipswFetched.Add(-48 * ipswCacheTTL)
	m.ipswFetch = func(context.Context) ([]ipsw.Entry, error) { return nil, errors.New("offline") }
	body := getSources(t, m)
	if entries, _ := body["entries"].([]any); len(entries) != 1 {
		t.Fatalf("stale list dropped: %v", body)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "offline") {
		t.Fatalf("error = %v", body["error"])
	}
}

func TestIPSWSourcesWithNoListReturnsEmptyArray(t *testing.T) {
	m := newTestManager(t)
	m.ipswFetch = func(context.Context) ([]ipsw.Entry, error) { return nil, errors.New("offline") }
	body := getSources(t, m)
	if entries, ok := body["entries"].([]any); !ok || len(entries) != 0 {
		t.Fatalf("entries = %#v, want []", body["entries"])
	}
}

func postPicker(m *Manager, remoteAddr string) map[string]any {
	req := httptest.NewRequest(http.MethodPost, "/api/ipsw/choose-file", nil)
	req.RemoteAddr = remoteAddr
	rec := httptest.NewRecorder()
	m.handleIPSWChooseFile(rec, req)
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	return body
}

func TestIPSWChooseFile(t *testing.T) {
	m := newTestManager(t)
	m.ipswPicker = func(context.Context) (string, error) {
		return "/Users/me/Downloads/UniversalMac_27.0.1_26A434_Restore.ipsw", nil
	}
	if body := postPicker(m, "127.0.0.1:50000"); body["path"] != "/Users/me/Downloads/UniversalMac_27.0.1_26A434_Restore.ipsw" {
		t.Fatalf("body = %v", body)
	}
	if body := postPicker(m, "[::1]:50000"); body["path"] == nil {
		t.Fatalf("IPv6 loopback refused: %v", body)
	}

	m.ipswPicker = func(context.Context) (string, error) { return "", errPickerCancelled }
	if body := postPicker(m, "127.0.0.1:50000"); body["cancelled"] != true {
		t.Fatalf("cancel = %v", body)
	}

	m.ipswPicker = func(context.Context) (string, error) { t.Fatal("picker ran for a remote client"); return "", nil }
	if body := postPicker(m, "192.168.1.20:50000"); body["error"] == nil {
		t.Fatalf("remote client was not refused: %v", body)
	}

	m.ipswPicker = func(context.Context) (string, error) { return "", errors.New("osascript missing") }
	if body := postPicker(m, "127.0.0.1:50000"); !strings.Contains(body["error"].(string), "osascript missing") {
		t.Fatalf("error = %v", body)
	}
}

func TestIPSWChooseFileRejectsGet(t *testing.T) {
	m := newTestManager(t)
	rec := httptest.NewRecorder()
	m.handleIPSWChooseFile(rec, httptest.NewRequest(http.MethodGet, "/api/ipsw/choose-file", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestIPSWSourcesMarksImagesAlreadyInTartsCache(t *testing.T) {
	m := newTestManager(t)
	m.cfg.VMStoragePath = t.TempDir()
	cacheDir := filepath.Join(m.cfg.VMStoragePath, "cache", "IPSWs")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "UniversalMac_26.0_Restore.ipsw"), make([]byte, 7), 0o644); err != nil {
		t.Fatal(err)
	}
	m.ipswFetch = func(context.Context) ([]ipsw.Entry, error) {
		return []ipsw.Entry{
			{Version: "26.0", URL: "https://cdn.test/UniversalMac_26.0_Restore.ipsw", Size: 7},
			{Version: "15.6", URL: "https://cdn.test/UniversalMac_15.6_Restore.ipsw", Size: 9},
		}, nil
	}
	entries := getSources(t, m)["entries"].([]any)
	have, missing := entries[0].(map[string]any), entries[1].(map[string]any)
	if have["downloaded"] != true || have["path"] != filepath.Join(cacheDir, "UniversalMac_26.0_Restore.ipsw") {
		t.Fatalf("cached entry = %v", have)
	}
	if missing["downloaded"] != false || missing["path"] != nil {
		t.Fatalf("uncached entry = %v", missing)
	}
}

// A browser that gives up must not throw away a download that is nearly done:
// the next request gets the finished list without starting another download.
func TestIPSWDownloadSurvivesTheRequestThatStartedIt(t *testing.T) {
	m := newTestManager(t)
	release := make(chan struct{})
	var mu sync.Mutex
	calls := 0
	m.ipswFetch = func(ctx context.Context) ([]ipsw.Entry, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		select {
		case <-release:
			return []ipsw.Entry{{Version: "27.0.1", URL: "https://x/a.ipsw"}}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	short, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, _, err := m.ipswSources(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a slow download should return the caller's deadline error, got %v", err)
	}
	close(release) // the download was still running, not cancelled with the request
	entries, _, err := m.ipswSources(context.Background())
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries = %v, err = %v", entries, err)
	}
	if calls != 1 {
		t.Fatalf("the feed was downloaded %d times, want 1", calls)
	}
}

func TestIPSWConcurrentRequestsShareOneDownload(t *testing.T) {
	m := newTestManager(t)
	var mu sync.Mutex
	calls := 0
	gate := make(chan struct{})
	m.ipswFetch = func(context.Context) ([]ipsw.Entry, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		<-gate
		return []ipsw.Entry{{Version: "27.0.1", URL: "https://x/a.ipsw"}}, nil
	}
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if entries, _, err := m.ipswSources(context.Background()); err != nil || len(entries) != 1 {
				t.Errorf("entries = %v, err = %v", entries, err)
			}
		}()
	}
	time.Sleep(100 * time.Millisecond)
	close(gate)
	wg.Wait()
	if calls != 1 {
		t.Fatalf("%d concurrent requests caused %d downloads, want 1", 5, calls)
	}
}

func TestIPSWListIsKeptOnDiskAcrossRestarts(t *testing.T) {
	m := newTestManager(t)
	m.ipswFetch = func(context.Context) ([]ipsw.Entry, error) {
		return []ipsw.Entry{{Version: "27.0.1", Build: "26A434", URL: "https://x/a.ipsw", Size: 5, Downloaded: true, Path: "/p"}}, nil
	}
	if _, _, err := m.ipswSources(context.Background()); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(m.ipswCachePath())
	if err != nil {
		t.Fatalf("the list was not saved: %v", err)
	}
	if strings.Contains(string(raw), `"path"`) && strings.Contains(string(raw), `"/p"`) {
		t.Errorf("per-request download marks leaked into the saved list: %s", raw)
	}

	// A fresh process with the same state directory starts with the list and
	// does not touch the network.
	again := newTestManager(t)
	again.statePath = m.statePath
	again.ipswFetch = func(context.Context) ([]ipsw.Entry, error) {
		t.Error("downloaded again although the saved list is fresh")
		return nil, errors.New("unexpected")
	}
	again.loadIPSWCache()
	entries, fetched, err := again.ipswSources(context.Background())
	if err != nil || len(entries) != 1 || entries[0].Build != "26A434" || fetched.IsZero() {
		t.Fatalf("entries = %v, fetched = %v, err = %v", entries, fetched, err)
	}

	// Once the saved list is older than a day it is refreshed, and kept if that fails.
	again.ipswFetched = again.ipswFetched.Add(-2 * ipswCacheTTL)
	again.ipswFetch = func(context.Context) ([]ipsw.Entry, error) { return nil, errors.New("offline") }
	entries, _, err = again.ipswSources(context.Background())
	if err == nil || len(entries) != 1 {
		t.Fatalf("a failed refresh should keep the saved list: %v, %v", entries, err)
	}
}

func TestIPSWDamagedSavedListIsIgnored(t *testing.T) {
	m := newTestManager(t)
	if err := os.WriteFile(m.ipswCachePath(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	m.loadIPSWCache()
	if len(m.ipswEntries) != 0 {
		t.Fatalf("loaded %v from a damaged file", m.ipswEntries)
	}
	m.ipswFetch = func(context.Context) ([]ipsw.Entry, error) {
		return []ipsw.Entry{{Version: "27.0.1", URL: "https://x/a.ipsw"}}, nil
	}
	if entries, _, err := m.ipswSources(context.Background()); err != nil || len(entries) != 1 {
		t.Fatalf("entries = %v, err = %v", entries, err)
	}
}

func TestIPSWPrefetchWarmsTheList(t *testing.T) {
	m := newTestManager(t)
	calls := 0
	m.ipswFetch = func(context.Context) ([]ipsw.Entry, error) {
		calls++
		return []ipsw.Entry{{Version: "27.0.1", URL: "https://x/a.ipsw"}}, nil
	}
	m.prefetchIPSW()
	body := getSources(t, m)
	if entries, _ := body["entries"].([]any); len(entries) != 1 || calls != 1 {
		t.Fatalf("entries = %v after %d downloads, want the prefetched list and 1 download", body["entries"], calls)
	}
}

func TestIPSWSourcesExplainsASlowDownload(t *testing.T) {
	m := newTestManager(t)
	release := make(chan struct{})
	defer close(release)
	m.ipswFetch = func(ctx context.Context) ([]ipsw.Entry, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil, errors.New("gave up")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	rec := httptest.NewRecorder()
	m.handleIPSWSources(rec, httptest.NewRequest(http.MethodGet, "/api/ipsw/sources", nil).WithContext(ctx))
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	msg, _ := body["error"].(string)
	if !strings.Contains(msg, "still downloading") || !strings.Contains(msg, "file from this Mac") {
		t.Fatalf("error = %q", msg)
	}
}

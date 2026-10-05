package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

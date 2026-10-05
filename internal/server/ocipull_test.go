package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestValidateOCIImageURI(t *testing.T) {
	valid := []string{
		"ghcr.io/cirruslabs/macos-sonoma-base:latest",
		"ghcr.io/cirruslabs/macos-sequoia-base:latest",
		"ghcr.io/cirruslabs/macos-tahoe-base:latest",
		"docker.io/library/macos:15",
		"registry.internal.corp/team/macos-runner:v1.2",
		"localhost:5000/myorg/macos-runner:latest",
	}
	for _, u := range valid {
		got, err := validateOCIImageURI(u)
		if err != nil || got != u {
			t.Errorf("validateOCIImageURI(%q) unexpected error: %v, got: %q", u, err, got)
		}
	}

	invalid := []string{
		"",
		"   ",
		"ghcr.io/cirruslabs/macos-sonoma-base; rm -rf /",
		"image with spaces",
		"ghcr.io/`touch bad`",
		"image$(whoami)",
		"http://ghcr.io/image:latest",
	}
	for _, u := range invalid {
		if _, err := validateOCIImageURI(u); err == nil {
			t.Errorf("validateOCIImageURI(%q) expected error but passed", u)
		}
	}
}

func TestCheckFreeDiskSpace(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "ocipull-disk-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	// Host should have at least 1000 bytes free
	free, err := checkFreeDiskSpace(tempDir, 1000)
	if err != nil {
		t.Fatalf("checkFreeDiskSpace unexpected error: %v", err)
	}
	if free == 0 {
		t.Errorf("expected non-zero free space")
	}

	// Insufficient space check with an impossibly large requirement (100 Petabytes)
	const impossible = 100 * 1024 * 1024 * 1024 * 1024 * 1024
	_, err = checkFreeDiskSpace(tempDir, impossible)
	if err == nil || !strings.Contains(err.Error(), "insufficient disk space") {
		t.Errorf("expected insufficient disk space error, got: %v", err)
	}
}

func TestHandleOCIPull(t *testing.T) {
	m := newTestManager(t)
	mux := m.routes()

	// 1. Method not allowed
	req := httptest.NewRequest(http.MethodGet, "/api/oci/pull", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 Method Not Allowed, got %d", rec.Code)
	}

	// 2. Bad JSON
	req = httptest.NewRequest(http.MethodPost, "/api/oci/pull", bytes.NewBufferString(`{invalid json`))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request, got %d", rec.Code)
	}

	// 3. Invalid image URI payload
	req = httptest.NewRequest(http.MethodPost, "/api/oci/pull", bytes.NewBufferString(`{"image":"bad;image"}`))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request, got %d", rec.Code)
	}

	// 4. Valid payload starts task
	validBody, _ := json.Marshal(ociPullReq{
		Image:    "ghcr.io/cirruslabs/macos-sonoma-base:latest",
		Insecure: true,
	})
	req = httptest.NewRequest(http.MethodPost, "/api/oci/pull", bytes.NewBuffer(validBody))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}
	var res map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || res["ok"] != true {
		t.Fatalf("unexpected response: %v", rec.Body.String())
	}
	if res["image"] != "ghcr.io/cirruslabs/macos-sonoma-base:latest" {
		t.Fatalf("expected image in response, got %v", res["image"])
	}
	if res["taskId"] == nil || res["taskId"] == "" {
		t.Fatalf("expected non-empty taskId in response, got %v", res["taskId"])
	}

	// 5. Duplicate pull prevention
	req = httptest.NewRequest(http.MethodPost, "/api/oci/pull", bytes.NewBuffer(validBody))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict for duplicate pull, got %d", rec.Code)
	}
}

package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestFreeSpaceUsesClosestExistingParent(t *testing.T) {
	dir := t.TempDir()
	free, exists, err := freeSpaceAt(dir)
	if err != nil || !exists || free == 0 {
		t.Fatalf("existing dir: free=%d exists=%v err=%v", free, exists, err)
	}
	free, exists, err = freeSpaceAt(filepath.Join(dir, "not", "created", "yet"))
	if err != nil || exists || free == 0 {
		t.Fatalf("missing dir: free=%d exists=%v err=%v", free, exists, err)
	}
}

func TestSetupCheckReportsHostAndStorage(t *testing.T) {
	m, h := newAuthTestManager(t)
	m.cfg.VMStoragePath = t.TempDir()
	rec := do(h, "GET", "/api/setup/check", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var out struct {
		AppleSilicon *bool `json:"appleSilicon"`
		Storage      struct {
			Path          string `json:"path"`
			Exists        bool   `json:"exists"`
			FreeBytes     uint64 `json:"freeBytes"`
			RequiredBytes uint64 `json:"requiredBytes"`
			Enough        bool   `json:"enough"`
		} `json:"storage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.AppleSilicon == nil {
		t.Fatal("appleSilicon missing")
	}
	if out.Storage.Path != m.cfg.VMStoragePath || !out.Storage.Exists || out.Storage.RequiredBytes != 40<<30 {
		t.Fatalf("storage = %+v", out.Storage)
	}
	if out.Storage.Enough != (out.Storage.FreeBytes >= 40<<30) {
		t.Fatalf("enough flag disagrees with free bytes: %+v", out.Storage)
	}
}

func TestSetupCheckMissingStorageIsNotAnError(t *testing.T) {
	m, h := newAuthTestManager(t)
	m.cfg.VMStoragePath = filepath.Join(t.TempDir(), "later")
	rec := do(h, "GET", "/api/setup/check", "", nil)
	var out struct {
		Storage struct {
			Exists bool   `json:"exists"`
			Error  string `json:"error"`
		} `json:"storage"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Storage.Exists || out.Storage.Error != "" {
		t.Fatalf("storage = %+v", out.Storage)
	}
	if _, err := os.Stat(m.cfg.VMStoragePath); err == nil {
		t.Fatal("the check must not create the folder")
	}
}

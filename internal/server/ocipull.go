package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"syscall"
)

var ociImageRegex = regexp.MustCompile(`^[a-zA-Z0-9_.-]+(:[0-9]+)?(/[a-zA-Z0-9_.-]+)+(:[a-zA-Z0-9_.-]+)?$`)

// minPullFreeBytes is 25 GiB
const minPullFreeBytes uint64 = 25 * 1024 * 1024 * 1024

// validateOCIImageURI validates and sanitizes an OCI image reference.
func validateOCIImageURI(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("image URI cannot be empty")
	}
	if strings.ContainsAny(raw, " \t\n\r;|&`$><(){}[]\\\"'") {
		return "", errors.New("image URI contains invalid characters")
	}
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		return "", errors.New("image URI must not include http:// or https:// protocol scheme")
	}
	if !ociImageRegex.MatchString(raw) {
		return "", fmt.Errorf("invalid OCI image reference format: %q", raw)
	}
	return raw, nil
}

// checkFreeDiskSpace checks available bytes on the filesystem backing path.
func checkFreeDiskSpace(path string, minBytes uint64) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	freeBytes := stat.Bavail * uint64(stat.Bsize)
	if freeBytes < minBytes {
		return freeBytes, fmt.Errorf("insufficient disk space: %d GB available, requires at least %d GB", freeBytes/(1024*1024*1024), minBytes/(1024*1024*1024))
	}
	return freeBytes, nil
}

type ociPullReq struct {
	Image    string `json:"image"`
	Insecure bool   `json:"insecure"`
}

func (m *Manager) isPullRunning(image string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.tasks {
		if t.Kind == "pull" && t.Target == image && t.Status == "running" {
			return true
		}
	}
	return false
}

func (m *Manager) handleOCIPull(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var req ociPullReq
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	image, err := validateOCIImageURI(req.Image)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if m.isPullRunning(image) {
		http.Error(w, "a pull for this image is already in progress", http.StatusConflict)
		return
	}

	storageDir := m.storage()
	if _, err := checkFreeDiskSpace(storageDir, minPullFreeBytes); err != nil {
		http.Error(w, err.Error(), http.StatusInsufficientStorage)
		return
	}

	t := m.newTask("pull", image)
	m.broadcast()

	go func() {
		args := []string{"pull"}
		if req.Insecure {
			args = append(args, "--insecure")
		}
		args = append(args, image)

		err := m.runInto(t, args...)
		m.finishTask(t, err)
		if err == nil {
			m.reconcile()
		}
		m.broadcast()
	}()

	writeJSON(w, map[string]interface{}{
		"ok":     true,
		"taskId": t.ID,
		"image":  image,
	})
}

package server

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

// recommendedFreeBytes is the free space the setup wizard asks for in the VM
// storage location: room for a base image plus a few clones. Falling short is
// a warning, never a block (pulls enforce their own, lower minimum).
const recommendedFreeBytes uint64 = 40 * 1024 * 1024 * 1024

// detectHost reports whether the Mac has an Apple silicon chip, and its name.
// hw.optional.arm64 stays 1 under Rosetta, so an x86_64 build running on an
// Apple silicon Mac is still recognised.
func detectHost() (appleSilicon bool, chip string) {
	if out, err := exec.Command("sysctl", "-n", "hw.optional.arm64").Output(); err == nil {
		appleSilicon = strings.TrimSpace(string(out)) == "1"
	} else {
		appleSilicon = runtime.GOARCH == "arm64"
	}
	if out, err := exec.Command("sysctl", "-n", "machdep.cpu.brand_string").Output(); err == nil {
		chip = strings.TrimSpace(string(out))
	}
	return appleSilicon, chip
}

// freeSpaceAt returns the free bytes on the volume holding path. A folder that
// doesn't exist yet is measured through its closest existing parent.
func freeSpaceAt(path string) (free uint64, exists bool, err error) {
	probe := path
	for {
		if _, statErr := os.Stat(probe); statErr == nil {
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			break
		}
		probe = parent
	}
	_, statErr := os.Stat(path)
	exists = statErr == nil
	var st syscall.Statfs_t
	if err := syscall.Statfs(probe, &st); err != nil {
		return 0, exists, err
	}
	return st.Bavail * uint64(st.Bsize), exists, nil
}

func (m *Manager) registerSetupRoutes(mux *http.ServeMux) {
	// GET: what the setup wizard needs to know that the dashboard state
	// doesn't already carry.
	mux.HandleFunc("/api/setup/check", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		path := m.cfg.VMStoragePath
		m.mu.Unlock()

		silicon, chip := detectHost()
		storage := map[string]any{
			"path":          path,
			"requiredBytes": recommendedFreeBytes,
		}
		if free, exists, err := freeSpaceAt(path); err != nil {
			storage["error"] = err.Error()
		} else {
			storage["exists"] = exists
			storage["freeBytes"] = free
			storage["enough"] = free >= recommendedFreeBytes
		}
		writeJSON(w, map[string]any{"appleSilicon": silicon, "chip": chip, "storage": storage})
	})
}

package server

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// ---------------------------------------------------------------------------
// Editable constants.
//
// These mirror the proven values from the original bash script. Change them
// here if your setup differs; everything that is meant to be tuned at runtime
// lives in Config instead.
// ---------------------------------------------------------------------------
const (
	// defaultStoragePath is the preferred TART_HOME (external SSD). If it is not
	// mounted at first launch, fallbackStoragePath is used as the initial value.
	defaultStoragePath  = "/Volumes/EXTERNAL_SSD/Tart"
	fallbackStoragePath = "/Users/Shared/Tart"

	// defaultSharedDir is bind-mounted into each VM via --dir=host_resources:...
	defaultSharedDir = "/Users/Shared/Tart/Resources"

	// jamfBin is run as "jamf recon" after start/stop when JamfRecon is enabled.
	jamfBin = "/usr/local/bin/jamf"

	// templateMarker: any VM whose name contains this is never auto-selected.
	templateMarker = "TEMPLATE"

	// hardMaxConcurrent is the absolute ceiling on simultaneously running VMs.
	// Apple's Virtualization.framework refuses to run more than 2 VMs at once,
	// so this is enforced regardless of the configured MaxConcurrent.
	hardMaxConcurrent = 2
)

// defaultTartBin is the full path to the tart binary used by default. The
// TartAppPath config holds this exact path — we never guess/derive it.
const defaultTartBin = "/Applications/tart.app/Contents/MacOS/tart"

// tartInstalledAt reports whether the configured tart binary exists.
func tartInstalledAt(binPath string) bool {
	fi, err := os.Stat(binPath)
	return err == nil && !fi.IsDir()
}

// appBundleFromBin derives the tart.app bundle directory from a binary path of
// the standard form .../tart.app/Contents/MacOS/tart, for install/update. If the
// path isn't in that shape, it falls back to the default /Applications location.
func appBundleFromBin(binPath string) string {
	const suffix = "/Contents/MacOS/tart"
	if strings.HasSuffix(binPath, suffix) {
		dest := strings.TrimSuffix(binPath, suffix)
		// Only replace the derived directory if it actually looks like a
		// tart.app bundle. Otherwise a user-supplied tartAppPath such as
		// "/some/dir/Contents/MacOS/tart" would make install/update `rm -rf`
		// an arbitrary directory; fall back to the safe default instead.
		if strings.HasSuffix(dest, ".app") {
			return dest
		}
	}
	return "/Applications/tart.app"
}

// ansiEscape matches terminal CSI escape sequences (cursor moves, line erase,
// etc.) that `tart create --from-ipsw` emits to redraw its progress bar in a
// real terminal; captured into a task log they show up as garbled bytes, so
// they're stripped before display.
var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

func stripANSI(s string) string {
	return ansiEscape.ReplaceAllString(s, "")
}

// boundedBuffer is a thread-safe writer that keeps only the last max bytes,
// used to capture the tail of a detached `tart run` process's output.
type boundedBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.max {
		b.buf = b.buf[len(b.buf)-b.max:]
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(string(b.buf))
}

// rotatingWriter wraps a file and rotates it when it exceeds maxBytes.
// Thread-safe for concurrent writes.
type rotatingWriter struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	file     *os.File
}

func newRotatingWriter(path string, maxBytes int64) (*rotatingWriter, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &rotatingWriter{path: path, maxBytes: maxBytes, file: f}, nil
}

func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.file == nil {
		f, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return 0, err
		}
		w.file = f
	}

	fi, err := w.file.Stat()
	if err == nil && fi.Size()+int64(len(p)) > w.maxBytes {
		_ = w.file.Close()
		_ = os.Rename(w.path, w.path+".1")
		newFile, openErr := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if openErr != nil {
			return 0, openErr
		}
		w.file = newFile
	}
	return w.file.Write(p)
}

func (w *rotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.file.Close()
}

// setupLogging configures log output to both stderr and a rotating file.
func setupLogging(logPath string) {
	if logPath == "" {
		return
	}
	logPath = expandHome(logPath)
	rw, err := newRotatingWriter(logPath, 5*1024*1024) // 5 MB max
	if err != nil {
		log.Printf("warning: could not open log file %s: %v (logging to stderr only)", logPath, err)
		return
	}
	log.SetOutput(io.MultiWriter(os.Stderr, rw))
	log.SetFlags(log.LstdFlags)
}

// expandHome replaces a leading ~ with the user's home directory.
// validSSHKeyPath rejects blank and relative identity paths. A relative path would
// resolve against the server's working directory — "/" under the LaunchAgent — so
// the key would be written to, and read from, somewhere the operator never intended.
func validSSHKeyPath(path string) bool {
	path = strings.TrimSpace(path)
	return strings.HasPrefix(path, "~/") || strings.HasPrefix(path, "/")
}

func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

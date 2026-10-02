// tart-oven - a small fleet manager for macOS VMs running under Tart.
//
// One binary, standard library only. It runs `tart` commands directly (as the
// logged-in console user via a LaunchAgent, so no sudo / user-switching), keeps
// all state in a single Manager struct guarded by a mutex, persists to a small
// JSON file, drives automatic VM runs from a scheduler goroutine, and serves a
// live dashboard over HTTP + Server-Sent Events.
//
// Build:   go build -o tart-oven
// Run:     ./tart-oven            (reads/writes ~/.tart-oven/state.json)
package main

import (
	"context"
	cryptorand "crypto/rand"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/net/route"
)

//go:embed index.html README.md CHANGELOG.md
var content embed.FS

const version = "1.54"

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

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

// Config is the user-tunable state. It is persisted and editable from the
// dashboard. Durations are kept as plain minutes/seconds so the JSON and the
// HTML form stay simple.
type JamfProfile struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	BaseURL        string `json:"baseUrl"`
	InvitationCode string `json:"invitationCode,omitempty"`
}

type jamfProfileView struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	BaseURL           string `json:"baseUrl"`
	InvitationCodeSet bool   `json:"invitationCodeSet"`
}

type Config struct {
	Listen                  string        `json:"listen"`                  // host:port to bind, e.g. 0.0.0.0:8080
	VMStoragePath           string        `json:"vmStoragePath"`           // becomes TART_HOME on every tart call
	SharedDir               string        `json:"sharedDir"`               // host_resources bind mount
	TartAppPath             string        `json:"tartAppPath"`             // full path to the tart binary
	IntervalMinutes         int           `json:"intervalMinutes"`         // how often the scheduler acts
	WindowMinutes           int           `json:"windowMinutes"`           // how long each VM stays up
	MaxConcurrent           int           `json:"maxConcurrent"`           // max VMs running at once
	SchedulerMode           string        `json:"schedulerMode"`           // "random" | "sequential" (alphabetical)
	Excluded                []string      `json:"excluded"`                // VM names never auto-selected
	ExcludeOCIFromScheduler bool          `json:"excludeOciFromScheduler"` // keep cached OCI images clone-only by default
	NoGraphics              bool          `json:"noGraphics"`              // run VMs headless without a GUI window (--no-graphics)
	NoAudio                 bool          `json:"noAudio"`                 // disable host audio pass-through (--no-audio)
	JamfRecon               bool          `json:"jamfRecon"`               // run `jamf recon` after start/stop
	Paused                  bool          `json:"paused"`                  // global scheduler pause
	DailyEnabled            bool          `json:"dailyEnabled"`            // gate auto-runs to a daily time window
	DailyStart              string        `json:"dailyStart"`              // "HH:MM" — begin running VMs
	DailyStop               string        `json:"dailyStop"`               // "HH:MM" — stop running VMs
	JamfProfiles            []JamfProfile `json:"jamfProfiles,omitempty"`
	JamfBaseURL             string        `json:"jamfBaseUrl,omitempty"`
	JamfInvitationCode      string        `json:"jamfInvitationCode,omitempty"`
	SSHUser                 string        `json:"sshUser"`
	SSHPassword             string        `json:"sshPassword"`           // guest SSH/sudo password; write-only
	SSHKey                  string        `json:"sshKey"`                // identity file for the SSH fallback
	SSHFallbackEnabled      bool          `json:"sshFallbackEnabled"`    // allow SSH when a guest has no Tart guest agent
	PrioritizeSSHShutdown   bool          `json:"prioritizeSshShutdown"` // try a clean SSH shutdown before the fast tart stop
	SSHTimeoutSec           int           `json:"sshTimeoutSec"`         // ssh connect timeout
	StatusCommand           string        `json:"statusCommand"`         // command for "Get info"
	RunArgs                 string        `json:"runArgs"`               // extra args appended to every `tart run`
	NetPriority             string        `json:"netPriority"`           // "auto" | "wifi" | "ethernet" | "shared"
	BootTimeoutSec          int           `json:"bootTimeoutSec"`        // wait for IP before declaring boot failure
	HistoryDays             int           `json:"historyDays"`           // run-history retention in days
	LogPath                 string        `json:"logPath"`               // path to log file (rotation at 5MB)
	ServerLabel             string        `json:"serverLabel"`           // custom label to identify this server instance
	ShowRunningOnly         bool          `json:"showRunningOnly"`       // dashboard filter: show only running VMs
	FirstRunCompleted       bool          `json:"firstRunCompleted"`     // whether initial setup wizard has completed
	OperatorRole            string        `json:"operatorRole"`          // operator persona preset (e.g. "jamf", "general")
}

type configView struct {
	Config
	JamfProfiles          []jamfProfileView `json:"jamfProfiles"`
	SSHPasswordSet        bool              `json:"sshPasswordSet"`
	JamfInvitationCodeSet bool              `json:"jamfInvitationCodeSet"`
}

func newConfigView(cfg Config) configView {
	view := configView{
		Config:                cfg,
		SSHPasswordSet:        cfg.SSHPassword != "",
		JamfInvitationCodeSet: cfg.JamfInvitationCode != "",
		JamfProfiles:          make([]jamfProfileView, len(cfg.JamfProfiles)),
	}
	view.SSHPassword = ""
	view.JamfInvitationCode = ""
	for i, p := range cfg.JamfProfiles {
		view.JamfProfiles[i] = jamfProfileView{
			ID:                p.ID,
			Name:              p.Name,
			BaseURL:           p.BaseURL,
			InvitationCodeSet: p.InvitationCode != "",
		}
	}
	return view
}

func normalizeJamfBaseURL(raw string) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return "", errors.New("Jamf Pro base URL must be an http or https URL with a hostname")
	}
	return raw, nil
}

func effectiveSSHCredentials(cfg Config, vm *VM) (string, string) {
	user, password := cfg.SSHUser, cfg.SSHPassword
	if vm != nil && vm.SSHUser != "" {
		user = vm.SSHUser
	}
	if vm != nil && vm.SSHPassword != "" {
		password = vm.SSHPassword
	}
	return user, password
}

// legacyJamfUserStatusCommand is the status command shipped before the MDM column
// existed. Its trailing lookup printed "(no Jamf user)" whenever the read failed for
// any reason — including on VMs that were enrolled but simply had no username-variable
// profile scoped to them. The MDM column answers that question properly now. Replaced
// on load only when a stored command matches this byte-for-byte.
const legacyJamfUserStatusCommand = `hostname; ioreg -c IOPlatformExpertDevice -d 2 | awk -F \" '/IOPlatformSerialNumber/{print $(NF-1)}'; sw_vers -productVersion; defaults read /Library/Managed\ Preferences/com.jamf.usernamevariable.plist jamfProUsername 2>/dev/null || echo "(no Jamf user)"`

func defaultConfig() Config {
	return Config{
		Listen:                  "127.0.0.1:9000",
		VMStoragePath:           fallbackStoragePath, // /Users/Shared/Tart
		SharedDir:               defaultSharedDir,
		TartAppPath:             defaultTartBin,
		IntervalMinutes:         5,
		WindowMinutes:           120,
		MaxConcurrent:           1,
		SchedulerMode:           "sequential",
		Excluded:                []string{},
		ExcludeOCIFromScheduler: true,
		NoGraphics:              false,
		NoAudio:                 false,
		JamfRecon:               false,
		Paused:                  true, // scheduler OFF until the user turns it on
		DailyEnabled:            true,
		DailyStart:              "08:30",
		DailyStop:               "22:00",
		SSHUser:                 "admin",
		SSHPassword:             "admin",
		SSHKey:                  "~/.ssh/tart-oven",
		SSHFallbackEnabled:      true,
		SSHTimeoutSec:           15,
		StatusCommand:           `hostname; ioreg -c IOPlatformExpertDevice -d 2 | awk -F \" '/IOPlatformSerialNumber/{print $(NF-1)}'; sw_vers -productVersion`,
		RunArgs:                 "",
		NetPriority:             "wifi",
		BootTimeoutSec:          60,
		HistoryDays:             60,
		LogPath:                 "~/Library/Logs/tart-oven.log",
		FirstRunCompleted:       false,
		OperatorRole:            "",
	}
}

// splitArgs turns a free-text "custom arguments" string into argv, honouring
// single and double quotes so values like --net-bridged="Wi-Fi" survive intact.
func splitArgs(s string) []string {
	var args []string
	var cur strings.Builder
	inSingle, inDouble := false, false
	flush := func() {
		if cur.Len() > 0 {
			args = append(args, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		switch {
		case r == '\'' && !inDouble:
			inSingle = !inSingle
		case r == '"' && !inSingle:
			inDouble = !inDouble
		case (r == ' ' || r == '\t' || r == '\n' || r == '\r') && !inSingle && !inDouble:
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return args
}

// hasArg reports whether target flag is present in args (either exact match or with =value).
func hasArg(args []string, flag string) bool {
	for _, a := range args {
		if a == flag || strings.HasPrefix(a, flag+"=") {
			return true
		}
	}
	return false
}

// parseHHMM parses "HH:MM" into minutes-since-midnight.
func parseHHMM(s string) (int, bool) {
	var h, m int
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d:%d", &h, &m); err != nil {
		return 0, false
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

// inDailyWindow reports whether now falls within [start, stop). Handles windows
// that wrap past midnight (start > stop). Invalid/equal bounds mean "always".
func inDailyWindow(now time.Time, start, stop string) bool {
	ps, ok1 := parseHHMM(start)
	pe, ok2 := parseHHMM(stop)
	if !ok1 || !ok2 || ps == pe {
		return true
	}
	cur := now.Hour()*60 + now.Minute()
	if ps < pe {
		return cur >= ps && cur < pe
	}
	return cur >= ps || cur < pe // overnight window
}

// VM is a single managed virtual machine. State is one of:
// stopped | starting | running | stopping.
type VM struct {
	Name         string    `json:"name"`
	Source       string    `json:"source,omitempty"`
	Disk         int       `json:"disk,omitempty"`     // virtual disk capacity reported by Tart, in GB
	Size         int       `json:"size,omitempty"`     // cached storage used, in GB
	Accessed     string    `json:"accessed,omitempty"` // RFC3339 in JSON mode; relative text in table fallback
	State        string    `json:"state"`
	IP           string    `json:"ip,omitempty"`
	StartedAt    time.Time `json:"startedAt,omitempty"`
	StopAt       time.Time `json:"stopAt,omitempty"`
	LastRun      time.Time `json:"lastRun,omitempty"`      // last time this VM was started
	BootFailed   bool      `json:"bootFailed,omitempty"`   // started but never got an IP
	SSHOK        bool      `json:"sshOk,omitempty"`        // last SSH connectivity check passed
	SSHCheckedAt time.Time `json:"sshCheckedAt,omitempty"` // when SSH was last checked
	Info         string    `json:"info,omitempty"`         // last "Get info" (status command) output
	InfoAt       time.Time `json:"infoAt,omitempty"`       // when Info was last fetched

	MDMEnrolled  bool      `json:"mdmEnrolled,omitempty"`  // guest reports an active MDM enrollment
	MDMServer    string    `json:"mdmServer,omitempty"`    // raw MDM check-in URL from the guest
	MDMCheckedAt time.Time `json:"mdmCheckedAt,omitempty"` // zero means never probed, which renders as unknown

	AutoEnroll bool `json:"autoEnroll,omitempty"` // set at clone time; next boot (manual or scheduler) runs the MDM auto-enroll script once

	PendingProvisioning string `json:"pendingProvisioning,omitempty"` // set at create time (fresh --from-ipsw only); rendered `key=value,...` consumed by the next `tart run` and cleared once it boots successfully

	Notes       string   `json:"notes,omitempty"`       // user-entered notes for tracking/inventory
	Tags        []string `json:"tags,omitempty"`        // user-defined tags for grouping/filtering
	SSHUser     string   `json:"sshUser,omitempty"`     // custom SSH user (overrides default)
	SSHPassword string   `json:"sshPassword,omitempty"` // custom SSH/sudo password; persisted, but masked before every client-facing response

	LastError string `json:"lastError,omitempty"`

	// Computed for the UI in stateSnapshot (not persisted meaningfully).
	AgentOK  bool `json:"agentOk"` // guest agent answered the last command
	Template bool `json:"template"`
	Excluded bool `json:"excluded"`
	Busy     bool `json:"busy"`
}

// RunEvent is one entry in the run history: a VM that was turned on. StoppedAt
// is filled in when the VM stops, so the history shows duration too.
type RunEvent struct {
	Name      string    `json:"name"`
	StartedAt time.Time `json:"startedAt"`
	StoppedAt time.Time `json:"stoppedAt,omitempty"`
	IP        string    `json:"ip,omitempty"`
	Trigger   string    `json:"trigger"` // "scheduler" | "manual"
}

// Task tracks a long-running management operation (create / clone) so the UI
// can show live progress. Kept in memory only (not persisted).
type Task struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`   // create | clone
	Target     string    `json:"target"` // VM name being produced
	Status     string    `json:"status"` // running | success | error | cancelled
	Output     string    `json:"output"` // tail of combined stdout/stderr
	Error      string    `json:"error,omitempty"`
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt,omitempty"`

	lastBcast time.Time          // throttles progress broadcasts; not serialized
	ctx       context.Context    // bound to the in-flight command(s); cancelling it kills them
	cancel    context.CancelFunc // cancels ctx; nil once the task has finished
}

// Manager holds everything, guarded by mu.
type Manager struct {
	mu                   sync.Mutex
	cfg                  Config
	vms                  map[string]*VM
	history              []*RunEvent // run log, pruned to cfg.HistoryDays
	tasks                []*Task     // recent create/clone operations
	logs                 []string    // rolling tart command log (last ~200 lines)
	performanceCollector *performanceCollector
	performanceHistory   []PerformanceSample
	hostIP               string               // local IP of the host Mac
	tartVersion          string               // `tart --version`, refreshed periodically
	lastSequential       string               // last VM started in sequential scheduler mode
	busy                 map[string]bool      // VMs with an op in flight (start/stop)
	opStart              map[string]time.Time // when each busy op started (to detect stuck ops)
	runningCmds          map[string]*exec.Cmd // live `tart run` processes
	agentProbedSinceBoot map[string]bool      // whether execViaAgent has been tried since this VM last (re)started
	subs                 map[chan []byte]struct{}
	storageMounted       bool
	tartJSON             bool // whether `tart list --format json` is supported
	supportsProvisioning bool // whether the host (macOS 27+) and tart build support `run --provisioning-opts`
	statePath            string
	reload               chan struct{} // poke the scheduler when interval changes
	mdmCopier            mdmProfileCopier
	mdmResolveIP         mdmIPResolver
	tartOutputOperation  func(context.Context, string, ...string) (string, error)
	sshOperation         func(context.Context, string, string, string) execResult
	runningProbe         func(string) (bool, error)
}

// persisted is the on-disk shape of state.json.
type persisted struct {
	Config  Config         `json:"config"`
	VMs     map[string]*VM `json:"vms"`
	History []*RunEvent    `json:"history"`
}

// stateSnapshot is what we send to the dashboard (GET /api/vms and SSE).
type stateSnapshot struct {
	VMs                  []*VM             `json:"vms"`
	Config               configView        `json:"config"`
	StorageMounted       bool              `json:"storageMounted"`
	StoragePath          string            `json:"storagePath"`
	WithinHours          bool              `json:"withinHours"` // currently inside the daily window
	Now                  time.Time         `json:"now"`
	Version              string            `json:"version"`
	TartJSON             bool              `json:"tartJSON"`
	TartInstalled        bool              `json:"tartInstalled"`
	TartVersion          string            `json:"tartVersion"`
	SupportsProvisioning bool              `json:"supportsProvisioning"`
	Tasks                []*Task           `json:"tasks"`
	Performance          PerformanceSample `json:"performance"`
	HostIP               string            `json:"hostIP"`
	Logs                 []string          `json:"logs"`
}

// tartVM matches the JSON emitted by `tart list --format json`.
type tartVM struct {
	Source   string `json:"Source"`
	Name     string `json:"Name"`
	Disk     int    `json:"Disk"`
	Size     int    `json:"Size"`
	Accessed string `json:"Accessed"`
	State    string `json:"State"`
	Running  bool   `json:"Running"`
}

// ---------------------------------------------------------------------------
// Persistence
// ---------------------------------------------------------------------------

func (m *Manager) load() {
	m.cfg = defaultConfig()
	data, err := os.ReadFile(m.statePath)
	if err != nil {
		return // first run
	}
	var p persisted
	if err := json.Unmarshal(data, &p); err != nil {
		log.Printf("state.json is corrupt, starting fresh: %v", err)
		return
	}
	// Overlay persisted config onto defaults, then repair any missing fields so
	// an old/partial file can't leave us with zero intervals etc.
	m.cfg = p.Config
	d := defaultConfig()
	// A plain bool cannot distinguish an older state file with no field from an
	// explicit false. Detect field presence so upgrades receive the safe default
	// while users can still opt OCI images back into scheduling.
	var presence struct {
		Config struct {
			ExcludeOCIFromScheduler *bool `json:"excludeOciFromScheduler"`
			SSHFallbackEnabled      *bool `json:"sshFallbackEnabled"`
		} `json:"config"`
	}
	if err := json.Unmarshal(data, &presence); err == nil {
		if presence.Config.ExcludeOCIFromScheduler == nil {
			m.cfg.ExcludeOCIFromScheduler = d.ExcludeOCIFromScheduler
		}
		if presence.Config.SSHFallbackEnabled == nil {
			m.cfg.SSHFallbackEnabled = d.SSHFallbackEnabled
		}
	}
	if m.cfg.Listen == "" {
		m.cfg.Listen = d.Listen
	}
	if m.cfg.VMStoragePath == "" {
		m.cfg.VMStoragePath = d.VMStoragePath
	}
	if m.cfg.SharedDir == "" {
		m.cfg.SharedDir = d.SharedDir
	}
	// Migrate the earlier directory-style value to a full binary path.
	if m.cfg.TartAppPath == "" || m.cfg.TartAppPath == "/Applications/" {
		m.cfg.TartAppPath = d.TartAppPath
	}
	if m.cfg.IntervalMinutes < 1 {
		m.cfg.IntervalMinutes = d.IntervalMinutes
	}
	if m.cfg.WindowMinutes < 1 {
		m.cfg.WindowMinutes = d.WindowMinutes
	}
	if m.cfg.MaxConcurrent < 1 {
		m.cfg.MaxConcurrent = d.MaxConcurrent
	}
	if m.cfg.MaxConcurrent > hardMaxConcurrent {
		m.cfg.MaxConcurrent = hardMaxConcurrent
	}
	if m.cfg.SchedulerMode != "sequential" {
		m.cfg.SchedulerMode = "random"
	}
	if m.cfg.SSHUser == "" {
		m.cfg.SSHUser = d.SSHUser
	}
	if m.cfg.SSHPassword == "" {
		m.cfg.SSHPassword = d.SSHPassword
	}
	if m.cfg.SSHTimeoutSec < 1 {
		m.cfg.SSHTimeoutSec = d.SSHTimeoutSec
	}
	if m.cfg.StatusCommand == "" {
		m.cfg.StatusCommand = d.StatusCommand
	}
	if m.cfg.StatusCommand == legacyJamfUserStatusCommand {
		m.cfg.StatusCommand = d.StatusCommand
	}
	if !validSSHKeyPath(m.cfg.SSHKey) {
		m.cfg.SSHKey = d.SSHKey
	}
	if m.cfg.Excluded == nil {
		m.cfg.Excluded = []string{}
	}
	if m.cfg.HistoryDays < 1 {
		m.cfg.HistoryDays = d.HistoryDays
	}
	if m.cfg.BootTimeoutSec < 10 {
		m.cfg.BootTimeoutSec = d.BootTimeoutSec
	}
	if m.cfg.NetPriority != "wifi" && m.cfg.NetPriority != "ethernet" && m.cfg.NetPriority != "shared" {
		m.cfg.NetPriority = "auto"
	}
	// Older state files predate the daily window; seed it (enabled) on upgrade.
	if m.cfg.DailyStart == "" || m.cfg.DailyStop == "" {
		m.cfg.DailyEnabled = true
		m.cfg.DailyStart = d.DailyStart
		m.cfg.DailyStop = d.DailyStop
	}
	// Older state files predate file logging; seed with default path.
	if m.cfg.LogPath == "" {
		m.cfg.LogPath = d.LogPath
	}
	if p.VMs != nil {
		m.vms = p.VMs
	}
	if p.History != nil {
		m.history = p.History
	}
	m.pruneHistory()
	// Seed the sequential cursor from the most recently run VM so sequential
	// scheduling continues from where it left off across restarts.
	var latest time.Time
	for name, vm := range m.vms {
		if vm.LastRun.After(latest) {
			latest = vm.LastRun
			m.lastSequential = name
		}
	}
}

// pruneHistory drops events older than the retention window. Caller MUST hold mu.
func (m *Manager) pruneHistory() {
	cutoff := time.Now().AddDate(0, 0, -m.cfg.HistoryDays)
	kept := m.history[:0]
	for _, ev := range m.history {
		if ev.StartedAt.After(cutoff) {
			kept = append(kept, ev)
		}
	}
	m.history = kept
}

// save writes state atomically. Caller MUST hold m.mu.
func (m *Manager) save() {
	p := persisted{Config: m.cfg, VMs: m.vms, History: m.history}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		log.Printf("marshal state: %v", err)
		return
	}
	tmp := m.statePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		log.Printf("write state: %v", err)
		return
	}
	if err := os.Rename(tmp, m.statePath); err != nil {
		log.Printf("rename state: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Tart command helpers. Every call sets TART_HOME to the configured storage.
// None of these hold m.mu, so callers must not hold it across exec either
// (we read the storage path under a short lock).
// ---------------------------------------------------------------------------

func (m *Manager) storage() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg.VMStoragePath
}

func (m *Manager) tartCmd(home string, args ...string) *exec.Cmd {
	m.mu.Lock()
	bin := m.cfg.TartAppPath
	m.mu.Unlock()
	c := exec.Command(bin, args...)
	c.Env = append(os.Environ(), "TART_HOME="+home)
	return c
}

// tartCmdCtx is tartCmd with a context, so a hung tart can be killed on timeout
// instead of pinning an operation (and its busy flag) forever.
func (m *Manager) tartCmdCtx(ctx context.Context, home string, args ...string) *exec.Cmd {
	m.mu.Lock()
	bin := m.cfg.TartAppPath
	m.mu.Unlock()
	c := exec.CommandContext(ctx, bin, args...)
	c.Env = append(os.Environ(), "TART_HOME="+home)
	return c
}

// tartOutputTimeout runs a tart command with a hard timeout and returns stdout.
func (m *Manager) tartOutputTimeout(d time.Duration, home string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return m.tartOutputContext(ctx, home, args...)
}

func (m *Manager) tartOutputContext(ctx context.Context, home string, args ...string) (string, error) {
	if m.tartOutputOperation != nil {
		return m.tartOutputOperation(ctx, home, args...)
	}
	out, err := m.tartCmdCtx(ctx, home, args...).Output()
	return string(out), err
}

// pollVMIP retries transient resolver failures until an IP appears or the
// caller's deadline expires. Tart's ARP resolver can fail immediately while
// the guest is still booting (for example, when `arp -an` is temporarily
// empty), even when `tart ip --wait` was requested.
func pollVMIP(ctx context.Context, retryInterval time.Duration, probe func(context.Context) (string, error)) (string, error) {
	if retryInterval <= 0 {
		retryInterval = time.Millisecond
	}
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("resolve VM IP before deadline: %w", err)
		}
		ip, err := probe(ctx)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", fmt.Errorf("resolve VM IP before deadline: %w", ctxErr)
		}
		if ip = strings.TrimSpace(ip); err == nil && ip != "" {
			return ip, nil
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = errors.New("Tart returned an empty VM IP")
		}

		timer := time.NewTimer(retryInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", fmt.Errorf("resolve VM IP before deadline (last error: %v): %w", lastErr, ctx.Err())
		case <-timer.C:
		}
	}
}

type arpNeighbor struct {
	IP  net.IP
	MAC net.HardwareAddr
}

// parseFlexMAC normalizes single-digit hex octets (common in Darwin arp output)
// into valid 2-digit hex pairs so net.ParseMAC can parse them correctly.
func parseFlexMAC(s string) (net.HardwareAddr, error) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) == 6 {
		for i, p := range parts {
			if len(p) == 1 {
				parts[i] = "0" + p
			}
		}
		return net.ParseMAC(strings.Join(parts, ":"))
	}
	return net.ParseMAC(strings.TrimSpace(s))
}

// parseARPOutput parses the output of `arp -an` on macOS.
func parseARPOutput(output string) []arpNeighbor {
	var entries []arpNeighbor
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "? (") {
			continue
		}
		openParen := strings.Index(line, "(")
		closeParen := strings.Index(line, ") at ")
		if openParen < 0 || closeParen <= openParen {
			continue
		}
		ipStr := line[openParen+1 : closeParen]
		ip := net.ParseIP(ipStr)
		if ip == nil {
			continue
		}
		rest := line[closeParen+len(") at "):]
		fields := strings.Fields(rest)
		if len(fields) < 1 {
			continue
		}
		mac, err := parseFlexMAC(fields[0])
		if err != nil {
			continue
		}
		entries = append(entries, arpNeighbor{IP: ip, MAC: mac})
	}
	return entries
}

// hostARPNeighbors retrieves active ARP entries by querying `arp -an` and Darwin routing table.
func hostARPNeighbors() ([]arpNeighbor, error) {
	var all []arpNeighbor
	seen := make(map[string]bool)

	// 1. Query /usr/sbin/arp -an
	for _, cmdPath := range []string{"/usr/sbin/arp", "arp"} {
		out, err := exec.Command(cmdPath, "-an").Output()
		if err == nil && len(out) > 0 {
			for _, entry := range parseARPOutput(string(out)) {
				key := entry.IP.String() + "|" + entry.MAC.String()
				if !seen[key] {
					seen[key] = true
					all = append(all, entry)
				}
			}
			break
		}
	}

	// 2. Query Darwin routing table
	native, _ := nativeARPNeighbors()
	for _, entry := range native {
		key := entry.IP.String() + "|" + entry.MAC.String()
		if !seen[key] {
			seen[key] = true
			all = append(all, entry)
		}
	}

	return all, nil
}

// resolveVMIP reads Tart's configured MAC address for a VM and matches it
// against the host's neighbor table.
func resolveVMIP(home, name string, neighbors func() ([]arpNeighbor, error)) (string, error) {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return "", fmt.Errorf("invalid VM name %q", name)
	}

	configPath := filepath.Join(home, "vms", name, "config.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		configPath = filepath.Join(home, name, "config.json")
		data, err = os.ReadFile(configPath)
		if err != nil {
			return "", fmt.Errorf("read VM network config: %w", err)
		}
	}
	var config struct {
		MACAddress string `json:"macAddress"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return "", fmt.Errorf("parse VM network config: %w", err)
	}
	target, err := parseFlexMAC(config.MACAddress)
	if err != nil {
		return "", fmt.Errorf("parse VM MAC address %q: %w", config.MACAddress, err)
	}

	entries, err := neighbors()
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if entry.IP != nil && entry.MAC.String() == target.String() {
			return entry.IP.String(), nil
		}
	}
	return "", fmt.Errorf("no neighbor-table entry for VM MAC %s", target)
}

// nativeARPNeighbors reads the Darwin routing information base directly.
func nativeARPNeighbors() ([]arpNeighbor, error) {
	ribType := route.RIBType(syscall.NET_RT_FLAGS)
	rib, err := route.FetchRIB(syscall.AF_UNSPEC, ribType, syscall.RTF_LLINFO)
	if err != nil {
		return nil, fmt.Errorf("read native neighbor table: %w", err)
	}
	messages, err := route.ParseRIB(ribType, rib)
	if err != nil {
		return nil, fmt.Errorf("parse native neighbor table: %w", err)
	}
	return arpNeighborsFromRouteMessages(messages), nil
}

func arpNeighborsFromRouteMessages(messages []route.Message) []arpNeighbor {
	entries := make([]arpNeighbor, 0, len(messages))
	for _, message := range messages {
		routeMessage, ok := message.(*route.RouteMessage)
		if !ok || len(routeMessage.Addrs) <= syscall.RTAX_GATEWAY {
			continue
		}
		destination, ok := routeMessage.Addrs[syscall.RTAX_DST].(*route.Inet4Addr)
		if !ok {
			continue
		}
		link, ok := routeMessage.Addrs[syscall.RTAX_GATEWAY].(*route.LinkAddr)
		if !ok || len(link.Addr) != 6 {
			continue
		}
		mac := append(net.HardwareAddr(nil), link.Addr...)
		entries = append(entries, arpNeighbor{IP: net.IP(destination.IP[:]), MAC: mac})
	}
	return entries
}

// maxOpAge is how long a start/stop op may stay "busy" before we consider it
// stuck (e.g. tart hung or the daemon was restarted mid-op) and clear it so the
// VM's real state can be reconciled. It is comfortably above the worst-case
// bounded duration of a stop (SSH shutdown + waits + tart-stop fallback).
const maxOpAge = 4 * time.Minute

// setBusy marks/unmarks a VM as having an op in flight, tracking when it began.
// Caller must hold m.mu.
func (m *Manager) setBusy(name string, b bool) {
	if b {
		m.busy[name] = true
		m.opStart[name] = time.Now()
	} else {
		delete(m.busy, name)
		delete(m.opStart, name)
	}
}

// healStuck clears the busy flag for ops that have run longer than maxAge, so a
// VM can't get wedged in "starting"/"stopping" forever. The next reconcile then
// sets its true state from tart.
func (m *Manager) healStuck(maxAge time.Duration) {
	now := time.Now()
	var cleared []string
	m.mu.Lock()
	for name := range m.busy {
		if started, ok := m.opStart[name]; ok && now.Sub(started) > maxAge {
			delete(m.busy, name)
			delete(m.opStart, name)
			cleared = append(cleared, name)
		}
	}
	m.mu.Unlock()
	for _, n := range cleared {
		m.logln("op on %q exceeded %s; clearing stuck busy flag", n, maxAge)
	}
}

// forceRefresh is the manual "Refresh VM status": heal any stuck ops, then
// reconcile against tart so the list and all states reflect reality.
func (m *Manager) forceRefresh() {
	m.checkStorage()
	m.healStuck(maxOpAge)
	m.reconcile()
	m.broadcast()
}

// updateTartVersion refreshes the cached `tart --version` string.
func (m *Manager) updateTartVersion() {
	v := ""
	if out, err := m.tartCmd(m.storage(), "--version").CombinedOutput(); err == nil {
		v = strings.TrimSpace(string(out))
	}
	m.mu.Lock()
	m.tartVersion = v
	m.mu.Unlock()
}

// logln records a line in the rolling tart log (shown in the UI) and the
// server log. Callers must NOT hold m.mu.
func (m *Manager) logln(format string, a ...any) {
	line := time.Now().Format("15:04:05") + "  " + fmt.Sprintf(format, a...)
	log.Print(line)
	m.mu.Lock()
	m.logs = append(m.logs, line)
	if len(m.logs) > 200 {
		m.logs = m.logs[len(m.logs)-200:]
	}
	m.mu.Unlock()
}

func (m *Manager) tartOutput(home string, args ...string) (string, error) {
	out, err := m.tartCmd(home, args...).Output()
	// Surface failures (except the noisy internal `list` polling) so the user
	// can see why a tart command went wrong.
	if err != nil && (len(args) == 0 || args[0] != "list") {
		stderr := ""
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			stderr = strings.TrimSpace(string(ee.Stderr))
		}
		m.logln("tart %s → %v %s", strings.Join(args, " "), err, stderr)
	}
	return string(out), err
}

// detectTartJSON probes once whether this tart version supports --format json.
func (m *Manager) detectTartJSON() {
	out, err := m.tartOutput(m.storage(), "list", "--format", "json")
	if err != nil {
		m.tartJSON = false
		return
	}
	var v []tartVM
	m.tartJSON = json.Unmarshal([]byte(out), &v) == nil
}

// hostMacOSMajorVersion returns the host's macOS major version (e.g. 27 for
// "27.0.1"), or 0 if it can't be determined.
func hostMacOSMajorVersion() int {
	out, err := exec.Command("sw_vers", "-productVersion").Output()
	if err != nil {
		return 0
	}
	major := strings.SplitN(strings.TrimSpace(string(out)), ".", 2)[0]
	n, err := strconv.Atoi(major)
	if err != nil {
		return 0
	}
	return n
}

// detectProvisioningSupport probes once whether this host + tart build can
// accept `run --provisioning-opts`. macOS 27 added the underlying guest
// provisioning API, but tart itself gates the flag behind a host OS-version
// check *and* a toolchain check (it's compiled out on older Swift toolchains),
// so checking the tart version number alone isn't enough — grep `--help`.
func (m *Manager) detectProvisioningSupport() {
	supported := false
	if hostMacOSMajorVersion() >= 27 {
		out, err := m.tartCmd(m.storage(), "run", "--help").CombinedOutput()
		if err == nil && strings.Contains(string(out), "--provisioning-opts") {
			supported = true
		}
	}
	m.supportsProvisioning = supported
}

// renderProvisioningOpts builds the `key=value,...` string tart expects for
// `run --provisioning-opts`, from the options given at create time. String
// fields are only included when actually set; the boolean toggles are always
// included since both false and true are meaningful choices.
func renderProvisioningOpts(fullName, username, password string, autoLogin, remoteLogin bool) string {
	var parts []string
	if fullName != "" {
		parts = append(parts, "fullName="+fullName)
	}
	if username != "" {
		parts = append(parts, "username="+username)
	}
	if password != "" {
		parts = append(parts, "password="+password)
	}
	parts = append(parts, fmt.Sprintf("logsInAutomatically=%t", autoLogin))
	parts = append(parts, fmt.Sprintf("enablesRemoteLogin=%t", remoteLogin))
	return strings.Join(parts, ",")
}

// redactProvisioningOpts masks the cleartext password inside a
// `--provisioning-opts=...` argument before it's written to the log, so the
// guest password never lands in ~/Library/Logs/tart-oven.log. Any other
// argument is returned unchanged.
func redactProvisioningOpts(arg string) string {
	const prefix = "--provisioning-opts="
	if !strings.HasPrefix(arg, prefix) {
		return arg
	}
	parts := strings.Split(strings.TrimPrefix(arg, prefix), ",")
	for i, p := range parts {
		if strings.HasPrefix(p, "password=") {
			parts[i] = "password=***REDACTED***"
		}
	}
	return prefix + strings.Join(parts, ",")
}

// listTart returns the current VMs straight from tart (the source of truth).
// It uses a hard timeout so a hung `tart list` can't stall the monitor loop.
func (m *Manager) listTart() ([]tartVM, error) {
	home := m.storage()
	if m.tartJSON {
		out, err := m.tartOutputTimeout(20*time.Second, home, "list", "--format", "json")
		if err != nil {
			return nil, err
		}
		var v []tartVM
		if err := json.Unmarshal([]byte(out), &v); err != nil {
			return nil, err
		}
		// Normalise State from the Running flag if needed.
		for i := range v {
			if v[i].State == "" {
				if v[i].Running {
					v[i].State = "running"
				} else {
					v[i].State = "stopped"
				}
			}
		}
		return v, nil
	}
	return m.parseTartTable(home)
}

// parseTartTable is the fallback for tart versions without JSON output.
// Expected columns: Source Name Disk Size Accessed State. Accessed can contain
// spaces (for example "3 hours ago"), so it occupies every field before State.
func (m *Manager) parseTartTable(home string) ([]tartVM, error) {
	out, err := m.tartOutputTimeout(20*time.Second, home, "list")
	if err != nil {
		return nil, err
	}
	var vms []tartVM
	for i, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		// Skip the header row.
		if i == 0 && strings.EqualFold(fields[0], "Source") {
			continue
		}
		if len(fields) < 6 {
			continue
		}
		name := fields[1]
		disk, _ := strconv.Atoi(fields[2])
		size, _ := strconv.Atoi(fields[3])
		accessed := strings.Join(fields[4:len(fields)-1], " ")
		state := strings.ToLower(fields[len(fields)-1])
		vms = append(vms, tartVM{Source: fields[0], Name: name, Disk: disk, Size: size, Accessed: accessed, State: state})
	}
	return vms, nil
}

func runJamf() {
	if err := exec.Command(jamfBin, "recon").Run(); err != nil {
		log.Printf("jamf recon (ignored): %v", err)
	}
}

// netService is a parsed network service from networksetup.
type netService struct {
	name   string // e.g. "Wi-Fi", "USB 10/100/1000 LAN"
	device string // e.g. "en0", "en7"
}

// isWifi returns true for Wi-Fi services.
func isWifi(name string) bool {
	return strings.Contains(name, "Wi-Fi")
}

// isEthernet returns true for wired Ethernet services (USB LAN, Thunderbolt Ethernet, etc).
func isEthernet(name string) bool {
	s := strings.ToLower(name)
	if strings.Contains(s, "bridge") {
		return false
	}
	return strings.Contains(s, "lan") || strings.Contains(s, "ethernet") || strings.Contains(s, "thunderbolt")
}

// listNetServices parses networksetup -listnetworkserviceorder output.
func listNetServices() ([]netService, error) {
	out, err := exec.Command("networksetup", "-listnetworkserviceorder").Output()
	if err != nil {
		return nil, fmt.Errorf("networksetup: %w", err)
	}
	// Match lines like: (1) Wi-Fi ... (Hardware Port: Wi-Fi, Device: en0)
	nameRe := regexp.MustCompile(`^\((\d+)\)\s+(.+)`)
	devRe := regexp.MustCompile(`Device:\s*([A-Za-z0-9]+)\)`)
	var services []netService
	lines := strings.Split(string(out), "\n")
	for i := 0; i < len(lines); i++ {
		if m := nameRe.FindStringSubmatch(lines[i]); m != nil {
			svcName := strings.TrimSpace(m[2])
			// Next line has the device
			if i+1 < len(lines) {
				if dm := devRe.FindStringSubmatch(lines[i+1]); dm != nil {
					services = append(services, netService{name: svcName, device: dm[1]})
				}
			}
		}
	}
	return services, nil
}

// isActive checks if a network device is active via ifconfig.
func isActive(device string) bool {
	out, err := exec.Command("ifconfig", device).Output()
	return err == nil && strings.Contains(string(out), "status: active")
}

// activeInterface finds an active network interface, optionally prioritizing
// Wi-Fi ("wifi") or Ethernet ("ethernet"). Falls back to any active interface.
func activeInterface(priority string) (string, error) {
	services, err := listNetServices()
	if err != nil {
		return "", err
	}

	// Collect active interfaces, categorized
	var preferred, fallback []string
	for _, svc := range services {
		if !isActive(svc.device) {
			continue
		}
		match := false
		switch priority {
		case "wifi":
			match = isWifi(svc.name)
		case "ethernet":
			match = isEthernet(svc.name)
		}
		if match {
			preferred = append(preferred, svc.device)
		} else {
			fallback = append(fallback, svc.device)
		}
	}

	if len(preferred) > 0 {
		return preferred[0], nil
	}
	if len(fallback) > 0 {
		return fallback[0], nil
	}
	return "", errors.New("no active network interface found")
}

// localIP returns the host's local IP address on the active interface.
func localIP() string {
	iface, err := activeInterface("auto")
	if err != nil {
		return ""
	}
	out, err := exec.Command("ipconfig", "getifaddr", iface).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// ---------------------------------------------------------------------------
// Reconciliation & storage status
// ---------------------------------------------------------------------------

// reconcile syncs our map with reality from `tart list`. Tart is authoritative
// for existence and running-state; we preserve our own timers (StartedAt/StopAt)
// for VMs that are genuinely still running. VMs with an op in flight are left
// alone so we don't clobber a transient starting/stopping state.
func (m *Manager) reconcile() {
	list, err := m.listTart()
	if err != nil {
		log.Printf("tart list failed (storage unmounted?): %v", err)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	seen := make(map[string]bool, len(list))
	for _, t := range list {
		seen[t.Name] = true
		vm := m.vms[t.Name]
		if vm == nil {
			vm = &VM{Name: t.Name}
			m.vms[t.Name] = vm
		}
		vm.Source = t.Source
		vm.Disk = t.Disk
		vm.Size = t.Size
		vm.Accessed = t.Accessed
		if m.busy[t.Name] {
			continue // mid-operation; leave transient state untouched
		}
		vm.State = t.State
		if t.State != "running" {
			// Clear only the live-session timers; keep last known IP / SSH
			// status / Info for reference after the VM stops.
			vm.StartedAt = time.Time{}
			vm.StopAt = time.Time{}
		}
	}
	// Drop VMs that no longer exist in tart (and aren't mid-operation).
	for name := range m.vms {
		if !seen[name] && !m.busy[name] {
			delete(m.vms, name)
		}
	}
}

// ensureSharedDir creates the host_resources bind-mount folder if it's
// missing. `tart run` crashes outright if the --dir target doesn't exist, so
// this must run before any VM starts (called once at startup).
func (m *Manager) ensureSharedDir() {
	m.mu.Lock()
	dir := m.cfg.SharedDir
	m.mu.Unlock()
	if dir == "" {
		return
	}
	if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("shared dir %q missing and could not be created: %v", dir, err)
		return
	}
	log.Printf("created missing shared dir %q", dir)
}

// checkStorage updates the mounted flag and broadcasts on change.
func (m *Manager) checkStorage() {
	m.mu.Lock()
	p := m.cfg.VMStoragePath
	m.mu.Unlock()

	fi, err := os.Stat(p)
	ok := err == nil && fi.IsDir()

	m.mu.Lock()
	changed := ok != m.storageMounted
	m.storageMounted = ok
	m.mu.Unlock()
	if changed {
		m.broadcast()
	}
}

// ---------------------------------------------------------------------------
// VM operations. doRun/doStop perform the slow exec work WITHOUT holding the
// lock, marking the VM busy so the scheduler and reconcile won't touch it.
// HTTP handlers invoke these in their own goroutines; the scheduler invokes
// them inline (it is already its own goroutine).
// ---------------------------------------------------------------------------

func (m *Manager) failOp(name string, err error) {
	log.Printf("op on %q failed: %v", name, err)
	m.mu.Lock()
	if vm := m.vms[name]; vm != nil {
		vm.State = "stopped"
		vm.LastError = err.Error()
	}
	m.setBusy(name, false)
	m.mu.Unlock()
	m.broadcast()
}

func (m *Manager) doRun(name, trigger string) {
	m.mu.Lock()
	if m.busy[name] {
		m.mu.Unlock()
		return
	}
	vm := m.vms[name]
	if vm == nil {
		vm = &VM{Name: name}
		m.vms[name] = vm
	}
	if deferVMStartForHistory(m.performanceHistory) {
		if vm.State == "" {
			vm.State = "stopped"
		}
		vm.LastError = "not started: host is under critical memory pressure"
		m.mu.Unlock()
		m.broadcast()
		m.logln("deferred start of %s: host is under critical memory pressure", name)
		return
	}
	// Hard concurrency ceiling: Virtualization.framework allows at most 2 VMs.
	// Enforced here so it covers manual runs as well as the scheduler.
	active := 0
	for n, v := range m.vms {
		if n != name && (v.State == "running" || v.State == "starting") {
			active++
		}
	}
	if active >= hardMaxConcurrent {
		vm.LastError = fmt.Sprintf("not started: %d VMs already running (host max %d)", active, hardMaxConcurrent)
		m.mu.Unlock()
		m.broadcast()
		log.Printf("refusing to start %q: %d already running", name, active)
		return
	}
	now := time.Now()
	m.setBusy(name, true)
	vm.State = "starting"
	vm.LastError = ""
	vm.LastRun = now
	// A fresh boot deserves a fresh guest-agent probe (see execInGuest).
	delete(m.agentProbedSinceBoot, name)
	// Advance the sequential-scheduler cursor on every start (manual or
	// scheduler) so the next sequential pick continues after whatever last ran.
	m.lastSequential = name
	// Log this run to the history; keep the pointer so we can fill in the IP
	// once it resolves and the StoppedAt when it later stops.
	ev := &RunEvent{Name: name, StartedAt: now, Trigger: trigger}
	m.history = append(m.history, ev)
	m.pruneHistory()
	home := m.cfg.VMStoragePath
	shared := m.cfg.SharedDir
	jamf := m.cfg.JamfRecon
	extra := splitArgs(m.cfg.RunArgs)
	statusCmd := m.cfg.StatusCommand
	window := time.Duration(m.cfg.WindowMinutes) * time.Minute
	bootTimeout := m.cfg.BootTimeoutSec
	netPriority := m.cfg.NetPriority
	pendingProvisioning := vm.PendingProvisioning
	supportsProvisioning := m.supportsProvisioning
	m.save()
	m.mu.Unlock()
	m.broadcast()

	// "shared" opts out of bridged networking entirely (tart's own default:
	// shared/NAT through the host), for networks — typically enterprise
	// Wi-Fi with client/MAC isolation — that silently drop a bridged VM's
	// second MAC address at the access point, leaving the guest with a
	// DHCP lease but no route to its gateway.
	var iface string
	if netPriority != "shared" {
		var err error
		iface, err = activeInterface(netPriority)
		if err != nil {
			m.logln("run %s: no active network interface: %v", name, err)
			m.failOp(name, err)
			return
		}
	}

	// tart run blocks for the life of the VM, so start it detached and reap it
	// in the background to avoid zombies. Any user-supplied custom arguments
	// (e.g. --vnc, --no-audio) are appended last. We capture its stdout/stderr
	// into runLog so a boot failure surfaces tart's actual error message.
	runArgs := []string{"run", name,
		"--dir=host_resources:" + shared}
	if netPriority != "shared" && !hasArg(extra, "--net-bridged") {
		// Only auto-inject the detected interface if the user hasn't already
		// supplied their own --net-bridged in Custom run arguments — tart
		// treats --net-bridged as a repeatable flag, so both would apply and
		// bridge the guest onto the same physical interface twice.
		runArgs = append(runArgs, "--net-bridged="+iface)
	}
	if m.cfg.NoGraphics && !hasArg(extra, "--no-graphics") {
		runArgs = append(runArgs, "--no-graphics")
	}
	if m.cfg.NoAudio && !hasArg(extra, "--no-audio") {
		runArgs = append(runArgs, "--no-audio")
	}
	runArgs = append(runArgs, extra...)
	if pendingProvisioning != "" && supportsProvisioning {
		runArgs = append(runArgs, "--provisioning-opts="+pendingProvisioning)
	}
	logArgs := make([]string, len(runArgs))
	for i, a := range runArgs {
		logArgs[i] = redactProvisioningOpts(a) // never let the guest password reach the log file
	}
	m.logln("$ tart %s", strings.Join(logArgs, " "))
	runLog := &boundedBuffer{max: 8192}
	cmd := m.tartCmd(home, runArgs...)
	cmd.Stdout = runLog
	cmd.Stderr = runLog
	if err := cmd.Start(); err != nil {
		m.logln("run %s: failed to start tart: %v", name, err)
		m.failOp(name, err)
		return
	}
	m.mu.Lock()
	m.runningCmds[name] = cmd
	m.mu.Unlock()
	go func(targetCmd *exec.Cmd) {
		werr := targetCmd.Wait()
		m.mu.Lock()
		if m.runningCmds[name] == targetCmd {
			delete(m.runningCmds, name)
		}
		busy := m.busy[name]
		m.mu.Unlock()
		if werr != nil {
			// A quick non-zero exit usually means tart couldn't start the VM.
			m.logln("tart run %s exited: %v%s", name, werr, prefixIfNotEmpty("\n", runLog.String()))
		}
		// The `tart run` process ended — if it wasn't us stopping it (e.g. tart
		// quit on its own), reconcile so the UI reflects the VM as stopped
		// promptly instead of waiting for the next monitor tick.
		if !busy {
			m.reconcile()
			m.broadcast()
		}
	}(cmd)

	if jamf {
		go runJamf()
	}

	// Resolve the IP by matching Tart's configured VM MAC against the host's
	// ARP table and Tart IP resolvers. Keep polling until the guest receives an IP
	// or the configured boot deadline expires.
	ipCtx, cancelIP := context.WithTimeout(context.Background(), time.Duration(bootTimeout)*time.Second)
	ip, ipErr := pollVMIP(ipCtx, 1500*time.Millisecond, func(ctx context.Context) (string, error) {
		return m.resolveVMIPRobust(ctx, home, name, 1)
	})
	cancelIP()

	// Boot failure: the VM came up but never handed us an IP. Stop it so it
	// doesn't hold a concurrency slot, flag it, and let the next scheduler tick
	// pick a different VM.
	if ipErr != nil || ip == "" {
		tartOut := runLog.String()
		m.logln("boot failure %s: no IP after %ds.%s", name, bootTimeout, prefixIfNotEmpty(" tart said: ", tartOut))
		m.tartCmd(home, "stop", name, "-t", "10").Run()
		m.mu.Lock()
		c := m.runningCmds[name]
		m.mu.Unlock()
		if c != nil && c.Process != nil {
			c.Process.Kill()
		}
		if jamf {
			go runJamf()
		}
		detail := "boot failure: no IP after boot"
		if tartOut != "" {
			detail += " — " + lastLine(tartOut)
		}
		m.mu.Lock()
		vm.State = "stopped"
		vm.IP = ""
		vm.StartedAt = time.Time{}
		vm.StopAt = time.Time{}
		vm.BootFailed = true
		vm.LastError = detail
		ev.StoppedAt = time.Now() // close the history entry
		m.setBusy(name, false)
		m.save()
		m.mu.Unlock()
		m.broadcast()
		return
	}

	m.mu.Lock()
	vm.State = "running"
	vm.IP = ip
	vm.StartedAt = time.Now()
	vm.StopAt = time.Now().Add(window)
	vm.BootFailed = false // a clean boot clears any previous failure flag
	vm.LastError = ""
	vm.SSHOK = false              // pending check; UI shows "checking…"
	vm.SSHCheckedAt = time.Time{} //
	ev.IP = ip
	if pendingProvisioning != "" {
		// First boot resolved an IP, so the guest applied the provisioning
		// options — don't reapply them on the next start.
		vm.PendingProvisioning = ""
	}
	m.setBusy(name, false)
	m.save()
	m.mu.Unlock()
	m.broadcast()
	log.Printf("started %q (ip=%s, window=%s)", name, ip, window)

	// Run the "Get info" (status) command over SSH: this both verifies SSH
	// connectivity (green/red bubble) and populates the Info column.
	res := m.sshExec(name, statusCmd, "")
	ok, info := sshOutcome(res)
	m.mu.Lock()
	vm.SSHOK = ok
	vm.SSHCheckedAt = time.Now()
	vm.Info = info
	vm.InfoAt = time.Now()
	m.save()
	m.mu.Unlock()
	m.broadcast()
	m.logln("info %s: ok=%v", name, ok)

	m.refreshMDMStatus(name)
	m.broadcast()

	// A VM cloned with "Auto enroll at boot" runs the MDM enrollment script
	// once it's up, whether this boot was the scheduler's or a manual Run —
	// that's the whole point of the flag. Skip it if already enrolled so a
	// VM that re-runs later (e.g. the scheduler cycling back to it) doesn't
	// redo an enrollment it already has. The trigger != "auto-enroll" guard
	// stops this from firing a second time when it was autoEnroll itself
	// that called doRun to boot a stopped VM — that caller runs the
	// enrollment script right after doRun returns, so re-triggering here
	// would enroll the same VM twice concurrently.
	m.mu.Lock()
	needsEnroll := vm.AutoEnroll && !vm.MDMEnrolled
	m.mu.Unlock()
	if needsEnroll && trigger != "auto-enroll" {
		m.runAutoEnrollTask(name, trigger)
	}
}

// sshPriorityShutdownCommand is the guest command run when "Prioritize shutdown
// using SSH" is enabled — a clean macOS shutdown request, tried before the fast
// tart stop.
const sshPriorityShutdownCommand = "sudo shutdown -h now"

// sshPriorityShutdownWait bounds how long a prioritized SSH shutdown gets before
// falling back to the standard tart stop.
const sshPriorityShutdownWait = 30 * time.Second

func (m *Manager) doStop(name string) {
	m.mu.Lock()
	if m.busy[name] {
		m.mu.Unlock()
		return
	}
	vm := m.vms[name]
	if vm == nil {
		m.mu.Unlock()
		return
	}
	m.setBusy(name, true)
	vm.State = "stopping"
	home := m.cfg.VMStoragePath
	jamf := m.cfg.JamfRecon
	prioritizeSSH := m.cfg.PrioritizeSSHShutdown
	ip := vm.IP
	sudoPassword := vm.SSHPassword
	if sudoPassword == "" {
		sudoPassword = m.cfg.SSHPassword
	}
	m.mu.Unlock()
	m.broadcast()

	stopped := false

	// Preferred, when enabled: ask the guest to shut down cleanly over SSH,
	// bounded to sshPriorityShutdownWait. The connection usually drops as the
	// guest powers off, so the command's own result is ignored — what matters
	// is whether the VM actually stops within the deadline.
	if prioritizeSSH && ip != "" {
		deadline := time.Now().Add(sshPriorityShutdownWait)
		m.logln("$ ssh %s %q", name, sshPriorityShutdownCommand)
		sshCtx, cancel := context.WithDeadline(context.Background(), deadline)
		m.sshExecContext(sshCtx, name, sshPriorityShutdownCommand, sudoPassword)
		cancel()
		for time.Now().Before(deadline) {
			if !m.isRunning(name) {
				stopped = true
				break
			}
			time.Sleep(1 * time.Second)
		}
		if stopped {
			m.logln("%s shut down cleanly via SSH", name)
		} else {
			m.logln("%s did not stop via SSH within %s; falling back to tart stop", name, sshPriorityShutdownWait)
		}
	}

	if !stopped {
		m.logln("$ tart stop %s -t 5", name)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		out, err := m.tartCmdCtx(ctx, home, "stop", name, "-t", "5").CombinedOutput()
		cancel()
		if err != nil {
			m.logln("tart stop %s → %v%s", name, err, prefixIfNotEmpty(" ", strings.TrimSpace(string(out))))
		}

		for i := 0; i < 6; i++ {
			time.Sleep(500 * time.Millisecond)
			if !m.isRunning(name) {
				stopped = true
				break
			}
		}
		if !stopped {
			m.logln("%s still running; forcing process termination", name)
			m.mu.Lock()
			cmd := m.runningCmds[name]
			m.mu.Unlock()
			if cmd != nil && cmd.Process != nil {
				cmd.Process.Kill()
			} else {
				m.tartCmd(home, "stop", name, "-t", "1").Run()
			}
		}
	}

	if jamf {
		go runJamf()
	}

	m.mu.Lock()
	vm.State = "stopped"
	vm.StartedAt = time.Time{}
	vm.StopAt = time.Time{}
	// Keep last known IP, SSH status, and Info as a reference after the VM stops.
	// Close the most recent open history event for this VM.
	for i := len(m.history) - 1; i >= 0; i-- {
		if m.history[i].Name == name && m.history[i].StoppedAt.IsZero() {
			m.history[i].StoppedAt = time.Now()
			break
		}
	}
	m.setBusy(name, false)
	m.save()
	m.mu.Unlock()
	m.broadcast()
	log.Printf("stopped %q", name)
}

func (m *Manager) runSSHOperation(ctx context.Context, name, command, password string) execResult {
	if m.sshOperation != nil {
		return m.sshOperation(ctx, name, command, password)
	}
	return m.sshExecContext(ctx, name, command, password)
}

func (m *Manager) probeVMRunning(name string) (bool, error) {
	if m.runningProbe != nil {
		return m.runningProbe(name)
	}
	return m.vmRunningState(name)
}

func (m *Manager) finishVMRun(name, state string) {
	m.mu.Lock()
	if vm := m.vms[name]; vm != nil {
		vm.State = state
		vm.StartedAt = time.Time{}
		vm.StopAt = time.Time{}
		vm.LastError = ""
	}
	for i := len(m.history) - 1; i >= 0; i-- {
		if m.history[i].Name == name && m.history[i].StoppedAt.IsZero() {
			m.history[i].StoppedAt = time.Now()
			break
		}
	}
	m.setBusy(name, false)
	m.save()
	m.mu.Unlock()
	m.broadcast()
}

func (m *Manager) vmRunningState(name string) (bool, error) {
	list, err := m.listTart()
	if err != nil {
		return false, err
	}
	for _, t := range list {
		if t.Name == name {
			return t.State == "running", nil
		}
	}
	return false, nil
}

func (m *Manager) isRunning(name string) bool {
	running, _ := m.vmRunningState(name)
	return running
}

// ---------------------------------------------------------------------------
// Scheduler
// ---------------------------------------------------------------------------

func isOCI(source string) bool {
	return strings.EqualFold(strings.TrimSpace(source), "OCI")
}

func eligibleForScheduler(vm *VM, excludeOCI bool, excluded map[string]bool, busy bool) bool {
	if vm == nil || vm.State != "stopped" || busy {
		return false
	}
	if strings.Contains(vm.Name, templateMarker) || excluded[vm.Name] {
		return false
	}
	return !excludeOCI || !isOCI(vm.Source)
}

// schedulerLoop waits for the configured interval and ticks. The interval is
// re-read each cycle and the wait is interruptible via the reload channel so
// config changes take effect immediately.
func (m *Manager) schedulerLoop() {
	for {
		m.mu.Lock()
		iv := time.Duration(m.cfg.IntervalMinutes) * time.Minute
		m.mu.Unlock()
		select {
		case <-time.After(iv):
			m.tick()
		case <-m.reload:
			// interval changed; recompute and wait again
		}
	}
}

// tick: stop expired VMs, then (if storage is mounted) start one fresh VM up to
// the concurrency limit.
func (m *Manager) tick() {
	m.checkStorage()
	m.reconcile()

	m.mu.Lock()
	if m.cfg.Paused {
		m.mu.Unlock()
		return
	}
	mounted := m.storageMounted
	maxc := m.cfg.MaxConcurrent
	excludeOCI := m.cfg.ExcludeOCIFromScheduler
	now := time.Now()
	within := !m.cfg.DailyEnabled || inDailyWindow(now, m.cfg.DailyStart, m.cfg.DailyStop)
	excluded := make(map[string]bool, len(m.cfg.Excluded))
	for _, e := range m.cfg.Excluded {
		if e = strings.TrimSpace(e); e != "" {
			excluded[e] = true
		}
	}

	var toStop []string
	running := 0
	for _, vm := range m.vms {
		if vm.State == "running" {
			running++
			// Outside the daily active hours, stop everything; inside, only stop
			// VMs whose run window has expired.
			if !within || (!vm.StopAt.IsZero() && now.After(vm.StopAt)) {
				toStop = append(toStop, vm.Name)
			}
		}
	}

	// Split candidates so a VM that just failed to boot is retried only when no
	// fresh VM is available — i.e. the next tick prefers "another one".
	var fresh, failed []string
	if within && mounted && running-len(toStop) < maxc {
		for name, vm := range m.vms {
			if !eligibleForScheduler(vm, excludeOCI, excluded, m.busy[name]) {
				continue
			}
			if vm.BootFailed {
				failed = append(failed, name)
			} else {
				fresh = append(fresh, name)
			}
		}
	}
	mode := m.cfg.SchedulerMode
	lastSeq := m.lastSequential
	m.mu.Unlock()

	for _, n := range toStop {
		m.doStop(n)
	}
	candidates := fresh
	if len(candidates) == 0 {
		candidates = failed // everything else failed too; give them another go
	}
	if len(candidates) > 0 {
		var pick string
		if mode == "sequential" {
			// Walk the eligible VMs in alphabetical order, advancing past the
			// last VM that ran so we cycle through the whole list. (doRun updates
			// the cursor for both manual and scheduled starts.)
			sort.Strings(candidates)
			pick = nextSequential(candidates, lastSeq)
		} else {
			pick = candidates[rand.Intn(len(candidates))]
		}
		m.doRun(pick, "scheduler")
	}
}

// nextSequential returns the first name in the sorted list that comes after
// last (alphabetically), wrapping to the first when there is none.
func nextSequential(sorted []string, last string) string {
	for _, n := range sorted {
		if n > last {
			return n
		}
	}
	return sorted[0]
}

// ---------------------------------------------------------------------------
// SSH exec ("Send command" / "Get info")
// ---------------------------------------------------------------------------

type execResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exitCode"`
	Error    string `json:"error,omitempty"`
}

// sshOutcome distills an execResult into (ok, displayable text) for the SSH
// bubble and the Info column.
func sshOutcome(res execResult) (bool, string) {
	ok := res.Error == "" && res.ExitCode == 0
	info := strings.TrimSpace(res.Stdout)
	if info == "" {
		if res.Error != "" {
			info = res.Error
		} else {
			info = strings.TrimSpace(res.Stderr)
		}
	}
	return ok, info
}

// sshExec runs command in the guest, preferring the Tart guest agent and falling
// back to SSH for images without it. The name is retained because it is the
// established entry point for Get info, Send command, and the boot probe; the
// transport is chosen per call by execInGuest. If sudoPassword is non-empty it is
// fed to `sudo -S` in the guest so commands needing sudo work without a TTY; the
// password itself is never logged.
//
// The deadline is deliberately generous and independent of SSHTimeoutSec, which is
// the SSH *connect* timeout: a small connect timeout must not kill a legitimately
// long command such as `softwareupdate`.
func (m *Manager) sshExec(name, command, sudoPassword string) execResult {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	return m.execInGuest(ctx, name, command, sudoPassword)
}

func (m *Manager) sshExecContext(ctx context.Context, name, command, sudoPassword string) execResult {
	m.mu.Lock()
	vm := m.vms[name]
	ip := ""
	if vm != nil {
		ip = vm.IP
	}
	user := m.cfg.SSHUser
	if vm != nil && vm.SSHUser != "" {
		user = vm.SSHUser
	}
	key := m.cfg.SSHKey
	timeout := m.cfg.SSHTimeoutSec
	home := m.cfg.VMStoragePath
	m.mu.Unlock()

	if ip == "" {
		// Resolve on demand if we don't have a cached IP, using the same
		// multi-tier resolver the boot path uses.
		resolveCtx, cancelResolve := context.WithTimeout(ctx, 20*time.Second)
		resolved, err := m.resolveVMIPRobust(resolveCtx, home, name, 10)
		cancelResolve()
		if err != nil {
			return execResult{Error: "could not resolve IP: " + err.Error()}
		}
		ip = strings.TrimSpace(resolved)
	}
	if ip == "" {
		return execResult{Error: "no IP for VM"}
	}
	if user == "" {
		user = "admin"
	}

	args := []string{
		"-o", "BatchMode=yes", // non-interactive: key auth only, never prompt
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null", // VMs change IPs; don't record/verify host keys
		"-o", "LogLevel=ERROR", // suppress the harmless "Permanently added ..." notice
		"-o", fmt.Sprintf("ConnectTimeout=%d", timeout),
		// Detect a dead connection (e.g. the guest powering off mid-`shutdown`)
		// and disconnect in ~15s instead of hanging forever — this keeps a stop
		// from wedging the scheduler goroutine. Live hosts keep responding to
		// keepalives, so legitimately long commands are unaffected.
		"-o", "ServerAliveInterval=5",
		"-o", "ServerAliveCountMax=3",
	}
	if key != "" {
		args = append(args, "-i", key)
	}

	// With a sudo password, make every `sudo` in the command read its password
	// from stdin (-S) instead of a TTY, and feed it on stdin. We inject -S
	// directly rather than priming `sudo -v`, because sudo's credential cache
	// isn't shared between separate sudo calls in a non-interactive SSH session.
	// (The password is only ever on stdin — never in the command or the logs.)
	remoteCmd := command
	if sudoPassword != "" {
		remoteCmd = rewriteSudoForStdin(command)
		m.logln("ssh sudo exec on %s: %s", name, remoteCmd)
	}
	args = append(args, "--", fmt.Sprintf("%s@%s", user, ip), remoteCmd)

	cmd := exec.CommandContext(ctx, "ssh", args...)
	if sudoPassword != "" {
		cmd.Stdin = strings.NewReader(sudoPassword + "\n")
	}
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	res := execResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.ExitCode = ee.ExitCode()
		} else {
			res.Error = err.Error()
		}
	}
	return res
}

// ---------------------------------------------------------------------------
// VM management (create / clone / set / rename / delete)
// ---------------------------------------------------------------------------

// shortID returns an 8-hex-char suffix, matching the style of the existing VM
// names (e.g. macOS-Overview-0056405C).
func shortID() string { return fmt.Sprintf("%08X", rand.Uint32()) }

// lastLine returns the last non-empty line of s (for compact error messages).
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return ""
}

// prefixIfNotEmpty returns prefix+s when s is non-empty, else "".
func prefixIfNotEmpty(prefix, s string) string {
	if s == "" {
		return ""
	}
	return prefix + s
}

// newTask registers a management task and trims the list to the last 20. Its
// ctx/cancel let a running create/clone be killed on demand from the UI.
func (m *Manager) newTask(kind, target string) *Task {
	ctx, cancel := context.WithCancel(context.Background())
	t := &Task{
		ID:        fmt.Sprintf("%d", time.Now().UnixNano()),
		Kind:      kind,
		Target:    target,
		Status:    "running",
		StartedAt: time.Now(),
		ctx:       ctx,
		cancel:    cancel,
	}
	m.mu.Lock()
	m.tasks = append(m.tasks, t)
	if len(m.tasks) > 20 {
		m.tasks = m.tasks[len(m.tasks)-20:]
	}
	m.mu.Unlock()
	return t
}

// cancelTask finds a running task by ID and cancels its in-flight command.
// The command's own exit path (runInto/cmdInto returning a context error)
// marks the task cancelled/finished — this just triggers that.
func (m *Manager) cancelTask(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.tasks {
		if t.ID == id {
			if t.Status != "running" || t.cancel == nil {
				return false
			}
			t.cancel()
			return true
		}
	}
	return false
}

// appendTaskOutput appends to a task's output (capped) and broadcasts at most
// once per second so a chatty download doesn't flood SSE subscribers.
func (m *Manager) appendTaskOutput(t *Task, s string) {
	s = stripANSI(s)
	m.mu.Lock()
	t.Output += s
	if len(t.Output) > 8192 {
		t.Output = t.Output[len(t.Output)-8192:]
	}
	do := time.Since(t.lastBcast) > time.Second
	if do {
		t.lastBcast = time.Now()
	}
	m.mu.Unlock()
	if do {
		m.broadcast()
	}
}

func (m *Manager) finishTask(t *Task, err error) {
	m.mu.Lock()
	t.FinishedAt = time.Now()
	switch {
	case err != nil && t.ctx.Err() == context.Canceled:
		t.Status = "cancelled"
		t.Error = "cancelled by user"
	case err != nil:
		t.Status = "error"
		t.Error = err.Error()
	default:
		t.Status = "success"
	}
	// Release the context's resources now that we're done with it, and clear
	// the func so further cancelTask calls on this ID become no-ops.
	if t.cancel != nil {
		t.cancel()
		t.cancel = nil
	}
	m.mu.Unlock()
}

// taskWriter funnels a command's stdout/stderr into a task's output.
type taskWriter struct {
	m *Manager
	t *Task
}

func (w *taskWriter) Write(p []byte) (int, error) {
	w.m.appendTaskOutput(w.t, string(p))
	return len(p), nil
}

// runInto runs a tart command, streaming output into the task. Bound to the
// task's context, so cancelTask kills it (and any child process) outright.
func (m *Manager) runInto(t *Task, args ...string) error {
	m.appendTaskOutput(t, "$ tart "+strings.Join(args, " ")+"\n")
	m.logln("$ tart %s", strings.Join(args, " "))
	cmd := m.tartCmdCtx(t.ctx, m.storage(), args...)
	w := &taskWriter{m, t}
	cmd.Stdout = w
	cmd.Stderr = w
	err := cmd.Run()
	if err != nil {
		m.logln("tart %s → %v", strings.Join(args, " "), err)
	}
	return err
}

// cmdInto runs an arbitrary command, streaming output into the task. Bound to
// the task's context, so cancelTask kills it outright.
func (m *Manager) cmdInto(t *Task, name string, args ...string) error {
	m.appendTaskOutput(t, "$ "+name+" "+strings.Join(args, " ")+"\n")
	m.logln("$ %s %s", name, strings.Join(args, " "))
	cmd := exec.CommandContext(t.ctx, name, args...)
	w := &taskWriter{m, t}
	cmd.Stdout = w
	cmd.Stderr = w
	err := cmd.Run()
	if err != nil {
		m.logln("%s → %v", name, err)
	}
	return err
}

// installTart downloads the latest tart release from GitHub (the manual install
// documented at tart.run) and places tart.app under the configured Tart app
// path (default /Applications). Used for both first install and "Update Tart".
func (m *Manager) installTart() {
	t := m.newTask("install", "tart")
	m.broadcast()

	m.mu.Lock()
	binPath := m.cfg.TartAppPath
	m.mu.Unlock()
	if strings.TrimSpace(binPath) == "" {
		binPath = defaultTartBin
	}
	dest := appBundleFromBin(binPath) // e.g. /Applications/tart.app

	tmp, err := os.MkdirTemp("", "tart-install")
	if err != nil {
		m.finishTask(t, err)
		m.broadcast()
		return
	}
	defer os.RemoveAll(tmp)

	archive := filepath.Join(tmp, "tart.tar.gz")
	steps := [][]string{
		{"curl", "-fsSL", "-o", archive, "https://github.com/openai/tart/releases/latest/download/tart.tar.gz"},
		{"tar", "-xzf", archive, "-C", tmp},
		{"rm", "-rf", dest},
		{"cp", "-R", filepath.Join(tmp, "tart.app"), dest},
	}
	for _, s := range steps {
		if err = m.cmdInto(t, s[0], s[1:]...); err != nil {
			break
		}
	}
	if err == nil {
		m.detectTartJSON() // re-probe now that tart exists / changed
		m.detectProvisioningSupport()
		m.updateTartVersion()
		m.mu.Lock()
		ver := m.tartVersion
		m.mu.Unlock()
		m.appendTaskOutput(t, "\nTart installed at "+dest+" ("+ver+")\n")
		m.reconcile()
	}
	m.finishTask(t, err)
	m.broadcast()
}

// createReq is the body of POST /api/vm/create.
type createReq struct {
	Mode         string `json:"mode"` // "ipsw" | "clone"
	Source       string `json:"source"`
	FromIpsw     string `json:"fromIpsw"`
	Linux        bool   `json:"linux"`
	Prefix       string `json:"prefix"`
	Count        int    `json:"count"`
	CPU          int    `json:"cpu"`
	Memory       int    `json:"memory"`
	DiskSize     int    `json:"diskSize"`
	Display      string `json:"display"`
	RandomMac    bool   `json:"randomMac"`
	RandomSerial bool   `json:"randomSerial"`
	AutoEnroll   bool   `json:"autoEnroll"` // clone-only: flag the VM so its next boot runs the MDM auto-enroll script

	// Guest provisioning (macOS 27+, IPSW creates only — see createVMs).
	ProvisioningEnabled     bool   `json:"provisioningEnabled"`
	ProvisioningFullName    string `json:"provisioningFullName"`
	ProvisioningUsername    string `json:"provisioningUsername"`
	ProvisioningPassword    string `json:"provisioningPassword"`
	ProvisioningAutoLogin   bool   `json:"provisioningAutoLogin"`
	ProvisioningRemoteLogin bool   `json:"provisioningRemoteLogin"`
}

// buildSetArgs assembles `tart set` args, omitting anything not provided.
func buildSetArgs(name string, cpu, memory, diskSize int, display string, randMac, randSerial bool) []string {
	args := []string{"set", name}
	if cpu > 0 {
		args = append(args, "--cpu", fmt.Sprintf("%d", cpu))
	}
	if memory > 0 {
		args = append(args, "--memory", fmt.Sprintf("%d", memory))
	}
	if diskSize > 0 {
		args = append(args, "--disk-size", fmt.Sprintf("%d", diskSize))
	}
	if display != "" {
		args = append(args, "--display", display)
	}
	if randMac {
		args = append(args, "--random-mac")
	}
	if randSerial {
		args = append(args, "--random-serial")
	}
	return args
}

// createVMs runs the (possibly multiple) create/clone operations sequentially,
// each as its own task, then applies the requested settings via `tart set`.
func (m *Manager) createVMs(req createReq) {
	for i := 0; i < req.Count; i++ {
		name := req.Prefix + shortID()
		t := m.newTask(req.Mode, name)
		m.broadcast()

		var createArgs []string
		var setDisk int // disk-size applied via `set` (clone only; ipsw sets it at create)
		switch req.Mode {
		case "clone":
			createArgs = []string{"clone", req.Source, name}
			setDisk = req.DiskSize // grow the cloned disk if a larger size is asked
		default: // ipsw
			if req.Linux {
				createArgs = []string{"create", name, "--linux"}
			} else {
				createArgs = []string{"create", name, "--from-ipsw", req.FromIpsw}
			}
			if req.DiskSize > 0 {
				createArgs = append(createArgs, "--disk-size", fmt.Sprintf("%d", req.DiskSize))
			}
		}

		err := m.runInto(t, createArgs...)
		if err == nil {
			setArgs := buildSetArgs(name, req.CPU, req.Memory, setDisk, req.Display, req.RandomMac, req.RandomSerial)
			if len(setArgs) > 2 { // more than just "set <name>"
				err = m.runInto(t, setArgs...)
			}
		}
		if err == nil && req.AutoEnroll {
			m.mu.Lock()
			vm := m.vms[name]
			if vm == nil {
				vm = &VM{Name: name}
				m.vms[name] = vm
			}
			vm.AutoEnroll = true
			m.save()
			m.mu.Unlock()
		}
		// Guest provisioning only applies to a fresh --from-ipsw create: it's
		// consumed on the guest's first boot, and a clone's guest has already
		// had its first boot. Linux guests don't support it either.
		if err == nil && req.Mode != "clone" && !req.Linux && req.ProvisioningEnabled {
			m.mu.Lock()
			if m.supportsProvisioning {
				vm := m.vms[name]
				if vm == nil {
					vm = &VM{Name: name}
					m.vms[name] = vm
				}
				vm.PendingProvisioning = renderProvisioningOpts(
					req.ProvisioningFullName, req.ProvisioningUsername, req.ProvisioningPassword,
					req.ProvisioningAutoLogin, req.ProvisioningRemoteLogin)
				m.save()
			}
			m.mu.Unlock()
		}
		m.finishTask(t, err)
		if releasable, released := maybeReleaseGoMemory(runtimeGoMemory{}); released {
			m.logln("released idle Go heap after %s task (%.0f MiB eligible)", req.Mode, float64(releasable)/(1<<20))
		}
		m.reconcile()
		m.broadcast()
		if m.cfg.JamfRecon {
			go runJamf()
		}
	}
}

// isActive reports whether a VM is running or mid-start (can't be edited/deleted).
func (m *Manager) isActive(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	vm := m.vms[name]
	return vm != nil && (vm.State == "running" || vm.State == "starting" || m.busy[name])
}

// ---------------------------------------------------------------------------
// Server self-control (restart / stop the tart-oven process itself)
// ---------------------------------------------------------------------------

// restartServer re-execs this binary in place (works whether launched manually
// or by launchd; same PID, so KeepAlive is unaffected). Falls back to exiting
// (KeepAlive then respawns) if re-exec isn't possible.
func (m *Manager) restartServer() {
	m.logln("server restart requested")
	time.Sleep(300 * time.Millisecond) // let the HTTP response flush
	if exe, err := os.Executable(); err == nil {
		if err := syscall.Exec(exe, os.Args, os.Environ()); err != nil {
			m.logln("re-exec failed: %v; exiting instead", err)
		}
	}
	os.Exit(0)
}

// stopServer stops the process for good. If we're a launchd agent, bootout so
// KeepAlive doesn't respawn us; otherwise a plain exit is enough.
func (m *Manager) stopServer() {
	m.logln("server stop requested")
	time.Sleep(300 * time.Millisecond) // let the HTTP response flush
	uid := os.Getuid()
	for _, label := range []string{"com.tartoven.agent", "com.user.tart-oven"} {
		exec.Command("launchctl", "bootout", fmt.Sprintf("gui/%d/%s", uid, label)).Run()
	}
	os.Exit(0)
}

const launchAgentLabel = "com.tartoven.agent"
const launchAgentPlist = "/Library/LaunchAgents/com.tartoven.agent.plist"

func (m *Manager) handleLaunchAgent(w http.ResponseWriter, r *http.Request) {
	uid := os.Getuid()
	domain := fmt.Sprintf("gui/%d", uid)

	if r.Method == http.MethodGet {
		installed := false
		if _, err := os.Stat(launchAgentPlist); err == nil {
			installed = true
		}
		enabled := m.launchAgentEnabled(domain)
		writeJSON(w, map[string]any{"installed": installed, "enabled": enabled})
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	action := "disable"
	if req.Enabled {
		action = "enable"
	}
	target := fmt.Sprintf("%s/%s", domain, launchAgentLabel)
	out, err := exec.Command("launchctl", action, target).CombinedOutput()
	if err != nil {
		m.logln("launchctl %s %s failed: %v: %s", action, target, err, string(out))
		http.Error(w, fmt.Sprintf("launchctl %s failed: %v", action, err), http.StatusInternalServerError)
		return
	}
	m.logln("launchagent %s: %s", action, target)
	writeJSON(w, map[string]bool{"ok": true})
}

func (m *Manager) launchAgentEnabled(domain string) bool {
	out, err := exec.Command("launchctl", "print-disabled", domain).CombinedOutput()
	if err != nil {
		return true // assume enabled if we can't check
	}
	// Output contains lines like: "com.tartoven.agent" => disabled
	// If not listed or listed without "disabled", it's enabled.
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, launchAgentLabel) && strings.Contains(line, "disabled") {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// SSE / broadcast
// ---------------------------------------------------------------------------

func (m *Manager) snapshot() stateSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()

	excluded := make(map[string]bool, len(m.cfg.Excluded))
	for _, e := range m.cfg.Excluded {
		excluded[strings.TrimSpace(e)] = true
	}

	vms := make([]*VM, 0, len(m.vms))
	for _, vm := range m.vms {
		// Make a copy with computed UI flags so we don't mutate stored state.
		v := *vm
		v.Template = strings.Contains(v.Name, templateMarker)
		v.Excluded = excluded[v.Name]
		v.Busy = m.busy[v.Name]
		v.SSHPassword = "" // write-only from clients; never echo the stored value
		vms = append(vms, &v)
	}
	sort.Slice(vms, func(i, j int) bool { return vms[i].Name < vms[j].Name })

	// Deep-copy tasks and logs: snapshotJSON marshals the returned snapshot
	// after m.mu is released, while appendTaskOutput/finishTask mutate live
	// task fields under the lock. Sharing the pointers would be a data race.
	tasks := make([]*Task, len(m.tasks))
	for i, t := range m.tasks {
		tc := *t
		tasks[i] = &tc
	}
	logs := append([]string(nil), m.logs...)

	var latestSample PerformanceSample
	if n := len(m.performanceHistory); n > 0 {
		latestSample = m.performanceHistory[n-1]
	}

	return stateSnapshot{
		VMs:                  vms,
		Config:               newConfigView(m.cfg),
		StorageMounted:       m.storageMounted,
		StoragePath:          m.cfg.VMStoragePath,
		WithinHours:          !m.cfg.DailyEnabled || inDailyWindow(time.Now(), m.cfg.DailyStart, m.cfg.DailyStop),
		Now:                  time.Now(),
		Version:              version,
		TartJSON:             m.tartJSON,
		TartInstalled:        tartInstalledAt(m.cfg.TartAppPath),
		TartVersion:          m.tartVersion,
		SupportsProvisioning: m.supportsProvisioning,
		Tasks:                tasks,
		Performance:          latestSample,
		HostIP:               m.hostIP,
		Logs:                 logs,
	}
}

func (m *Manager) snapshotJSON() []byte {
	data, _ := json.Marshal(m.snapshot())
	return data
}

func (m *Manager) broadcast() {
	data := m.snapshotJSON()
	m.mu.Lock()
	for ch := range m.subs {
		select {
		case ch <- data:
		default: // slow consumer; drop this update for them
		}
	}
	m.mu.Unlock()
}

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// decodeName pulls {"name": "..."} from a POST body.
func decodeName(r *http.Request) (string, error) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return "", err
	}
	if strings.TrimSpace(body.Name) == "" {
		return "", errors.New("missing name")
	}
	return body.Name, nil
}

type mdmIPResolver func(context.Context, string, string) (string, error)

type mdmProfileResponse struct {
	OK          bool     `json:"ok"`
	Name        string   `json:"name,omitempty"`
	Path        string   `json:"path,omitempty"`
	PayloadUUID string   `json:"payloadUUID,omitempty"`
	Stage       mdmStage `json:"stage,omitempty"`
	Error       string   `json:"error,omitempty"`
}

func writeMDMProfileError(w http.ResponseWriter, status int, name string, stage mdmStage, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	writeJSON(w, mdmProfileResponse{Name: name, Stage: stage, Error: message})
}

func safeMDMStageError(stage mdmStage) (int, string) {
	switch stage {
	case mdmStageConfiguration:
		return http.StatusBadRequest, "profile configuration is incomplete"
	case mdmStageVM:
		return http.StatusBadRequest, "VM is not available"
	case mdmStageIP:
		return http.StatusBadGateway, "could not resolve VM IP"
	case mdmStageAuthentication:
		return http.StatusBadGateway, "SSH authentication failed"
	case mdmStageSFTP:
		return http.StatusBadGateway, "SFTP upload failed"
	case mdmStageVerification:
		return http.StatusBadGateway, "uploaded profile verification failed"
	default:
		return http.StatusBadGateway, "profile transfer failed"
	}
}

type mdmProfileRequest struct {
	Name        string `json:"name"`
	ProfileID   string `json:"profileId,omitempty"`
	SSHUser     string `json:"sshUser,omitempty"`
	SSHPassword string `json:"sshPassword,omitempty"`
}

func (m *Manager) handleMDMProfile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeMDMProfileError(w, http.StatusMethodNotAllowed, "", "", "method not allowed")
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeMDMProfileError(w, http.StatusBadRequest, "", "", "invalid request")
		return
	}
	var in mdmProfileRequest
	if err := json.Unmarshal(body, &in); err != nil {
		writeMDMProfileError(w, http.StatusBadRequest, "", "", "invalid request")
		return
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		writeMDMProfileError(w, http.StatusBadRequest, "", "", "missing name")
		return
	}

	m.mu.Lock()
	cfg := m.cfg
	vm := m.vms[name]
	var vmCopy VM
	if vm != nil {
		vmCopy = *vm
	}
	m.mu.Unlock()

	// Select target Jamf profile:
	// 1. By profileId if provided (a non-empty id that matches nothing is an
	//    error, not a silent fall-through to a different server)
	// 2. First configured profile in cfg.JamfProfiles if available
	// 3. Fallback to legacy cfg.JamfBaseURL and cfg.JamfInvitationCode
	var targetProfile JamfProfile
	profileFound := false
	if in.ProfileID != "" {
		for _, p := range cfg.JamfProfiles {
			if p.ID == in.ProfileID {
				targetProfile = p
				profileFound = true
				break
			}
		}
		if !profileFound {
			writeMDMProfileError(w, http.StatusBadRequest, name, mdmStageConfiguration,
				"selected Jamf server profile no longer exists")
			return
		}
	}
	if !profileFound && len(cfg.JamfProfiles) > 0 {
		targetProfile = cfg.JamfProfiles[0]
		profileFound = true
	}
	if !profileFound {
		targetProfile = JamfProfile{
			Name:           "Default Server",
			BaseURL:        cfg.JamfBaseURL,
			InvitationCode: cfg.JamfInvitationCode,
		}
	}

	username, password := effectiveSSHCredentials(cfg, &vmCopy)
	if strings.TrimSpace(in.SSHUser) != "" {
		username = strings.TrimSpace(in.SSHUser)
	}
	if in.SSHPassword != "" {
		password = in.SSHPassword
	}
	sshTimeout := cfg.SSHTimeoutSec
	if sshTimeout < 1 {
		sshTimeout = 30
	}
	baseURL, baseURLErr := normalizeJamfBaseURL(targetProfile.BaseURL)
	if baseURLErr != nil || baseURL == "" || strings.TrimSpace(targetProfile.InvitationCode) == "" ||
		strings.TrimSpace(username) == "" || strings.TrimSpace(password) == "" {
		status, message := safeMDMStageError(mdmStageConfiguration)
		writeMDMProfileError(w, status, name, mdmStageConfiguration, message)
		return
	}
	if vm == nil || vmCopy.State != "running" {
		status, message := safeMDMStageError(mdmStageVM)
		writeMDMProfileError(w, status, name, mdmStageVM, message)
		return
	}

	ip := strings.TrimSpace(vmCopy.IP)
	if ip == "" {
		if m.mdmResolveIP == nil {
			log.Printf("MDM profile copy failed for VM %q at stage %s", name, mdmStageIP)
			writeMDMProfileError(w, http.StatusInternalServerError, name, mdmStageIP, "IP resolver unavailable")
			return
		}
		ip, err = m.mdmResolveIP(r.Context(), name, cfg.VMStoragePath)
		if err != nil || strings.TrimSpace(ip) == "" {
			log.Printf("MDM profile copy failed for VM %q at stage %s", name, mdmStageIP)
			status, message := safeMDMStageError(mdmStageIP)
			writeMDMProfileError(w, status, name, mdmStageIP, message)
			return
		}
		ip = strings.TrimSpace(ip)
	}

	profile, payloadUUID, err := generateMDMProfile(mdmProfileInput{
		BaseURL:        baseURL,
		InvitationCode: targetProfile.InvitationCode,
	}, cryptorand.Reader)
	if err != nil {
		log.Printf("MDM profile copy failed for VM %q at stage %s", name, mdmStageConfiguration)
		status, message := safeMDMStageError(mdmStageConfiguration)
		writeMDMProfileError(w, status, name, mdmStageConfiguration, message)
		return
	}
	if m.mdmCopier == nil {
		log.Printf("MDM profile copy failed for VM %q at stage %s", name, mdmStageSFTP)
		writeMDMProfileError(w, http.StatusInternalServerError, name, mdmStageSFTP, "profile copy service unavailable")
		return
	}

	target := mdmTransferTarget{
		Address:  net.JoinHostPort(ip, "22"),
		Username: username,
		Password: password,
		Timeout:  time.Duration(sshTimeout) * time.Second,
	}
	if err := m.mdmCopier.CopyAndVerify(r.Context(), target, profile, payloadUUID); err != nil {
		stage := mdmStageSFTP
		var stageErr *mdmStageError
		if errors.As(err, &stageErr) {
			stage = stageErr.Stage
		}
		log.Printf("MDM profile copy failed for VM %q at stage %s", name, stage)
		status, message := safeMDMStageError(stage)
		writeMDMProfileError(w, status, name, stage, message)
		return
	}

	writeJSON(w, mdmProfileResponse{
		OK:          true,
		Name:        name,
		Path:        mdmProfileDisplayPath,
		PayloadUUID: payloadUUID,
	})
}

// resolveVMIPRobust resolves a guest IP, preferring the Tart guest agent, which
// Tart documents as the only resolver that works reliably in all cases and which
// needs no guest network traffic at all. The host ARP match and Tart's own ARP
// resolver are kept for guests without the agent. waitSec bounds each --wait.
//
// The `dhcp` resolver is deliberately absent: Tart only populates a DHCP lease for
// VMs that are NOT bridged, and Tart Oven always runs bridged, so that tier could
// never succeed.
func (m *Manager) resolveVMIPRobust(ctx context.Context, home, name string, waitSec int) (string, error) {
	wait := strconv.Itoa(waitSec)
	if out, err := m.tartCmdCtx(ctx, home, "ip", name, "--wait", wait, "--resolver", "agent").Output(); err == nil {
		if ip := strings.TrimSpace(string(out)); ip != "" {
			return ip, nil
		}
	}
	if ip, err := resolveVMIP(home, name, hostARPNeighbors); err == nil && strings.TrimSpace(ip) != "" {
		return strings.TrimSpace(ip), nil
	}
	if out, err := m.tartCmdCtx(ctx, home, "ip", name, "--wait", wait, "--resolver", "arp").Output(); err == nil {
		if ip := strings.TrimSpace(string(out)); ip != "" {
			return ip, nil
		}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", ctxErr
	}
	return "", errors.New("could not resolve VM IP")
}

func (m *Manager) resolveMDMIPWithTart(ctx context.Context, name, home string) (string, error) {
	return m.resolveVMIPRobust(ctx, home, name, 10)
}

func (m *Manager) routes() *http.ServeMux {
	mux := http.NewServeMux()

	// Dashboard.
	indexHTML, _ := content.ReadFile("index.html")
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})

	mux.HandleFunc("/api/readme", func(w http.ResponseWriter, r *http.Request) {
		b, err := content.ReadFile("README.md")
		if err != nil {
			http.Error(w, "readme not embedded", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Write(b)
	})

	mux.HandleFunc("/api/changelog", func(w http.ResponseWriter, r *http.Request) {
		b, err := content.ReadFile("CHANGELOG.md")
		if err != nil {
			http.Error(w, "changelog not embedded", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Write(b)
	})

	mux.HandleFunc("/CHANGELOG.md", func(w http.ResponseWriter, r *http.Request) {
		b, err := content.ReadFile("CHANGELOG.md")
		if err != nil {
			http.Error(w, "changelog not embedded", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write(b)
	})

	mux.HandleFunc("/api/vms", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, m.snapshot())
	})
	mux.HandleFunc("/api/performance", m.handlePerformance)

	mux.HandleFunc("/api/refresh", func(w http.ResponseWriter, r *http.Request) {
		go m.forceRefresh()
		writeJSON(w, map[string]bool{"ok": true})
	})

	mux.HandleFunc("/api/run", func(w http.ResponseWriter, r *http.Request) {
		name, err := decodeName(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		go m.doRun(name, "manual")
		writeJSON(w, map[string]bool{"ok": true})
	})

	mux.HandleFunc("/api/stop", func(w http.ResponseWriter, r *http.Request) {
		name, err := decodeName(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		go m.doStop(name)
		writeJSON(w, map[string]bool{"ok": true})
	})

	mux.HandleFunc("/api/restart", func(w http.ResponseWriter, r *http.Request) {
		name, err := decodeName(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		go func() {
			m.doStop(name)
			m.doRun(name, "manual")
		}()
		writeJSON(w, map[string]bool{"ok": true})
	})

	mux.HandleFunc("/api/exec", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name         string `json:"name"`
			Command      string `json:"command"`
			SudoPassword string `json:"sudoPassword"` // transient; never stored or logged
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if body.Name == "" || body.Command == "" {
			http.Error(w, "name and command required", http.StatusBadRequest)
			return
		}
		writeJSON(w, m.sshExec(body.Name, body.Command, body.SudoPassword))
	})

	mux.HandleFunc("/api/info", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("name")
		if name == "" {
			http.Error(w, "name required", http.StatusBadRequest)
			return
		}
		m.mu.Lock()
		cmd := m.cfg.StatusCommand
		m.mu.Unlock()
		res := m.sshExec(name, cmd, "")
		// Reflect the result in the SSH bubble and the Info column.
		ok, info := sshOutcome(res)
		m.mu.Lock()
		if vm := m.vms[name]; vm != nil {
			vm.SSHOK = ok
			vm.SSHCheckedAt = time.Now()
			vm.Info = info
			vm.InfoAt = time.Now()
		}
		m.mu.Unlock()
		m.refreshMDMStatus(name)
		m.broadcast()
		writeJSON(w, res)
	})

	mux.HandleFunc("/api/history", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		// Return newest-first; copy so we don't hand out internal pointers.
		events := make([]RunEvent, 0, len(m.history))
		for i := len(m.history) - 1; i >= 0; i-- {
			events = append(events, *m.history[i])
		}
		days := m.cfg.HistoryDays
		m.mu.Unlock()
		writeJSON(w, map[string]any{"events": events, "retentionDays": days})
	})

	// ----- VM management -----
	mux.HandleFunc("/api/vm/create", func(w http.ResponseWriter, r *http.Request) {
		var req createReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.Count < 1 {
			req.Count = 1
		}
		if req.Count > 20 {
			req.Count = 20
		}
		if strings.TrimSpace(req.Prefix) == "" {
			req.Prefix = "macOS-Overview-"
		}
		if req.Mode == "clone" {
			if strings.TrimSpace(req.Source) == "" {
				http.Error(w, "clone requires a source VM", http.StatusBadRequest)
				return
			}
		} else {
			req.Mode = "ipsw"
			if !req.Linux && strings.TrimSpace(req.FromIpsw) == "" {
				req.FromIpsw = "latest"
			}
		}
		go m.createVMs(req)
		writeJSON(w, map[string]bool{"ok": true})
	})

	mux.HandleFunc("/api/oci/pull", m.handleOCIPull)

	// Cancels a running Activity task (create/clone/install) by killing its
	// in-flight command. No-op if the task already finished or doesn't exist.
	mux.HandleFunc("/api/task/cancel", func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if b.ID == "" {
			http.Error(w, "id required", http.StatusBadRequest)
			return
		}
		if !m.cancelTask(b.ID) {
			writeJSON(w, map[string]string{"error": "task not running or not found"})
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})

	mux.HandleFunc("/api/vm/set", func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Name         string `json:"name"`
			CPU          int    `json:"cpu"`
			Memory       int    `json:"memory"`
			DiskSize     int    `json:"diskSize"`
			Display      string `json:"display"`
			RandomMac    bool   `json:"randomMac"`
			RandomSerial bool   `json:"randomSerial"`
		}
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if b.Name == "" {
			http.Error(w, "name required", http.StatusBadRequest)
			return
		}
		if m.isActive(b.Name) {
			writeJSON(w, map[string]string{"error": "stop the VM before editing it"})
			return
		}
		args := buildSetArgs(b.Name, b.CPU, b.Memory, b.DiskSize, b.Display, b.RandomMac, b.RandomSerial)
		if len(args) <= 2 {
			writeJSON(w, map[string]string{"error": "no changes specified"})
			return
		}
		out, err := m.tartCmd(m.storage(), args...).CombinedOutput()
		m.reconcile()
		m.broadcast()
		res := map[string]string{"output": string(out)}
		if err != nil {
			res["error"] = err.Error() + ": " + string(out)
		}
		writeJSON(w, res)
	})

	mux.HandleFunc("/api/vm/rename", func(w http.ResponseWriter, r *http.Request) {
		var b struct{ Name, NewName string }
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if b.Name == "" || b.NewName == "" {
			http.Error(w, "name and newName required", http.StatusBadRequest)
			return
		}
		if m.isActive(b.Name) {
			writeJSON(w, map[string]string{"error": "stop the VM before renaming it"})
			return
		}
		out, err := m.tartCmd(m.storage(), "rename", b.Name, b.NewName).CombinedOutput()
		if err == nil {
			// Carry the VM's server-side state (notes, tags, SSH credentials —
			// the SSH password is write-only and can't be re-sent by the client)
			// to the new name before reconcile rebuilds the map from `tart list`,
			// which would otherwise drop the old entry and lose those fields.
			m.mu.Lock()
			if old := m.vms[b.Name]; old != nil {
				moved := *old
				moved.Name = b.NewName
				m.vms[b.NewName] = &moved
				delete(m.vms, b.Name)
			}
			m.save()
			m.mu.Unlock()
		}
		m.reconcile()
		m.broadcast()
		res := map[string]string{"output": string(out)}
		if err != nil {
			res["error"] = err.Error() + ": " + string(out)
		}
		writeJSON(w, res)
	})

	mux.HandleFunc("/api/vm/delete", func(w http.ResponseWriter, r *http.Request) {
		name, err := decodeName(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if m.isActive(name) {
			writeJSON(w, map[string]string{"error": "stop the VM before deleting it"})
			return
		}
		out, derr := m.tartCmd(m.storage(), "delete", name).CombinedOutput()
		m.reconcile()
		m.broadcast()
		res := map[string]string{"output": string(out)}
		if derr != nil {
			res["error"] = derr.Error() + ": " + string(out)
		}
		writeJSON(w, res)
	})

	mux.HandleFunc("/api/vm/get", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("name")
		if name == "" {
			http.Error(w, "name required", http.StatusBadRequest)
			return
		}
		out, err := m.tartOutput(m.storage(), "get", name, "--format", "json")
		if err != nil {
			writeJSON(w, map[string]string{"error": err.Error() + ": " + out})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(out)) // pass tart's JSON straight through
	})

	mux.HandleFunc("/api/vm/notes", func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Name        string   `json:"name"`
			Notes       string   `json:"notes"`
			Tags        []string `json:"tags"`
			SSHUser     string   `json:"sshUser"`
			SSHPassword string   `json:"sshPassword"`
		}
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if b.Name == "" {
			http.Error(w, "name required", http.StatusBadRequest)
			return
		}
		// Filter empty tags
		var tags []string
		for _, t := range b.Tags {
			if t = strings.TrimSpace(t); t != "" {
				tags = append(tags, t)
			}
		}
		m.mu.Lock()
		vm := m.vms[b.Name]
		if vm == nil {
			m.mu.Unlock()
			writeJSON(w, map[string]string{"error": "VM not found"})
			return
		}
		vm.Notes = b.Notes
		vm.Tags = tags
		// Omitted by the dashboard's notes-only editor, so a blank submission means
		// "leave it unchanged" — matching the password below. Assigning it
		// unconditionally silently wiped a VM's custom user when saving a note.
		if user := strings.TrimSpace(b.SSHUser); user != "" {
			vm.SSHUser = user
		}
		// Write-only: the client never receives the stored password back, so a
		// blank submission means "leave it unchanged", not "clear it".
		if pw := strings.TrimSpace(b.SSHPassword); pw != "" {
			vm.SSHPassword = pw
		}
		m.save()
		m.mu.Unlock()
		m.broadcast()
		writeJSON(w, map[string]bool{"ok": true})
	})

	// Clears a VM's boot-failure flag once the underlying issue (network,
	// storage, config) has been fixed, so it competes normally in the
	// scheduler again instead of being deprioritized indefinitely.
	mux.HandleFunc("/api/vm/clear-boot-failure", func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if b.Name == "" {
			http.Error(w, "name required", http.StatusBadRequest)
			return
		}
		m.mu.Lock()
		vm := m.vms[b.Name]
		if vm == nil {
			m.mu.Unlock()
			writeJSON(w, map[string]string{"error": "VM not found"})
			return
		}
		vm.BootFailed = false
		m.save()
		m.mu.Unlock()
		m.broadcast()
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("/api/vm/install-agent", func(w http.ResponseWriter, r *http.Request) {
		name, err := decodeName(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		go m.installGuestAgent(name)
		writeJSON(w, map[string]bool{"ok": true})
	})

	mux.HandleFunc("/api/vm/mdm-profile", m.handleMDMProfile)

	// Runs the VM if stopped (or reuses it if already running), pushes a fresh
	// enrollment profile, and drives the guest through Install/Enroll — the
	// same flow "Auto enroll at boot" triggers automatically, but on demand.
	mux.HandleFunc("/api/vm/auto-enroll", func(w http.ResponseWriter, r *http.Request) {
		name, err := decodeName(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		go m.runAutoEnrollTask(name, "manual")
		writeJSON(w, map[string]bool{"ok": true})
	})

	// One-time-per-base-VM: primes the Automation + Accessibility grants
	// auto-enroll's UI scripting depends on. See "Enable Auto-Enrollment
	// Capabilities on Base VM" in VM Management.
	mux.HandleFunc("/api/vm/prep-golden-image", func(w http.ResponseWriter, r *http.Request) {
		name, err := decodeName(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		go m.runPrepGoldenImageTask(name)
		writeJSON(w, map[string]bool{"ok": true})
	})

	// Read-only compatibility snapshot for the Target VM dropdown in "Enable
	// Auto-Enrollment Capabilities on Base VM" — see checkEnrollReadiness.
	mux.HandleFunc("/api/vm/enroll-readiness", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimSpace(r.URL.Query().Get("name"))
		if name == "" {
			http.Error(w, "missing name", http.StatusBadRequest)
			return
		}
		writeJSON(w, m.checkEnrollReadiness(r.Context(), name))
	})

	// Install or update tart (downloads the latest release either way).
	mux.HandleFunc("/api/install-tart", func(w http.ResponseWriter, r *http.Request) {
		go m.installTart()
		writeJSON(w, map[string]bool{"ok": true})
	})

	mux.HandleFunc("/api/server/restart", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]bool{"ok": true})
		go m.restartServer()
	})
	mux.HandleFunc("/api/server/stop", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]bool{"ok": true})
		go m.stopServer()
	})

	mux.HandleFunc("/api/server/launchagent", m.handleLaunchAgent)

	mux.HandleFunc("/api/config", m.handleConfig)
	mux.HandleFunc("/events", m.handleEvents)

	return mux
}

func (m *Manager) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		m.mu.Lock()
		cfg := m.cfg
		m.mu.Unlock()
		writeJSON(w, newConfigView(cfg))
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Validate JamfBaseURL if present
	if raw, ok := fields["jamfBaseUrl"]; ok {
		var urlStr string
		if err := json.Unmarshal(raw, &urlStr); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if urlStr != "" {
			if _, err := normalizeJamfBaseURL(urlStr); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
	}
	if raw, ok := fields["jamfProfiles"]; ok {
		var list []JamfProfile
		if err := json.Unmarshal(raw, &list); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		for _, p := range list {
			if strings.TrimSpace(p.BaseURL) != "" {
				if _, err := normalizeJamfBaseURL(p.BaseURL); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
			}
		}
	}

	m.mu.Lock()
	prevPaused := m.cfg.Paused
	prevStorage := m.cfg.VMStoragePath

	if raw, ok := fields["listen"]; ok {
		var v string
		if json.Unmarshal(raw, &v) == nil && v != "" {
			m.cfg.Listen = v
		}
	}
	if raw, ok := fields["vmStoragePath"]; ok {
		var v string
		if json.Unmarshal(raw, &v) == nil && v != "" {
			m.cfg.VMStoragePath = v
		}
	}
	if raw, ok := fields["sharedDir"]; ok {
		var v string
		if json.Unmarshal(raw, &v) == nil && v != "" {
			m.cfg.SharedDir = v
		}
	}
	if raw, ok := fields["tartAppPath"]; ok {
		var v string
		if json.Unmarshal(raw, &v) == nil && v != "" {
			m.cfg.TartAppPath = v
		}
	}
	if raw, ok := fields["intervalMinutes"]; ok {
		var v int
		if json.Unmarshal(raw, &v) == nil && v >= 1 {
			m.cfg.IntervalMinutes = v
		}
	}
	if raw, ok := fields["windowMinutes"]; ok {
		var v int
		if json.Unmarshal(raw, &v) == nil && v >= 1 {
			m.cfg.WindowMinutes = v
		}
	}
	if raw, ok := fields["maxConcurrent"]; ok {
		var v int
		if json.Unmarshal(raw, &v) == nil && v >= 1 {
			if v > hardMaxConcurrent {
				v = hardMaxConcurrent
			}
			m.cfg.MaxConcurrent = v
		}
	}
	if raw, ok := fields["schedulerMode"]; ok {
		var v string
		if json.Unmarshal(raw, &v) == nil {
			if v == "sequential" {
				m.cfg.SchedulerMode = "sequential"
			} else if v == "random" {
				m.cfg.SchedulerMode = "random"
			}
		}
	}
	if raw, ok := fields["excluded"]; ok {
		var v []string
		if json.Unmarshal(raw, &v) == nil {
			m.cfg.Excluded = v
		}
	}
	if raw, ok := fields["excludeOciFromScheduler"]; ok {
		var v bool
		if json.Unmarshal(raw, &v) == nil {
			m.cfg.ExcludeOCIFromScheduler = v
		}
	}
	if raw, ok := fields["noGraphics"]; ok {
		var v bool
		if json.Unmarshal(raw, &v) == nil {
			m.cfg.NoGraphics = v
		}
	}
	if raw, ok := fields["noAudio"]; ok {
		var v bool
		if json.Unmarshal(raw, &v) == nil {
			m.cfg.NoAudio = v
		}
	}
	if raw, ok := fields["jamfRecon"]; ok {
		var v bool
		if json.Unmarshal(raw, &v) == nil {
			m.cfg.JamfRecon = v
		}
	}
	if raw, ok := fields["paused"]; ok {
		var v bool
		if json.Unmarshal(raw, &v) == nil {
			m.cfg.Paused = v
		}
	}
	if raw, ok := fields["dailyEnabled"]; ok {
		var v bool
		if json.Unmarshal(raw, &v) == nil {
			m.cfg.DailyEnabled = v
		}
	}
	if raw, ok := fields["dailyStart"]; ok {
		var v string
		if json.Unmarshal(raw, &v) == nil && v != "" {
			if _, ok := parseHHMM(v); ok {
				m.cfg.DailyStart = v
			}
		}
	}
	if raw, ok := fields["dailyStop"]; ok {
		var v string
		if json.Unmarshal(raw, &v) == nil && v != "" {
			if _, ok := parseHHMM(v); ok {
				m.cfg.DailyStop = v
			}
		}
	}
	if raw, ok := fields["jamfProfiles"]; ok {
		var list []JamfProfile
		if json.Unmarshal(raw, &list) == nil {
			existingMap := make(map[string]string)
			for _, p := range m.cfg.JamfProfiles {
				existingMap[p.ID] = p.InvitationCode
			}
			// The dashboard represents a legacy single-server config as a
			// synthesized profile with id "default". When that row is saved
			// without re-entering its (write-only) invitation code, inherit the
			// stored legacy code so the profile isn't persisted code-less.
			if m.cfg.JamfInvitationCode != "" {
				if _, ok := existingMap["default"]; !ok {
					existingMap["default"] = m.cfg.JamfInvitationCode
				}
			}
			var cleaned []JamfProfile
			for i, p := range list {
				p.Name = strings.TrimSpace(p.Name)
				if p.Name == "" {
					p.Name = fmt.Sprintf("Jamf Server %d", i+1)
				}
				if p.ID == "" {
					p.ID = fmt.Sprintf("jamf-%d", time.Now().UnixNano()+int64(i))
				}
				if norm, err := normalizeJamfBaseURL(p.BaseURL); err == nil && norm != "" {
					p.BaseURL = norm
				}
				if strings.TrimSpace(p.InvitationCode) == "" {
					p.InvitationCode = existingMap[p.ID]
				} else {
					p.InvitationCode = strings.TrimSpace(p.InvitationCode)
				}
				if p.BaseURL != "" {
					cleaned = append(cleaned, p)
				}
			}
			m.cfg.JamfProfiles = cleaned
		}
	}
	if raw, ok := fields["jamfBaseUrl"]; ok {
		var v string
		if json.Unmarshal(raw, &v) == nil {
			if v == "" {
				m.cfg.JamfBaseURL = ""
			} else if norm, err := normalizeJamfBaseURL(v); err == nil {
				m.cfg.JamfBaseURL = norm
			}
		}
	}
	if raw, ok := fields["jamfInvitationCode"]; ok {
		var v string
		if json.Unmarshal(raw, &v) == nil && strings.TrimSpace(v) != "" {
			m.cfg.JamfInvitationCode = strings.TrimSpace(v)
		}
	}
	if raw, ok := fields["sshUser"]; ok {
		var v string
		if json.Unmarshal(raw, &v) == nil && strings.TrimSpace(v) != "" {
			m.cfg.SSHUser = strings.TrimSpace(v)
		}
	}
	if raw, ok := fields["sshPassword"]; ok {
		var v string
		if json.Unmarshal(raw, &v) == nil && v != "" {
			m.cfg.SSHPassword = v
		}
	}
	if raw, ok := fields["sshFallbackEnabled"]; ok {
		var v bool
		if json.Unmarshal(raw, &v) == nil {
			m.cfg.SSHFallbackEnabled = v
		}
	}
	if raw, ok := fields["prioritizeSshShutdown"]; ok {
		var v bool
		if json.Unmarshal(raw, &v) == nil {
			m.cfg.PrioritizeSSHShutdown = v
		}
	}
	if raw, ok := fields["sshKey"]; ok {
		var v string
		if json.Unmarshal(raw, &v) == nil && validSSHKeyPath(v) {
			m.cfg.SSHKey = strings.TrimSpace(v)
		}
	}
	if raw, ok := fields["sshTimeoutSec"]; ok {
		var v int
		if json.Unmarshal(raw, &v) == nil && v >= 1 {
			m.cfg.SSHTimeoutSec = v
		}
	}
	if raw, ok := fields["statusCommand"]; ok {
		var v string
		if json.Unmarshal(raw, &v) == nil {
			m.cfg.StatusCommand = v
		}
	}
	if raw, ok := fields["runArgs"]; ok {
		var v string
		if json.Unmarshal(raw, &v) == nil {
			m.cfg.RunArgs = v
		}
	}
	if raw, ok := fields["netPriority"]; ok {
		var v string
		if json.Unmarshal(raw, &v) == nil && (v == "wifi" || v == "ethernet" || v == "auto" || v == "shared") {
			m.cfg.NetPriority = v
		}
	}
	if raw, ok := fields["bootTimeoutSec"]; ok {
		var v int
		if json.Unmarshal(raw, &v) == nil && v >= 10 {
			m.cfg.BootTimeoutSec = v
		}
	}
	if raw, ok := fields["historyDays"]; ok {
		var v int
		if json.Unmarshal(raw, &v) == nil && v >= 1 {
			m.cfg.HistoryDays = v
		}
	}
	if raw, ok := fields["logPath"]; ok {
		var v string
		if json.Unmarshal(raw, &v) == nil {
			m.cfg.LogPath = v
		}
	}
	if raw, ok := fields["serverLabel"]; ok {
		var v string
		if json.Unmarshal(raw, &v) == nil {
			m.cfg.ServerLabel = v
		}
	}
	if raw, ok := fields["showRunningOnly"]; ok {
		var v bool
		if json.Unmarshal(raw, &v) == nil {
			m.cfg.ShowRunningOnly = v
		}
	}
	if raw, ok := fields["firstRunCompleted"]; ok {
		var v bool
		if json.Unmarshal(raw, &v) == nil {
			m.cfg.FirstRunCompleted = v
		}
	}
	if raw, ok := fields["operatorRole"]; ok {
		var v string
		if json.Unmarshal(raw, &v) == nil {
			m.cfg.OperatorRole = strings.TrimSpace(v)
		}
	}

	m.pruneHistory() // retention may have shrunk
	nowOn := prevPaused && !m.cfg.Paused
	nowOff := !prevPaused && m.cfg.Paused
	if nowOff {
		// Turning the scheduler OFF stops all auto-stop countdowns: clear StopAt
		// on running VMs so they keep running with no timer.
		for _, vm := range m.vms {
			if vm.State == "running" {
				vm.StopAt = time.Time{}
			}
		}
	}
	if nowOn {
		// Turning it back ON re-arms a fresh run window on anything still running.
		window := time.Duration(m.cfg.WindowMinutes) * time.Minute
		for _, vm := range m.vms {
			if vm.State == "running" && vm.StopAt.IsZero() {
				vm.StopAt = time.Now().Add(window)
			}
		}
	}
	m.save()
	m.mu.Unlock()

	// Storage path may have changed; re-check and wake the scheduler so the new
	// interval takes effect immediately.
	if m.cfg.VMStoragePath != prevStorage {
		m.checkStorage()
	}
	select {
	case m.reload <- struct{}{}:
	default:
	}
	// Turning the scheduler ON should act right away rather than waiting a full
	// interval for the first run.
	if nowOn {
		go m.tick()
	}
	m.broadcast()
	writeJSON(w, map[string]bool{"ok": true})
}

func (m *Manager) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := make(chan []byte, 8)
	m.mu.Lock()
	m.subs[ch] = struct{}{}
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.subs, ch)
		m.mu.Unlock()
	}()

	// Initial state immediately.
	fmt.Fprintf(w, "data: %s\n\n", m.snapshotJSON())
	flusher.Flush()

	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case data := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		case <-heartbeat.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

func main() {
	home, _ := os.UserHomeDir()
	defaultState := filepath.Join(home, ".tart-oven", "state.json")

	listenFlag := flag.String("listen", "", "host:port to bind (overrides config)")
	stateFlag := flag.String("state", defaultState, "path to state.json")
	versionFlag := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *versionFlag {
		fmt.Println(version)
		return
	}

	if err := os.MkdirAll(filepath.Dir(*stateFlag), 0o755); err != nil {
		log.Fatalf("cannot create state dir: %v", err)
	}

	m := &Manager{
		vms:                  map[string]*VM{},
		busy:                 map[string]bool{},
		opStart:              map[string]time.Time{},
		runningCmds:          map[string]*exec.Cmd{},
		agentProbedSinceBoot: map[string]bool{},
		subs:                 map[chan []byte]struct{}{},
		statePath:            *stateFlag,
		reload:               make(chan struct{}, 1),
		performanceCollector: &performanceCollector{source: systemPerformanceSource{}},
	}
	m.mdmCopier = newSFTPProfileCopier()
	m.mdmResolveIP = m.resolveMDMIPWithTart
	m.load()
	if *listenFlag != "" {
		m.cfg.Listen = *listenFlag
	}

	// Set up file logging (after config is loaded so we have LogPath).
	setupLogging(m.cfg.LogPath)

	// Reconcile reality at startup: detect JSON support, check storage, sync.
	m.detectTartJSON()
	m.detectProvisioningSupport()
	m.updateTartVersion()
	m.checkStorage()
	m.ensureSharedDir()
	m.reconcile()
	m.updatePerformance(time.Now())
	m.hostIP = localIP()

	go m.schedulerLoop()

	// Background monitor: keep storage status, VM states and timers fresh even
	// between scheduler ticks. It also heals ops that got stuck "busy" and
	// reconciles against tart, so a VM that tart stopped on its own (or one
	// wedged in "stopping") self-corrects within ~10s.
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for range t.C {
			m.checkStorage()
			m.healStuck(maxOpAge)
			m.reconcile()
			m.broadcast()
		}
	}()

	// Refresh native host performance metrics once a minute.
	go func() {
		t := time.NewTicker(60 * time.Second)
		defer t.Stop()
		for range t.C {
			m.updatePerformance(time.Now())
			m.updateTartVersion()
			m.hostIP = localIP()
			m.broadcast()
		}
	}()

	// The SSH fallback (used only for guests without the Tart guest agent) needs a
	// usable identity. Nothing is ever pushed into a guest automatically.
	if _, err := ensureSSHKeyPair(m.cfg.SSHKey); err != nil {
		log.Printf("ssh identity unavailable, SSH fallback disabled: %v", err)
	}

	log.Printf("tart-oven %s", version)
	log.Printf("host IP: %s", m.hostIP)
	log.Printf("tart binary: %s", m.cfg.TartAppPath)
	log.Printf("tart JSON list support: %v", m.tartJSON)
	log.Printf("state file: %s", m.statePath)
	log.Printf("TART_HOME: %s (mounted=%v)", m.cfg.VMStoragePath, m.storageMounted)
	log.Printf("listening on http://%s", m.cfg.Listen)

	if err := http.ListenAndServe(m.cfg.Listen, sameOriginGuard(m.routes())); err != nil {
		log.Fatalf("http server: %v", err)
	}
}

// sameOriginGuard rejects state-changing requests that carry a cross-origin
// Origin header. Browsers attach Origin to every non-GET cross-origin request
// (simple requests included), so this blocks a malicious page the operator has
// open from driving the local API via forged POSTs (CSRF). Requests with no
// Origin header (curl, same-origin navigations) are left untouched.
func sameOriginGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			// Safe methods: no state change to protect.
		default:
			if origin := r.Header.Get("Origin"); origin != "" {
				if u, err := url.Parse(origin); err != nil || u.Host != r.Host {
					http.Error(w, "cross-origin request forbidden", http.StatusForbidden)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

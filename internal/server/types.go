package server

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"time"

	"tart-oven/internal/ipsw"
	"tart-oven/internal/mdm"
	"tart-oven/internal/perf"
	"tart-oven/internal/update"
)

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
	SSHPassword             string        `json:"sshPassword"`            // guest SSH/sudo password; write-only
	SSHKey                  string        `json:"sshKey"`                 // identity file for the SSH fallback
	SSHFallbackEnabled      bool          `json:"sshFallbackEnabled"`     // allow SSH when a guest has no Tart guest agent
	PrioritizeSSHShutdown   bool          `json:"prioritizeSshShutdown"`  // try a clean SSH shutdown before the fast tart stop
	ShowJamfFeatures        bool          `json:"showJamfFeatures"`       // opt-in: show Jamf-specific UI (base VM prep, jamf shortcuts)
	DisableTartUpdateCheck  bool          `json:"disableTartUpdateCheck"` // opt-out of the daily Tart release check
	DisableOvenUpdateCheck  bool          `json:"disableOvenUpdateCheck"` // opt-out of the daily Tart Oven release check
	SSHTimeoutSec           int           `json:"sshTimeoutSec"`          // ssh connect timeout
	StatusCommand           string        `json:"statusCommand"`          // command for "Get info"
	RunArgs                 string        `json:"runArgs"`                // extra args appended to every `tart run`
	NetPriority             string        `json:"netPriority"`            // "auto" | "wifi" | "ethernet" | "shared"
	BootTimeoutSec          int           `json:"bootTimeoutSec"`         // wait for IP before declaring boot failure
	HistoryDays             int           `json:"historyDays"`            // run-history retention in days
	LogPath                 string        `json:"logPath"`                // path to log file (rotation at 5MB)
	ServerLabel             string        `json:"serverLabel"`            // custom label to identify this server instance
	ShowRunningOnly         bool          `json:"showRunningOnly"`        // dashboard filter: show only running VMs
	FirstRunCompleted       bool          `json:"firstRunCompleted"`      // whether initial setup wizard has completed
	OperatorRole            string        `json:"operatorRole"`           // operator persona preset (e.g. "jamf", "general")
	TLSEnabled              bool          `json:"tlsEnabled"`             // serve HTTPS instead of HTTP (applies on restart)
	TLSCertPath             string        `json:"tlsCertPath"`            // PEM certificate; empty = managed self-signed cert
	TLSKeyPath              string        `json:"tlsKeyPath"`             // PEM private key; empty = managed self-signed cert
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
	Name         string       `json:"name"`
	Source       string       `json:"source,omitempty"`
	Disk         int          `json:"disk,omitempty"`     // virtual disk capacity reported by Tart, in GB
	Size         int          `json:"size,omitempty"`     // cached storage used, in GB
	Accessed     string       `json:"accessed,omitempty"` // RFC3339 in JSON mode; relative text in table fallback
	State        string       `json:"state"`
	IP           string       `json:"ip,omitempty"`
	StartedAt    time.Time    `json:"startedAt,omitempty"`
	StopAt       time.Time    `json:"stopAt,omitempty"`
	LastRun      time.Time    `json:"lastRun,omitempty"`      // last time this VM was started
	Headless     bool         `json:"headless,omitempty"`     // current run is using --no-graphics
	RunOverride  *runOverride `json:"runOverride,omitempty"`  // current run's "Run with arguments" choice, reused by Restart
	BootFailed   bool         `json:"bootFailed,omitempty"`   // started but never got an IP
	SSHOK        bool         `json:"sshOk,omitempty"`        // last SSH connectivity check passed
	SSHCheckedAt time.Time    `json:"sshCheckedAt,omitempty"` // when SSH was last checked

	AgentOK        bool      `json:"agentOk"`                  // guest agent answered the last probe
	AgentCheckedAt time.Time `json:"agentCheckedAt,omitempty"` // zero means never probed
	Info           string    `json:"info,omitempty"`           // last "Get info" (status command) output
	InfoAt         time.Time `json:"infoAt,omitempty"`         // when Info was last fetched

	MDMEnrolled  bool      `json:"mdmEnrolled,omitempty"`  // guest reports an active MDM enrollment
	MDMServer    string    `json:"mdmServer,omitempty"`    // raw MDM check-in URL from the guest
	MDMCheckedAt time.Time `json:"mdmCheckedAt,omitempty"` // zero means never probed, which renders as unknown

	AutoEnroll        bool   `json:"autoEnroll,omitempty"`        // set at clone time; next boot (manual or scheduler) runs the MDM auto-enroll script once
	AutoEnrollProfile string `json:"autoEnrollProfile,omitempty"` // Jamf server profile ID to enroll into; "" = whatever profile is already on the guest's Desktop, else the first one

	Hostname         string `json:"hostname,omitempty"`         // custom guest computer name ("" = not managed unless HostnameFromName)
	HostnameFromName bool   `json:"hostnameFromName,omitempty"` // keep the guest's hostname equal to the VM name
	HostnameApplied  string `json:"hostnameApplied,omitempty"`  // last name successfully set in the guest

	PendingProvisioning string `json:"pendingProvisioning,omitempty"` // set at create time (fresh --from-ipsw only); rendered `key=value,...` consumed by the next `tart run` and cleared once it boots successfully

	Notes       string   `json:"notes,omitempty"`       // user-entered notes for tracking/inventory
	Tags        []string `json:"tags,omitempty"`        // user-defined tags for grouping/filtering
	SSHUser     string   `json:"sshUser,omitempty"`     // custom SSH user (overrides default)
	SSHPassword string   `json:"sshPassword,omitempty"` // custom SSH/sudo password; persisted, but masked before every client-facing response

	LastError string `json:"lastError,omitempty"`

	// Computed for the UI in stateSnapshot (not persisted meaningfully).
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
	// StopUnknown marks a run whose stop was never recorded (the server went
	// away while it ran); StoppedAt is then set to StartedAt only so that no
	// later stop gets attributed to this stale entry.
	StopUnknown bool `json:"stopUnknown,omitempty"`
}

// Task tracks a long-running management operation (create / clone) so the UI
// can show live progress. Kept in memory only (not persisted).
type Task struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`            // create | clone
	Target     string    `json:"target"`          // VM name being produced
	Batch      string    `json:"batch,omitempty"` // create/clone request this task belongs to
	Status     string    `json:"status"`          // running | success | error | cancelled
	Output     string    `json:"output"`          // tail of combined stdout/stderr
	Error      string    `json:"error,omitempty"`
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt,omitempty"`

	lastBcast time.Time          // throttles progress broadcasts; not serialized
	termCarry string             // escape sequence split across two writes (see applyTerminalOutput)
	ctx       context.Context    // bound to the in-flight command(s); cancelling it kills them
	cancel    context.CancelFunc // cancels ctx; nil once the task has finished
}

// createBatch is one /api/vm/create request. Its VMs are built one after
// another, so the batch — not any single task — is what the UI shows as in
// progress and what Cancel stops.
type createBatch struct {
	ID        string `json:"id"`
	Mode      string `json:"mode"` // ipsw | clone
	Total     int    `json:"total"`
	Started   int    `json:"started"` // VMs begun so far, including the current one
	Cancelled bool   `json:"cancelled"`
}

// Manager holds everything, guarded by mu.
type Manager struct {
	batches              map[string]*createBatch // in-flight create batches; lazily initialised
	authOnce             sync.Once
	authSt               *authState // UI access token + sessions; see auth.go
	tlsActive            bool       // the listener is serving HTTPS
	mu                   sync.Mutex
	cfg                  Config
	vms                  map[string]*VM
	history              []*RunEvent // run log, pruned to cfg.HistoryDays
	tasks                []*Task     // recent create/clone operations
	logs                 []string    // rolling tart command log (last ~200 lines)
	performanceCollector *perf.Collector
	performanceHistory   []perf.PerformanceSample
	hostIP               string                // local IP of the host Mac
	tartVersion          string                // `tart --version`, refreshed periodically
	tartLatest           string                // latest tart release tag on GitHub ("" = not checked yet)
	ovenLatest           string                // latest tart-oven release tag on GitHub
	dismissedUpdates     map[string]string     // update banner kind -> latest version dismissed this run
	releaseFetcher       update.ReleaseFetcher // nil = fetchLatestRelease; overridden in tests
	lastSequential       string                // last VM started in sequential scheduler mode
	busy                 map[string]bool       // VMs with an op in flight (start/stop)
	opStart              map[string]time.Time  // when each busy op started (to detect stuck ops)
	runningCmds          map[string]*exec.Cmd  // live `tart run` processes
	agentProbedSinceBoot map[string]bool       // whether execViaAgent has been tried since this VM last (re)started
	hostnameBusy         map[string]bool       // VMs with a hostname change in flight
	reservedNames        map[string]struct{}   // VM names picked by a create batch that isn't finished with them yet

	ipswMu               sync.Mutex                                // guards the restore image list below and serializes its refresh
	ipswEntries          []ipsw.Entry                              // cached AppleDB list of installable macOS restore images
	ipswFetched          time.Time                                 // when ipswEntries was fetched
	ipswFetch            ipswFetchFunc                             // nil = fetchIPSWFeed; replaced in tests
	ipswPicker           func(ctx context.Context) (string, error) // nil = chooseIPSWFile; replaced in tests
	guestAgent           guestAgentInfo                            // bundled guest agent PKG discovery and staging state
	guestAgentSearchDirs []string                                  // where to look for the bundled agent PKG; nil = next to the binary
	subs                 map[chan []byte]struct{}
	storageMounted       bool
	tartJSON             bool // whether `tart list --format json` is supported
	supportsProvisioning bool // whether the host (macOS 27+) and tart build support `run --provisioning-opts`
	statePath            string
	reload               chan struct{} // poke the scheduler when interval changes
	mdmCopier            mdm.ProfileCopier
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

// closeStaleHistory closes run-history entries left open by a server that
// stopped or crashed while their VM ran. Must run after the startup reconcile
// so VM states reflect tart. A VM that is still running keeps its newest open
// entry; every other open entry is marked StopUnknown.
func (m *Manager) closeStaleHistory() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	keep := map[*RunEvent]bool{}
	for i := len(m.history) - 1; i >= 0; i-- {
		ev := m.history[i]
		if !ev.StoppedAt.IsZero() {
			continue
		}
		vm := m.vms[ev.Name]
		active := vm != nil && (vm.State == "running" || vm.State == "starting")
		if !active {
			continue
		}
		claimed := false
		for k := range keep {
			if k.Name == ev.Name {
				claimed = true
				break
			}
		}
		if !claimed {
			keep[ev] = true
		}
	}
	closed := 0
	for _, ev := range m.history {
		if ev.StoppedAt.IsZero() && !keep[ev] {
			ev.StoppedAt = ev.StartedAt
			ev.StopUnknown = true
			closed++
		}
	}
	return closed
}

// stateSnapshot is what we send to the dashboard (GET /api/vms and SSE).
type stateSnapshot struct {
	VMs                  []*VM                  `json:"vms"`
	Config               configView             `json:"config"`
	StorageMounted       bool                   `json:"storageMounted"`
	StoragePath          string                 `json:"storagePath"`
	WithinHours          bool                   `json:"withinHours"` // currently inside the daily window
	Now                  time.Time              `json:"now"`
	Version              string                 `json:"version"`
	TartJSON             bool                   `json:"tartJSON"`
	TartInstalled        bool                   `json:"tartInstalled"`
	TartVersion          string                 `json:"tartVersion"`
	Updates              update.Views           `json:"updates"`
	SupportsProvisioning bool                   `json:"supportsProvisioning"`
	Tasks                []*Task                `json:"tasks"`
	CreateBatches        []createBatch          `json:"createBatches"`
	Performance          perf.PerformanceSample `json:"performance"`
	HostIP               string                 `json:"hostIP"`
	Logs                 []string               `json:"logs"`
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

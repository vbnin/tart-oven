package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/net/route"
)

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
	// Re-check guest access for all running VMs (non-busy ones only).
	m.mu.Lock()
	var toProbe []string
	for name, vm := range m.vms {
		if vm.State == "running" && !m.busy[name] {
			toProbe = append(toProbe, name)
		}
	}
	m.mu.Unlock()
	for _, name := range toProbe {
		go m.probeGuestChannels(name)
	}
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

package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const errNoBundledAgent = "no bundled guest agent package found"

// guestAgentInfo holds discovery and staging state for the bundled guest agent
// installer package. It's attached to Manager (mutex-guarded) and updated at
// startup and by stageGuestAgentInstaller.
type guestAgentInfo struct {
	Available bool   // whether a bundled PKG was found and staged
	Version   string // parsed from the filename, e.g. "0.15.0"
	PKGName   string // filename only, e.g. "tart-guest-agent-0.15.0.pkg"
	HostPath  string // absolute path on the host to the PKG
	GuestPath string // where the guest sees it (in the shared folder)
	Error     string // last discovery/staging error, if any
}

// findBundledGuestAgentPkg locates the highest-version tart-guest-agent-*.pkg
// in the typical install and dev locations. Returns empty strings if none found.
// searchDirs can be injected for testing; when nil, defaults to the standard
// layout (installed: <exe>/guest-agent/, dev: <exe>/packaging/build/).
func findBundledGuestAgentPkg(searchDirs []string) (pkgPath, licensePath, version string) {
	if searchDirs == nil {
		exe, err := os.Executable()
		if err != nil {
			return "", "", ""
		}
		exeDir := filepath.Dir(exe)
		searchDirs = []string{
			filepath.Join(exeDir, "guest-agent"),
			filepath.Join(exeDir, "packaging", "build"),
		}
	}

	type candidate struct {
		path    string
		version string
		major   int
		minor   int
		patch   int
	}
	var found []candidate

	pkgRe := regexp.MustCompile(`^tart-guest-agent-(\d+)\.(\d+)\.(\d+)\.pkg$`)
	for _, dir := range searchDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			m := pkgRe.FindStringSubmatch(e.Name())
			if m == nil {
				continue
			}
			major, _ := strconv.Atoi(m[1])
			minor, _ := strconv.Atoi(m[2])
			patch, _ := strconv.Atoi(m[3])
			found = append(found, candidate{
				path:    filepath.Join(dir, e.Name()),
				version: fmt.Sprintf("%d.%d.%d", major, minor, patch),
				major:   major,
				minor:   minor,
				patch:   patch,
			})
		}
	}

	if len(found) == 0 {
		return "", "", ""
	}

	// Pick the highest version
	sort.Slice(found, func(i, j int) bool {
		if found[i].major != found[j].major {
			return found[i].major > found[j].major
		}
		if found[i].minor != found[j].minor {
			return found[i].minor > found[j].minor
		}
		return found[i].patch > found[j].patch
	})

	pkgPath = found[0].path
	version = found[0].version
	dir := filepath.Dir(pkgPath)
	licensePath = filepath.Join(dir, "tart-guest-agent-LICENSE.txt")
	if _, err := os.Stat(licensePath); err != nil {
		licensePath = ""
	}
	return pkgPath, licensePath, version
}

// sha256file computes the SHA-256 checksum of a file.
func sha256file(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// stageGuestAgentInstaller copies the bundled agent PKG, LICENSE, and a
// double-clickable .command install script into <cfg.SharedDir>/tart-guest-agent/,
// making them visible inside every guest (tart run --dir mounts SharedDir).
// It's idempotent: skips copying a file if an identical one (same size + checksum)
// is already there, and removes older PKG versions. Updates m.guestAgent with
// the result (mutex-guarded). Never fails startup — records available=false on error.
func (m *Manager) stageGuestAgentInstaller() {
	m.mu.Lock()
	sharedDir := m.cfg.SharedDir
	m.mu.Unlock()

	if sharedDir == "" {
		m.mu.Lock()
		m.guestAgent = guestAgentInfo{Error: "shared directory not configured"}
		m.mu.Unlock()
		return
	}

	pkgPath, licensePath, version := findBundledGuestAgentPkg(m.guestAgentSearchDirs)
	if pkgPath == "" {
		m.mu.Lock()
		m.guestAgent = guestAgentInfo{Error: errNoBundledAgent}
		m.mu.Unlock()
		log.Printf("guest agent: %s (looked next to the binary in guest-agent/ and packaging/build/)", errNoBundledAgent)
		return
	}

	pkgName := filepath.Base(pkgPath)
	stageDir := filepath.Join(sharedDir, "tart-guest-agent")
	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		m.mu.Lock()
		m.guestAgent = guestAgentInfo{
			Error: fmt.Sprintf("could not create %s: %v", stageDir, err),
		}
		m.mu.Unlock()
		return
	}

	// Copy PKG (skip if identical)
	destPKG := filepath.Join(stageDir, pkgName)
	if err := copyIfChanged(pkgPath, destPKG); err != nil {
		m.mu.Lock()
		m.guestAgent = guestAgentInfo{
			Error: fmt.Sprintf("could not copy %s: %v", pkgName, err),
		}
		m.mu.Unlock()
		return
	}

	// Copy LICENSE if present
	if licensePath != "" {
		destLicense := filepath.Join(stageDir, "tart-guest-agent-LICENSE.txt")
		_ = copyIfChanged(licensePath, destLicense) // best effort
	}

	// Generate .command script
	commandScript := filepath.Join(stageDir, "Install Tart Guest Agent.command")
	scriptContent := fmt.Sprintf(`#!/bin/sh
cd "$(dirname "$0")"
echo "Installing Tart guest agent %s..."
sudo /usr/sbin/installer -pkg "%s" -target /
echo ""
echo "Press Return to close this window."
read dummy
`, version, pkgName)
	if err := os.WriteFile(commandScript, []byte(scriptContent), 0o755); err != nil {
		m.mu.Lock()
		m.guestAgent = guestAgentInfo{
			Error: fmt.Sprintf("could not write .command script: %v", err),
		}
		m.mu.Unlock()
		return
	}

	// Remove older PKG versions
	entries, _ := os.ReadDir(stageDir)
	pkgRe := regexp.MustCompile(`^tart-guest-agent-\d+\.\d+\.\d+\.pkg$`)
	for _, e := range entries {
		if e.IsDir() || e.Name() == pkgName || !pkgRe.MatchString(e.Name()) {
			continue
		}
		_ = os.Remove(filepath.Join(stageDir, e.Name()))
	}

	guestPath := fmt.Sprintf("/Volumes/My Shared Files/host_resources/tart-guest-agent/%s", pkgName)
	log.Printf("guest agent: staged %s into %s", pkgName, stageDir)
	m.mu.Lock()
	m.guestAgent = guestAgentInfo{
		Available: true,
		Version:   version,
		PKGName:   pkgName,
		HostPath:  destPKG,
		GuestPath: guestPath,
	}
	m.mu.Unlock()
}

// copyIfChanged copies src to dst only if dst is missing or differs in size or
// SHA-256 checksum. Returns nil if dst is already up-to-date.
func copyIfChanged(src, dst string) error {
	srcStat, err := os.Stat(src)
	if err != nil {
		return err
	}
	dstStat, err := os.Stat(dst)
	needCopy := false
	if err != nil {
		needCopy = true
	} else if srcStat.Size() != dstStat.Size() {
		needCopy = true
	} else {
		srcSum, err1 := sha256file(src)
		dstSum, err2 := sha256file(dst)
		if err1 != nil || err2 != nil || srcSum != dstSum {
			needCopy = true
		}
	}
	if !needCopy {
		return nil
	}

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

// installGuestAgent installs the bundled Tart guest agent PKG into a running VM
// over SSH, then verifies the result by asking the agent itself to run a command.
// SSH is the only possible transport here: the agent is precisely what is missing,
// so it cannot bootstrap itself.
func (m *Manager) installGuestAgent(name string) {
	t := m.newTask("agent-install", name)
	m.broadcast()

	m.mu.Lock()
	vm := m.vms[name]
	var vmCopy VM
	if vm != nil {
		vmCopy = *vm
	}
	cfg := m.cfg
	agentInfo := m.guestAgent
	m.mu.Unlock()

	if !agentInfo.Available {
		errMsg := "no bundled guest agent package — build it with packaging/build-agent-pkg.sh"
		if agentInfo.Error != "" && agentInfo.Error != errNoBundledAgent {
			errMsg = "guest agent package unavailable: " + agentInfo.Error
		}
		m.finishTask(t, errors.New(errMsg))
		m.broadcast()
		return
	}

	if vm == nil || vmCopy.State != "running" {
		m.finishTask(t, errors.New("the VM must be running to install the guest agent"))
		m.broadcast()
		return
	}

	_, password := effectiveSSHCredentials(cfg, &vmCopy)
	if strings.TrimSpace(password) == "" {
		m.finishTask(t, errors.New("a guest SSH password is required to install the agent; set one in Configuration"))
		m.broadcast()
		return
	}

	m.appendTaskOutput(t, fmt.Sprintf("Installing Tart guest agent %s on %s over SSH.\n", agentInfo.Version, name))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// Check the PKG is visible in the guest
	checkScript := fmt.Sprintf(`if [ ! -f "%s" ]; then
  echo "error: the guest cannot see the shared folder — restart the VM so it is mounted" >&2
  exit 1
fi
echo "Found %s in the guest"`, agentInfo.GuestPath, agentInfo.PKGName)
	res := m.sshExecContext(ctx, name, checkScript, "")
	if out := strings.TrimSpace(res.Stdout); out != "" {
		m.appendTaskOutput(t, out+"\n")
	}
	if errText := strings.TrimSpace(res.Stderr); errText != "" {
		m.appendTaskOutput(t, errText+"\n")
	}
	if res.Error != "" {
		m.finishTask(t, errors.New(res.Error))
		m.broadcast()
		return
	}
	if res.ExitCode != 0 {
		m.finishTask(t, fmt.Errorf("shared folder check failed with exit %d", res.ExitCode))
		m.broadcast()
		return
	}

	// Run the installer
	installScript := fmt.Sprintf(`sudo /usr/sbin/installer -pkg "%s" -target /`, agentInfo.GuestPath)
	m.appendTaskOutput(t, "==> running installer\n")
	res = m.sshExecContext(ctx, name, installScript, password)
	if out := strings.TrimSpace(res.Stdout); out != "" {
		m.appendTaskOutput(t, out+"\n")
	}
	if errText := strings.TrimSpace(res.Stderr); errText != "" {
		m.appendTaskOutput(t, errText+"\n")
	}
	if res.Error != "" {
		m.finishTask(t, errors.New(res.Error))
		m.broadcast()
		return
	}
	if res.ExitCode != 0 {
		m.finishTask(t, fmt.Errorf("installer exited %d", res.ExitCode))
		m.broadcast()
		return
	}

	// Verify through the agent itself
	m.appendTaskOutput(t, "==> verifying the agent responds\n")
	verifyCtx, cancelVerify := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancelVerify()
	// Clear the "known unavailable this boot" cache regardless of outcome —
	// we just did a fresh, authoritative check right here (see execInGuest).
	m.mu.Lock()
	delete(m.agentProbedSinceBoot, name)
	m.mu.Unlock()
	// The freshly loaded agent can take a few seconds to start listening, so
	// retry until the deadline. Only a clean `true` counts: execViaAgent also
	// reports an expired context as handled.
	responding := false
	for verifyCtx.Err() == nil {
		res, handled := m.execViaAgent(verifyCtx, name, "true", "")
		if handled && res.Error == "" && res.ExitCode == 0 {
			responding = true
			break
		}
		select {
		case <-verifyCtx.Done():
		case <-time.After(5 * time.Second):
		}
	}
	if !responding {
		m.setAgentOK(name, false)
		m.finishTask(t, errors.New("Installed, but the agent isn't answering yet. It starts when a user is logged in to the guest; log in (or enable auto-login) and run Refresh info."))
		m.broadcast()
		return
	}
	m.setAgentOK(name, true)
	m.appendTaskOutput(t, "The guest agent is responding. Commands for this VM no longer use SSH.\n")
	m.finishTask(t, nil)
	m.broadcast()
}

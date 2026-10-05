package server

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	tartoven "tart-oven"
	"tart-oven/internal/mdm"
	"tart-oven/web"
)

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
	OK          bool      `json:"ok"`
	Name        string    `json:"name,omitempty"`
	Path        string    `json:"path,omitempty"`
	PayloadUUID string    `json:"payloadUUID,omitempty"`
	Stage       mdm.Stage `json:"stage,omitempty"`
	Error       string    `json:"error,omitempty"`
}

func writeMDMProfileError(w http.ResponseWriter, status int, name string, stage mdm.Stage, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	writeJSON(w, mdmProfileResponse{Name: name, Stage: stage, Error: message})
}

func safeMDMStageError(stage mdm.Stage) (int, string) {
	switch stage {
	case mdm.StageConfiguration:
		return http.StatusBadRequest, "profile configuration is incomplete"
	case mdm.StageVM:
		return http.StatusBadRequest, "VM is not available"
	case mdm.StageIP:
		return http.StatusBadGateway, "could not resolve VM IP"
	case mdm.StageAuthentication:
		return http.StatusBadGateway, "SSH authentication failed"
	case mdm.StageSFTP:
		return http.StatusBadGateway, "SFTP upload failed"
	case mdm.StageVerification:
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
			writeMDMProfileError(w, http.StatusBadRequest, name, mdm.StageConfiguration,
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
		status, message := safeMDMStageError(mdm.StageConfiguration)
		writeMDMProfileError(w, status, name, mdm.StageConfiguration, message)
		return
	}
	if vm == nil || vmCopy.State != "running" {
		status, message := safeMDMStageError(mdm.StageVM)
		writeMDMProfileError(w, status, name, mdm.StageVM, message)
		return
	}

	ip := strings.TrimSpace(vmCopy.IP)
	if ip == "" {
		if m.mdmResolveIP == nil {
			log.Printf("MDM profile copy failed for VM %q at stage %s", name, mdm.StageIP)
			writeMDMProfileError(w, http.StatusInternalServerError, name, mdm.StageIP, "IP resolver unavailable")
			return
		}
		ip, err = m.mdmResolveIP(r.Context(), name, cfg.VMStoragePath)
		if err != nil || strings.TrimSpace(ip) == "" {
			log.Printf("MDM profile copy failed for VM %q at stage %s", name, mdm.StageIP)
			status, message := safeMDMStageError(mdm.StageIP)
			writeMDMProfileError(w, status, name, mdm.StageIP, message)
			return
		}
		ip = strings.TrimSpace(ip)
	}

	profile, payloadUUID, err := mdm.GenerateProfile(mdm.ProfileInput{
		BaseURL:        baseURL,
		InvitationCode: targetProfile.InvitationCode,
	}, cryptorand.Reader)
	if err != nil {
		log.Printf("MDM profile copy failed for VM %q at stage %s", name, mdm.StageConfiguration)
		status, message := safeMDMStageError(mdm.StageConfiguration)
		writeMDMProfileError(w, status, name, mdm.StageConfiguration, message)
		return
	}
	if m.mdmCopier == nil {
		log.Printf("MDM profile copy failed for VM %q at stage %s", name, mdm.StageSFTP)
		writeMDMProfileError(w, http.StatusInternalServerError, name, mdm.StageSFTP, "profile copy service unavailable")
		return
	}

	target := mdm.TransferTarget{
		Address:  net.JoinHostPort(ip, "22"),
		Username: username,
		Password: password,
		Timeout:  time.Duration(sshTimeout) * time.Second,
	}
	if err := m.mdmCopier.CopyAndVerify(r.Context(), target, profile, payloadUUID); err != nil {
		stage := mdm.StageSFTP
		var stageErr *mdm.StageError
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
		Path:        mdm.ProfileDisplayPath,
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
	indexHTML, _ := web.Content.ReadFile("index.html")
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})

	mux.HandleFunc("/icon.png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write(tartoven.Icon)
	})

	mux.HandleFunc("/api/readme", func(w http.ResponseWriter, r *http.Request) {
		b, err := tartoven.Docs.ReadFile("README.md")
		if err != nil {
			http.Error(w, "readme not embedded", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Write(b)
	})

	mux.HandleFunc("/api/changelog", func(w http.ResponseWriter, r *http.Request) {
		b, err := tartoven.Docs.ReadFile("CHANGELOG.md")
		if err != nil {
			http.Error(w, "changelog not embedded", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Write(b)
	})

	mux.HandleFunc("/CHANGELOG.md", func(w http.ResponseWriter, r *http.Request) {
		b, err := tartoven.Docs.ReadFile("CHANGELOG.md")
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
		var body struct {
			Name     string       `json:"name"`
			Headless bool         `json:"headless,omitempty"`
			Override *runOverride `json:"override,omitempty"` // "Run with arguments"
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(body.Name) == "" {
			http.Error(w, "missing name", http.StatusBadRequest)
			return
		}
		if err := validateRunOverride(body.Override); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		go m.doRun(body.Name, "manual", runOptions{Headless: body.Headless, Override: body.Override})
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
		m.mu.Lock()
		vm := m.vms[name]
		opts := runOptions{}
		if vm != nil {
			opts = runOptions{Headless: vm.Headless, Override: vm.RunOverride}
		}
		m.mu.Unlock()
		go func() {
			m.doStop(name)
			m.doRun(name, "manual", opts)
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
		m.probeGuestChannels(name)

		// The status command only fills the Info column; connectivity was probed above.
		m.mu.Lock()
		cmd := m.cfg.StatusCommand
		m.mu.Unlock()
		res := m.sshExec(name, cmd, "")
		_, info := sshOutcome(res)
		m.mu.Lock()
		if vm := m.vms[name]; vm != nil {
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
		req.NameTemplate = strings.TrimSpace(req.NameTemplate)
		if req.NameTemplate == "" && strings.TrimSpace(req.Prefix) != "" {
			req.NameTemplate = strings.TrimSpace(req.Prefix) + varRand8 // older API callers
		}
		if req.NameTemplate == "" {
			req.NameTemplate = varRand8
		}
		if err := validateNameTemplate(req.NameTemplate); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		req.Hostname = strings.TrimSpace(req.Hostname)
		if err := validateHostname(req.Hostname); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.Mode == "clone" {
			if strings.TrimSpace(req.Source) == "" {
				http.Error(w, "clone requires a source VM", http.StatusBadRequest)
				return
			}
			if req.AutoEnrollProfile != "" {
				m.mu.Lock()
				_, found := jamfProfileByID(m.cfg, req.AutoEnrollProfile)
				m.mu.Unlock()
				if !found {
					http.Error(w, "unknown Jamf server profile", http.StatusBadRequest)
					return
				}
			}
		} else {
			req.Mode = "ipsw"
			src, err := normalizeIpswSource(req.FromIpsw)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			req.FromIpsw = src
		}
		batchID := fmt.Sprintf("b%d", time.Now().UnixNano())
		m.mu.Lock()
		if m.batches == nil {
			m.batches = map[string]*createBatch{}
		}
		m.batches[batchID] = &createBatch{ID: batchID, Mode: req.Mode, Total: req.Count}
		m.mu.Unlock()
		go m.createVMs(req, batchID)
		writeJSON(w, map[string]any{"ok": true, "batch": batchID})
	})

	mux.HandleFunc("/api/ipsw/sources", m.handleIPSWSources)
	mux.HandleFunc("/api/ipsw/choose-file", m.handleIPSWChooseFile)
	mux.HandleFunc("/api/oci/pull", m.handleOCIPull)

	// Cancels a running Activity task (create/clone/install) by killing its
	// in-flight command. No-op if the task already finished or doesn't exist.
	mux.HandleFunc("/api/task/cancel", func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			ID    string `json:"id"`
			Batch string `json:"batch"`
		}
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if b.Batch != "" {
			if !m.cancelBatch(b.Batch) {
				writeJSON(w, map[string]string{"error": "batch not running or not found"})
				return
			}
			writeJSON(w, map[string]bool{"ok": true})
			return
		}
		if b.ID == "" {
			http.Error(w, "id or batch required", http.StatusBadRequest)
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
		b.NewName = strings.TrimSpace(b.NewName)
		if b.Name == "" || b.NewName == "" {
			http.Error(w, "name and newName required", http.StatusBadRequest)
			return
		}
		if m.isActive(b.Name) {
			writeJSON(w, map[string]string{"error": "stop the VM before renaming it"})
			return
		}
		// The new name is a template, like when creating VMs: $RAND8 and
		// $AUTONUM are expanded, and a taken name gets -1, -2, ... added.
		if err := validateNameTemplate(b.NewName); err != nil {
			writeJSON(w, map[string]string{"error": err.Error()})
			return
		}
		if b.NewName == b.Name && !hasNameVariable(b.NewName) {
			writeJSON(w, map[string]string{"output": "", "name": b.Name}) // nothing to rename
			return
		}
		newName, err := m.allocVMName(newVMNamer(b.NewName, nil))
		if err != nil {
			writeJSON(w, map[string]string{"error": err.Error()})
			return
		}
		defer m.releaseVMName(newName)
		b.NewName = newName
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
		res := map[string]string{"output": string(out), "name": b.NewName}
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
			AutoEnroll  *bool    `json:"autoEnroll,omitempty"`

			// Jamf server profile to auto-enroll into at the next boot; "" turns
			// auto-enroll off.
			AutoEnrollProfile *string `json:"autoEnrollProfile,omitempty"`

			Hostname         *string `json:"hostname,omitempty"`
			HostnameFromName *bool   `json:"hostnameFromName,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if b.Name == "" {
			http.Error(w, "name required", http.StatusBadRequest)
			return
		}
		if b.Hostname != nil {
			if err := validateHostname(*b.Hostname); err != nil {
				writeJSON(w, map[string]string{"error": err.Error()})
				return
			}
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
		// AutoEnroll: only change when explicitly present (pointers).
		if err := applyAutoEnrollChoice(vm, m.cfg, b.AutoEnroll, b.AutoEnrollProfile); err != nil {
			m.mu.Unlock()
			writeJSON(w, map[string]string{"error": err.Error()})
			return
		}
		if b.Hostname != nil {
			vm.Hostname = strings.TrimSpace(*b.Hostname)
		}
		if b.HostnameFromName != nil {
			vm.HostnameFromName = *b.HostnameFromName
		}
		running := vm.State == "running"
		m.save()
		m.mu.Unlock()
		m.broadcast()
		if running {
			go m.applyHostnameIfPending(b.Name)
		}
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

	mux.HandleFunc("/api/guest-agent", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		info := m.guestAgent
		m.mu.Unlock()
		writeJSON(w, map[string]interface{}{
			"available": info.Available,
			"version":   info.Version,
			"pkgName":   info.PKGName,
			"guestPath": info.GuestPath,
			"error":     info.Error,
		})
	})

	mux.HandleFunc("/api/guest-agent/pkg", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		info := m.guestAgent
		m.mu.Unlock()
		if !info.Available || info.HostPath == "" {
			http.Error(w, "guest agent package not available", http.StatusNotFound)
			return
		}
		f, err := os.Open(info.HostPath)
		if err != nil {
			http.Error(w, "could not open package file", http.StatusInternalServerError)
			return
		}
		defer f.Close()
		stat, err := f.Stat()
		if err != nil {
			http.Error(w, "could not stat package file", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", info.PKGName))
		w.Header().Set("Content-Length", fmt.Sprintf("%d", stat.Size()))
		io.Copy(w, f)
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
		go m.runAutoEnrollTask(name, "manual", "")
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
	mux.HandleFunc("/api/updates/dismiss", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Kind string `json:"kind"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := m.dismissUpdate(body.Kind); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		m.broadcast()
		writeJSON(w, map[string]bool{"ok": true})
	})
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

	m.registerAuthRoutes(mux)
	m.registerTLSRoutes(mux)
	m.registerSetupRoutes(mux)

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

	if err := m.prepareTLSFields(fields); err != nil {
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
	prevShared := m.cfg.SharedDir

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
	if raw, ok := fields["tlsEnabled"]; ok {
		var v bool
		if json.Unmarshal(raw, &v) == nil {
			m.cfg.TLSEnabled = v
		}
	}
	for key, field := range map[string]*string{"tlsCertPath": &m.cfg.TLSCertPath, "tlsKeyPath": &m.cfg.TLSKeyPath} {
		if raw, ok := fields[key]; ok {
			var v string
			if json.Unmarshal(raw, &v) == nil {
				*field = strings.TrimSpace(v)
			}
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
	if raw, ok := fields["showJamfFeatures"]; ok {
		var v bool
		if json.Unmarshal(raw, &v) == nil {
			m.cfg.ShowJamfFeatures = v
		}
	}
	for key, field := range map[string]*bool{
		"disableTartUpdateCheck": &m.cfg.DisableTartUpdateCheck,
		"disableOvenUpdateCheck": &m.cfg.DisableOvenUpdateCheck,
	} {
		if raw, ok := fields[key]; ok {
			var v bool
			if json.Unmarshal(raw, &v) == nil {
				if *field && !v {
					go m.checkUpdates() // turned back on: don't wait for the next daily check
				}
				*field = v
			}
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
	if m.cfg.SharedDir != prevShared {
		m.ensureSharedDir()
		go m.stageGuestAgentInstaller()
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

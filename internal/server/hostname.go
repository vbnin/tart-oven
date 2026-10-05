package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const maxHostnameLen = 63

// desiredHostname is the name a VM's guest should carry, or "" when Tart Oven
// doesn't manage it.
func desiredHostname(vm *VM) string {
	if vm.HostnameFromName {
		return vm.Name
	}
	return vm.Hostname
}

// validateHostname checks a user-entered computer name. Spaces and punctuation
// are allowed (they're fine in ComputerName); the network names are derived
// with dnsHostname.
func validateHostname(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	if len(name) > maxHostnameLen {
		return fmt.Errorf("hostname must be %d characters or fewer", maxHostnameLen)
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return errors.New("hostname contains control characters")
		}
	}
	if dnsHostname(name) == "" {
		return errors.New("hostname needs at least one letter or digit")
	}
	return nil
}

// dnsHostname turns a computer name into a valid HostName / LocalHostName:
// ASCII letters, digits and hyphens only, no leading or trailing hyphen.
func dnsHostname(name string) string {
	var b strings.Builder
	lastHyphen := false
	for _, r := range name {
		if r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			b.WriteRune(r)
			lastHyphen = false
		} else if !lastHyphen && b.Len() > 0 {
			b.WriteByte('-')
			lastHyphen = true
		}
	}
	s := strings.TrimRight(b.String(), "-")
	if len(s) > maxHostnameLen {
		s = strings.TrimRight(s[:maxHostnameLen], "-")
	}
	return s
}

// cloneHostname is the custom hostname for one clone of a batch: as typed for
// a single clone, with the VM's random suffix appended when creating several
// so they stay unique on the network.
func cloneHostname(hostname, id string, count int) string {
	if hostname == "" || count <= 1 {
		return hostname
	}
	suffix := "-" + id
	for len(hostname)+len(suffix) > maxHostnameLen {
		_, size := utf8.DecodeLastRuneInString(hostname)
		hostname = hostname[:len(hostname)-size]
	}
	return hostname + suffix
}

// hostnameCommand sets ComputerName, LocalHostName and HostName in one sudo
// call, so the password on stdin is only read once.
func hostnameCommand(name string) string {
	dns := dnsHostname(name)
	script := fmt.Sprintf("scutil --set ComputerName %s && scutil --set LocalHostName %s && scutil --set HostName %s",
		shellQuote(name), shellQuote(dns), shellQuote(dns))
	return "sudo /bin/sh -c " + shellQuote(script)
}

// shellQuote wraps s in single quotes for /bin/sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// applyHostnameIfPending sets the guest's hostname when the desired name
// differs from the one last applied. Safe to call repeatedly: it does nothing
// once applied, and a failed attempt is retried on the next call.
func (m *Manager) applyHostnameIfPending(name string) {
	m.mu.Lock()
	vm := m.vms[name]
	if vm == nil || vm.State != "running" || m.hostnameBusy[name] {
		m.mu.Unlock()
		return
	}
	want := desiredHostname(vm)
	if want == "" || want == vm.HostnameApplied {
		m.mu.Unlock()
		return
	}
	if m.hostnameBusy == nil {
		m.hostnameBusy = map[string]bool{}
	}
	m.hostnameBusy[name] = true
	vmCopy := *vm
	cfg := m.cfg
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.hostnameBusy, name)
		m.mu.Unlock()
	}()

	_, password := effectiveSSHCredentials(cfg, &vmCopy)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	res := m.execInGuest(ctx, name, hostnameCommand(want), password)
	cancel()
	if res.Error != "" || res.ExitCode != 0 {
		detail := res.Error
		if detail == "" {
			detail = strings.TrimSpace(res.Stderr)
		}
		m.logln("hostname %s: could not set %q: %s", name, want, detail)
		return
	}
	m.mu.Lock()
	if vm := m.vms[name]; vm != nil {
		vm.HostnameApplied = want
		m.save()
	}
	m.mu.Unlock()
	m.logln("hostname %s: set to %q", name, want)
	m.broadcast()
}

package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// HTTPS
//
// TLS is opt-in. The operator either points at their own PEM certificate and
// key, or leaves the paths empty and Tart Oven manages a self-signed pair in
// <state dir>/tls. A bad configuration is refused when saved, and a server
// that cannot load its certificate refuses to start rather than fall back to
// plain HTTP.
// ---------------------------------------------------------------------------

const (
	selfSignedValidity = 365 * 24 * time.Hour
	selfSignedRenewAt  = 30 * 24 * time.Hour
)

func (m *Manager) tlsDir() string { return filepath.Join(filepath.Dir(m.statePath), "tls") }

func (m *Manager) managedTLSPaths() (cert, key string) {
	return filepath.Join(m.tlsDir(), "cert.pem"), filepath.Join(m.tlsDir(), "key.pem")
}

// localSANs lists the names and addresses a self-signed cert should cover.
func localSANs(extraIP string) ([]string, []net.IP) {
	names := []string{"localhost"}
	if h, err := os.Hostname(); err == nil && h != "" {
		names = append(names, h)
	}
	ips := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
	if ip := net.ParseIP(extraIP); ip != nil {
		ips = append(ips, ip)
	}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && !n.IP.IsLinkLocalUnicast() && !n.IP.IsLoopback() {
				ips = append(ips, n.IP)
			}
		}
	}
	return names, ips
}

// generateSelfSigned writes a fresh ECDSA P-256 certificate and key.
func generateSelfSigned(certPath, keyPath string, names []string, ips []net.IP) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Tart Oven", Organization: []string{"Tart Oven (self-signed)"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(selfSignedValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              names,
		IPAddresses:           ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(certPath), 0o700); err != nil {
		return err
	}
	if err := writeFileAtomic(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	return writeFileAtomic(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// regenerateManagedTLS replaces the managed self-signed pair.
func (m *Manager) regenerateManagedTLS() (cert, key string, err error) {
	cert, key = m.managedTLSPaths()
	names, ips := localSANs(m.hostIP)
	return cert, key, generateSelfSigned(cert, key, names, ips)
}

// certInfo describes the first certificate in a PEM file.
type certInfo struct {
	Subject     string    `json:"subject"`
	Issuer      string    `json:"issuer"`
	NotAfter    time.Time `json:"notAfter"`
	Fingerprint string    `json:"fingerprint"` // SHA-256, colon-separated hex
	SelfSigned  bool      `json:"selfSigned"`
	Names       []string  `json:"names"`
}

func readCertInfo(certPath string) (*certInfo, error) {
	data, err := os.ReadFile(certPath)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("no PEM certificate found")
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(c.Raw)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	names := append([]string{}, c.DNSNames...)
	for _, ip := range c.IPAddresses {
		names = append(names, ip.String())
	}
	return &certInfo{
		Subject:     c.Subject.String(),
		Issuer:      c.Issuer.String(),
		NotAfter:    c.NotAfter,
		Fingerprint: strings.Join(parts, ":"),
		SelfSigned:  c.Subject.String() == c.Issuer.String(),
		Names:       names,
	}, nil
}

// validateTLSPair checks that the certificate and key load together and that
// the certificate has not expired.
func validateTLSPair(certPath, keyPath string) error {
	if !filepath.IsAbs(certPath) || !filepath.IsAbs(keyPath) {
		return errors.New("certificate and key paths must be absolute")
	}
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return fmt.Errorf("cannot load certificate and key: %v", err)
	}
	if len(pair.Certificate) > 0 {
		if c, err := x509.ParseCertificate(pair.Certificate[0]); err == nil && time.Now().After(c.NotAfter) {
			return errors.New("the certificate has expired")
		}
	}
	return nil
}

// prepareTLSFields validates TLS settings in a config POST before anything is
// applied. Turning HTTPS on with no paths provisions the managed cert and
// rewrites fields to point at it.
func (m *Manager) prepareTLSFields(fields map[string]json.RawMessage) error {
	_, hasEnabled := fields["tlsEnabled"]
	_, hasCert := fields["tlsCertPath"]
	_, hasKey := fields["tlsKeyPath"]
	if !hasEnabled && !hasCert && !hasKey {
		return nil
	}
	m.mu.Lock()
	enabled, cert, key := m.cfg.TLSEnabled, m.cfg.TLSCertPath, m.cfg.TLSKeyPath
	m.mu.Unlock()
	if raw, ok := fields["tlsEnabled"]; ok {
		if err := json.Unmarshal(raw, &enabled); err != nil {
			return errors.New("tlsEnabled must be true or false")
		}
	}
	for k, dst := range map[string]*string{"tlsCertPath": &cert, "tlsKeyPath": &key} {
		if raw, ok := fields[k]; ok {
			if err := json.Unmarshal(raw, dst); err != nil {
				return fmt.Errorf("%s must be a string", k)
			}
			*dst = strings.TrimSpace(*dst)
		}
	}
	if !enabled {
		fields["tlsCertPath"], _ = json.Marshal(cert)
		fields["tlsKeyPath"], _ = json.Marshal(key)
		return nil
	}
	if (cert == "") != (key == "") {
		return errors.New("set both the certificate and the key path, or leave both empty for a self-signed certificate")
	}
	if cert == "" {
		c, k := m.managedTLSPaths()
		if _, err := os.Stat(c); err != nil {
			if c, k, err = m.regenerateManagedTLS(); err != nil {
				return fmt.Errorf("cannot create a self-signed certificate: %v", err)
			}
		}
		cert, key = c, k
	}
	if err := validateTLSPair(cert, key); err != nil {
		return err
	}
	fields["tlsCertPath"], _ = json.Marshal(cert)
	fields["tlsKeyPath"], _ = json.Marshal(key)
	return nil
}

// startupTLS returns the listener's TLS config, or nil when HTTPS is off.
func (m *Manager) startupTLS() (*tls.Config, error) {
	m.mu.Lock()
	enabled, cert, key := m.cfg.TLSEnabled, m.cfg.TLSCertPath, m.cfg.TLSKeyPath
	m.mu.Unlock()
	if !enabled {
		return nil, nil
	}
	mc, mk := m.managedTLSPaths()
	if cert == "" || key == "" {
		cert, key = mc, mk
	}
	if cert == mc && key == mk {
		// Managed pair: create it, and renew it before it expires.
		if info, err := readCertInfo(cert); err != nil || time.Until(info.NotAfter) < selfSignedRenewAt {
			var err error
			if cert, key, err = m.regenerateManagedTLS(); err != nil {
				return nil, fmt.Errorf("self-signed certificate: %v", err)
			}
			m.logln("generated a self-signed certificate at %s", cert)
		}
	}
	if err := validateTLSPair(cert, key); err != nil {
		return nil, err
	}
	pair, err := tls.LoadX509KeyPair(cert, key)
	if err != nil {
		return nil, err
	}
	if info, err := readCertInfo(cert); err == nil && time.Until(info.NotAfter) < selfSignedRenewAt {
		m.logln("WARNING: the TLS certificate expires %s", info.NotAfter.Format("2006-01-02"))
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}}, nil
}

func (m *Manager) registerTLSRoutes(mux *http.ServeMux) {
	// GET: what is configured and what the listener is doing.
	mux.HandleFunc("/api/tls", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		cfg := m.cfg
		m.mu.Unlock()
		cert := cfg.TLSCertPath
		if cert == "" {
			cert, _ = m.managedTLSPaths()
		}
		out := map[string]any{"active": m.tlsActive, "enabled": cfg.TLSEnabled, "managed": cfg.TLSCertPath == ""}
		if info, err := readCertInfo(cert); err == nil {
			out["cert"] = info
		}
		writeJSON(w, out)
	})

	// POST: (re)generate the managed self-signed certificate and select it.
	mux.HandleFunc("/api/tls/self-signed", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		cert, key, err := m.regenerateManagedTLS()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		m.mu.Lock()
		m.cfg.TLSCertPath, m.cfg.TLSKeyPath = cert, key
		m.save()
		m.mu.Unlock()
		m.logln("generated a self-signed certificate at %s", cert)
		info, _ := readCertInfo(cert)
		writeJSON(w, map[string]any{"ok": true, "cert": info})
	})
}

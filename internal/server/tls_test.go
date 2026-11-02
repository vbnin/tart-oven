package server

import (
	"crypto/tls"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func timeNowPlus(sec int) time.Time { return time.Now().Add(time.Duration(sec) * time.Second) }

func TestManagedSelfSignedCert(t *testing.T) {
	m, _ := newAuthTestManager(t)
	cert, key, err := m.regenerateManagedTLS()
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(key); fi.Mode().Perm() != 0o600 {
		t.Fatalf("key mode = %v", fi.Mode().Perm())
	}
	if err := validateTLSPair(cert, key); err != nil {
		t.Fatal(err)
	}
	info, err := readCertInfo(cert)
	if err != nil || !info.SelfSigned || len(info.Fingerprint) != 95 {
		t.Fatalf("info = %+v, %v", info, err)
	}
	found := false
	for _, n := range info.Names {
		found = found || n == "localhost" || n == "127.0.0.1"
	}
	if !found {
		t.Fatalf("SANs = %v", info.Names)
	}
}

func TestStartupTLS(t *testing.T) {
	m, _ := newAuthTestManager(t)
	if conf, err := m.startupTLS(); conf != nil || err != nil {
		t.Fatalf("off = %v, %v", conf, err)
	}
	m.cfg.TLSEnabled = true
	conf, err := m.startupTLS() // provisions the managed pair
	if err != nil || conf == nil || conf.MinVersion != tls.VersionTLS12 {
		t.Fatalf("managed = %v, %v", conf, err)
	}
	m.cfg.TLSCertPath = filepath.Join(t.TempDir(), "missing.pem")
	m.cfg.TLSKeyPath = m.cfg.TLSCertPath
	if conf, err := m.startupTLS(); err == nil || conf != nil {
		t.Fatal("a missing user cert must stop startup, not fall back to HTTP")
	}
}

func TestConfigRejectsBadTLSAndProvisionsManaged(t *testing.T) {
	m, h := newAuthTestManager(t)
	post := func(body string) int { return do(h, "POST", "/api/config", body, nil).Code }

	if c := post(`{"tlsEnabled":true,"tlsCertPath":"/nope/c.pem","tlsKeyPath":"/nope/k.pem"}`); c != http.StatusBadRequest {
		t.Errorf("bad paths = %d", c)
	}
	if c := post(`{"tlsEnabled":true,"tlsCertPath":"/only/cert.pem"}`); c != http.StatusBadRequest {
		t.Errorf("cert without key = %d", c)
	}
	if m.cfg.TLSEnabled {
		t.Fatal("rejected config was applied")
	}
	if c := post(`{"tlsEnabled":true}`); c != http.StatusOK {
		t.Fatalf("enable managed = %d", c)
	}
	if !m.cfg.TLSEnabled || !strings.HasSuffix(m.cfg.TLSCertPath, "cert.pem") {
		t.Fatalf("cfg = %+v", m.cfg)
	}

	rec := do(h, "GET", "/api/tls", "", nil)
	var st struct {
		Enabled bool
		Managed bool
		Cert    *certInfo
	}
	json.Unmarshal(rec.Body.Bytes(), &st)
	if !st.Enabled || st.Cert == nil {
		t.Fatalf("tls status = %s", rec.Body)
	}
	if c := do(h, "POST", "/api/tls/self-signed", "", nil).Code; c != 200 {
		t.Fatalf("regenerate = %d", c)
	}
}

func TestServesOverTLS(t *testing.T) {
	m, h := newAuthTestManager(t)
	cert, key, err := m.regenerateManagedTLS()
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.LoadX509KeyPair(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	srv := newTLSTestServer(h, pair)
	defer srv.Close()
	client := srv.Client()
	resp, err := client.Get(srv.URL + "/api/health")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.TLS == nil || resp.StatusCode != 200 {
		t.Fatalf("resp = %+v", resp)
	}
}

package auth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTokenHashRoundTrip(t *testing.T) {
	tok, err := NewToken()
	if err != nil || !strings.HasPrefix(tok, "to_") {
		t.Fatalf("token = %q, %v", tok, err)
	}
	dir := t.TempDir()
	if h, err := Load(dir); h != "" || err != nil {
		t.Fatalf("missing file = %q, %v", h, err)
	}
	if err := Save(dir, Hash(tok)); err != nil {
		t.Fatal(err)
	}
	h, err := Load(dir)
	if err != nil || !Match(h, tok) || Match(h, tok+"x") || Match(h, "") {
		t.Fatalf("match failed: %q %v", h, err)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, FileName)); strings.Contains(string(data), tok) {
		t.Fatal("plaintext token stored")
	}
	if fi, _ := os.Stat(filepath.Join(dir, FileName)); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", fi.Mode().Perm())
	}
	if err := Remove(dir); err != nil {
		t.Fatal(err)
	}
	if h, _ := Load(dir); h != "" {
		t.Fatal("still enabled after Remove")
	}
	if err := Remove(dir); err != nil {
		t.Fatalf("second Remove: %v", err)
	}
}

func TestDamagedFileNeverMatches(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, FileName), []byte("{nope"), 0o600)
	if _, err := Load(dir); err == nil {
		t.Fatal("damaged file loaded")
	}
	if Match(Locked, "anything") {
		t.Fatal("Locked matched")
	}
}

func TestControlKey(t *testing.T) {
	dir := t.TempDir()
	key, err := NewControlKey(dir)
	if err != nil || key == "" || ReadControlKey(dir) != key {
		t.Fatalf("key = %q, %v", key, err)
	}
}

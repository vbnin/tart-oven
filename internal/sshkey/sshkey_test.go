package sshkey

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestEnsureSSHKeyPairGeneratesWhenMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tart-oven")
	pub, err := EnsureKeyPair(path)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if !strings.HasPrefix(string(pub), "ssh-ed25519 ") {
		t.Fatalf("public key = %q", pub)
	}
	if _, _, _, _, err := ssh.ParseAuthorizedKey(pub); err != nil {
		t.Fatalf("public key does not parse: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read private key: %v", err)
	}
	if _, err := ssh.ParsePrivateKey(raw); err != nil {
		t.Fatalf("private key does not parse: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("private key mode = %o, want 600", mode)
	}
}

func TestEnsureSSHKeyPairNeverOverwritesAnExistingKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tart-oven")
	first, err := EnsureKeyPair(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := EnsureKeyPair(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatal("existing key was regenerated")
	}
}

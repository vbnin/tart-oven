package sshkey

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
)

// EnsureKeyPair returns the public key for the identity at path, generating an
// ed25519 pair when the private key does not exist yet. An existing key is never
// overwritten: the public half is re-derived from it if the .pub file is missing.
//
// Tart Oven never pushes this key into a guest. It exists so the SSH fallback has a
// usable identity for images that do not ship the Tart guest agent; installing it
// in a guest is a documented one-time operator step.
func EnsureKeyPair(path string) ([]byte, error) {
	path = expandHome(strings.TrimSpace(path))
	if path == "" {
		return nil, errors.New("no SSH identity file configured")
	}
	publicPath := path + ".pub"

	if raw, err := os.ReadFile(path); err == nil {
		if pub, err := os.ReadFile(publicPath); err == nil && len(bytes.TrimSpace(pub)) > 0 {
			return bytes.TrimSpace(pub), nil
		}
		signer, err := ssh.ParsePrivateKey(raw)
		if err != nil {
			return nil, fmt.Errorf("parse existing SSH private key: %w", err)
		}
		pub := bytes.TrimSpace(ssh.MarshalAuthorizedKey(signer.PublicKey()))
		if err := os.WriteFile(publicPath, append(pub, '\n'), 0o644); err != nil {
			return nil, fmt.Errorf("write SSH public key: %w", err)
		}
		return pub, nil
	}

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate SSH key: %w", err)
	}
	block, err := ssh.MarshalPrivateKey(privateKey, "tart-oven")
	if err != nil {
		return nil, fmt.Errorf("encode SSH private key: %w", err)
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create SSH key directory: %w", err)
		}
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		return nil, fmt.Errorf("write SSH private key: %w", err)
	}
	sshPublic, err := ssh.NewPublicKey(publicKey)
	if err != nil {
		return nil, fmt.Errorf("derive SSH public key: %w", err)
	}
	pub := bytes.TrimSpace(ssh.MarshalAuthorizedKey(sshPublic))
	if err := os.WriteFile(publicPath, append(pub, '\n'), 0o644); err != nil {
		return nil, fmt.Errorf("write SSH public key: %w", err)
	}
	return pub, nil
}

func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

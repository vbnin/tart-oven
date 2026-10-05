// Package auth holds the UI access-token primitives shared by the server and
// the CLI. Only a SHA-256 hash of the token is ever stored, in auth.json next
// to state.json, so a lost token can be replaced but never read back.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

const (
	// FileName holds the token hash, in the state directory.
	FileName = "auth.json"
	// ControlFileName holds a per-start secret that lets the local CLI
	// stop and restart the server without knowing the UI token.
	ControlFileName = "control.key"
	// Locked is the hash used when auth.json exists but cannot be read: it
	// matches nothing, so a damaged file locks the UI instead of opening it.
	Locked = "!"
	prefix = "to_"
)

// NewToken returns a fresh random access token.
func NewToken() (string, error) {
	return randomString(prefix, 32)
}

// NewSessionID returns a random browser session identifier.
func NewSessionID() (string, error) {
	return randomString("", 32)
}

func randomString(p string, n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return p + base64.RawURLEncoding.EncodeToString(b), nil
}

// Hash returns the hex SHA-256 of a token.
func Hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Match reports whether token hashes to the stored hash, in constant time.
func Match(hash, token string) bool {
	if hash == "" || hash == Locked || token == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(hash), []byte(Hash(token))) == 1
}

// Load reads the stored hash. A missing file is not an error: it means the
// token is disabled and the returned hash is empty.
func Load(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, FileName))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var f struct {
		TokenHash string `json:"tokenHash"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return "", err
	}
	if len(f.TokenHash) != sha256.Size*2 {
		return "", errors.New("auth.json has no valid token hash")
	}
	return f.TokenHash, nil
}

// Save atomically writes the hash with owner-only permissions.
func Save(dir, hash string) error {
	data, err := json.MarshalIndent(map[string]string{"tokenHash": hash}, "", "  ")
	if err != nil {
		return err
	}
	return writePrivate(filepath.Join(dir, FileName), data)
}

// Remove disables the token.
func Remove(dir string) error {
	err := os.Remove(filepath.Join(dir, FileName))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// NewControlKey creates and stores a fresh control key.
func NewControlKey(dir string) (string, error) {
	key, err := randomString("", 32)
	if err != nil {
		return "", err
	}
	return key, writePrivate(filepath.Join(dir, ControlFileName), []byte(key))
}

// ReadControlKey returns the stored control key, or "" when there is none.
func ReadControlKey(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, ControlFileName))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func writePrivate(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

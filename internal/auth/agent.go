package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// AgentFileName holds the agent-scope tokens, in the state directory. An agent
// token can only reach /api/agent/*; it never opens the dashboard.
const AgentFileName = "agent-tokens.json"

const agentPrefix = "ta_"

// AgentToken is one named agent credential. Only the hash is stored.
type AgentToken struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Hash      string    `json:"hash"`
	CreatedAt time.Time `json:"createdAt"`
}

var agentNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,39}$`)

// ValidAgentName reports whether name can label an agent token. It becomes part
// of VM names and lease ownership, so it is kept to a short, shell-safe set.
func ValidAgentName(name string) error {
	if !agentNameRe.MatchString(name) {
		return errors.New("agent name must be 1-40 letters, digits, '.', '_' or '-', starting with a letter or digit")
	}
	return nil
}

// NewAgentToken returns a fresh random agent token.
func NewAgentToken() (string, error) {
	return randomString(agentPrefix, 32)
}

// NewAgentID returns a short random identifier for a token entry.
func NewAgentID() (string, error) {
	return randomString("", 6)
}

// LoadAgentTokens reads the agent tokens. A missing file means there are none.
func LoadAgentTokens(dir string) ([]AgentToken, error) {
	data, err := os.ReadFile(filepath.Join(dir, AgentFileName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f struct {
		Tokens []AgentToken `json:"tokens"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, err
	}
	return f.Tokens, nil
}

// SaveAgentTokens atomically writes the tokens with owner-only permissions. An
// empty list removes the file.
func SaveAgentTokens(dir string, tokens []AgentToken) error {
	if len(tokens) == 0 {
		return RemoveAgentTokens(dir)
	}
	data, err := json.MarshalIndent(map[string]any{"tokens": tokens}, "", "  ")
	if err != nil {
		return err
	}
	return writePrivate(filepath.Join(dir, AgentFileName), data)
}

// RemoveAgentTokens deletes every agent token.
func RemoveAgentTokens(dir string) error {
	err := os.Remove(filepath.Join(dir, AgentFileName))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// AddAgentToken creates a token named name and returns its plaintext once.
func AddAgentToken(dir, name string) (string, error) {
	name = strings.TrimSpace(name)
	if err := ValidAgentName(name); err != nil {
		return "", err
	}
	tokens, err := LoadAgentTokens(dir)
	if err != nil {
		return "", err
	}
	for _, t := range tokens {
		if strings.EqualFold(t.Name, name) {
			return "", fmt.Errorf("an agent token named %q already exists", name)
		}
	}
	plain, err := NewAgentToken()
	if err != nil {
		return "", err
	}
	id, err := NewAgentID()
	if err != nil {
		return "", err
	}
	tokens = append(tokens, AgentToken{ID: id, Name: name, Hash: Hash(plain), CreatedAt: time.Now().UTC()})
	return plain, SaveAgentTokens(dir, tokens)
}

// RevokeAgentToken removes the token whose name or ID is key.
func RevokeAgentToken(dir, key string) error {
	tokens, err := LoadAgentTokens(dir)
	if err != nil {
		return err
	}
	kept := tokens[:0]
	found := false
	for _, t := range tokens {
		if t.ID == key || strings.EqualFold(t.Name, key) {
			found = true
			continue
		}
		kept = append(kept, t)
	}
	if !found {
		return fmt.Errorf("no agent token %q", key)
	}
	return SaveAgentTokens(dir, kept)
}

// MatchAgent returns the token that the plaintext belongs to, comparing in
// constant time against every stored hash.
func MatchAgent(tokens []AgentToken, plain string) (AgentToken, bool) {
	var hit AgentToken
	found := false
	for _, t := range tokens {
		if Match(t.Hash, plain) { // keep going: don't leak position by returning early
			hit, found = t, true
		}
	}
	return hit, found
}

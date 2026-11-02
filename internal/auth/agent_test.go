package auth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentTokenLifecycle(t *testing.T) {
	dir := t.TempDir()
	tok, err := AddAgentToken(dir, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok, "ta_") {
		t.Fatalf("token %q has no agent prefix", tok)
	}
	tokens, err := LoadAgentTokens(dir)
	if err != nil || len(tokens) != 1 {
		t.Fatalf("load = %v, %v", tokens, err)
	}
	if strings.Contains(readFile(t, filepath.Join(dir, AgentFileName)), tok) {
		t.Fatal("plaintext token was stored")
	}
	if got, ok := MatchAgent(tokens, tok); !ok || got.Name != "claude" {
		t.Fatalf("MatchAgent = %v, %v", got, ok)
	}
	if _, ok := MatchAgent(tokens, tok+"x"); ok {
		t.Fatal("a wrong token matched")
	}
	if _, ok := MatchAgent(tokens, ""); ok {
		t.Fatal("an empty token matched")
	}
	if _, err := AddAgentToken(dir, "CLAUDE"); err == nil {
		t.Fatal("duplicate name (case-insensitive) was accepted")
	}
	if err := RevokeAgentToken(dir, "claude"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, AgentFileName)); !os.IsNotExist(err) {
		t.Fatal("file should be gone once the last token is revoked")
	}
	if err := RevokeAgentToken(dir, "claude"); err == nil {
		t.Fatal("revoking a missing token should fail")
	}
}

func TestValidAgentName(t *testing.T) {
	for _, ok := range []string{"claude", "ci-bot", "a.b_c", "A1"} {
		if err := ValidAgentName(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-x", "has space", "semi;colon", "a/b", strings.Repeat("a", 41)} {
		if err := ValidAgentName(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

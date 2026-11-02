package web

import (
	"os/exec"
	"strings"
	"testing"
)

// TestJavaScriptUISuite runs index_ui_test.js (Node's built-in test runner) as
// part of `go test ./...`, so the JavaScript tests can't drift out of date
// unnoticed. It is skipped when Node isn't installed.
func TestJavaScriptUISuite(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed; skipping the JavaScript UI tests")
	}
	cmd := exec.Command(node, "web/index_ui_test.js")
	cmd.Dir = ".." // the suite reads web/index.html relative to the repo root
	out, err := cmd.CombinedOutput()
	if err != nil {
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		if len(lines) > 60 {
			lines = lines[len(lines)-60:]
		}
		t.Fatalf("JavaScript UI tests failed: %v\n%s", err, strings.Join(lines, "\n"))
	}
}

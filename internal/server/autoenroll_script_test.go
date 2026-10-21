package server

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The SecurityAgent dialog's button is "Enrol" on non-US English guests; the
// script matched only "Enroll" and never clicked it.
func TestAutoEnrollScriptClicksBothEnrolSpellings(t *testing.T) {
	for _, label := range []string{`"Enroll"`, `"Enrol"`} {
		if !strings.Contains(autoEnrollScript, label) {
			t.Errorf("autoEnrollScript does not try the %s button label", label)
		}
	}
}

func TestAutoEnrollScriptIsValidBash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "enroll.sh")
	if err := os.WriteFile(path, []byte(autoEnrollScript), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("/bin/bash", "-n", path).CombinedOutput(); err != nil {
		t.Fatalf("bash -n: %v\n%s", err, out)
	}
}

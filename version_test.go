package tartoven

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestReleaseVersionIsConsistent(t *testing.T) {
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	changelog, err := os.ReadFile("CHANGELOG.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(readme), fmt.Sprintf("Current release: **%s**", Version)) {
		t.Fatal("README release mismatch")
	}
	if !strings.Contains(string(changelog), fmt.Sprintf("## %s", Version)) {
		t.Fatal("CHANGELOG release missing")
	}
}

func TestReleaseBinaryReportsVersion(t *testing.T) {
	output, err := exec.Command("./tart-oven", "-version").CombinedOutput()
	if err != nil {
		t.Fatalf("run tracked executable: %v\n%s", err, output)
	}
	if got, want := string(output), Version+"\n"; got != want {
		t.Fatalf("tracked executable version output = %q, want %q", got, want)
	}
}

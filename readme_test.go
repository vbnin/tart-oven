package tartoven

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestReadmeContainsOnboardingAndSafetyEssentials(t *testing.T) {
	b, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	content := string(b)

	required := []string{
		"## Screenshots",
		"## Prerequisites",
		"## Quick start",
		"## Basic usage",
		"## Configuration",
		"## Jamf and MDM",
		"## Automation",
		"## Security",
		"## Troubleshooting",
		"## Build from source",
		fmt.Sprintf("TartOven-%s.pkg", Version),
		"/Library/LaunchAgents/com.tartoven.agent.plist",
		"http://127.0.0.1:9000",
		"At least 25 GiB free",
		"Random MAC",
		"Random serial",
		"The response only confirms that Tart Oven accepted the request",
		"### Access token",
		"### HTTPS",
		"Tart Oven keeps only a hash",
		"go test ./... && node web/index_ui_test.js",
	}

	for _, text := range required {
		if !strings.Contains(content, text) {
			t.Errorf("README.md missing required onboarding or safety text: %s", text)
		}
	}

	if strings.Contains(content, "0.0.0.0:9000") && !strings.Contains(content, "Do not expose `0.0.0.0:9000`") {
		t.Error("README.md must not mention a wildcard listen address without warning against exposing it")
	}
}

// Package update checks GitHub for newer Tart and Tart Oven releases.
package update

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	tartoven "tart-oven"
)

const (
	TartReleaseAPI  = "https://api.github.com/repos/openai/tart/releases/latest"
	OvenReleaseAPI  = "https://api.github.com/repos/vbnin/tart-oven/releases/latest"
	OvenReleasePage = "https://github.com/vbnin/tart-oven/releases/latest"
	CheckInterval   = 24 * time.Hour
)

var releaseHTTPClient = &http.Client{Timeout: 15 * time.Second}

// ReleaseFetcher returns the latest release tag behind a GitHub API URL.
type ReleaseFetcher func(url string) (string, error)

// View is one "newer version available" entry in the state snapshot.
type View struct {
	Latest    string `json:"latest,omitempty"`
	Available bool   `json:"available"`
	URL       string `json:"url,omitempty"`
}

// Views holds the Tart and Tart Oven entries.
type Views struct {
	Tart View `json:"tart"`
	Oven View `json:"oven"`
}

// FetchLatestRelease returns the tag of the latest published (non-draft,
// non-prerelease) GitHub release behind a /releases/latest API URL.
func FetchLatestRelease(url string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "tart-oven/"+tartoven.Version)
	resp, err := releaseHTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: %s", url, resp.Status)
	}
	var body struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	if strings.TrimSpace(body.TagName) == "" {
		return "", fmt.Errorf("%s: no tag_name", url)
	}
	return strings.TrimSpace(body.TagName), nil
}

var versionPattern = regexp.MustCompile(`v?(\d+(?:\.\d+)*)(-[0-9A-Za-z.]+)?`)

// ParseVersion pulls the first dotted version out of s ("tart 2.35.0",
// "v1.54", "1.55-dev19"). pre is the "-dev19" style suffix, if any.
func ParseVersion(s string) (parts []int, pre string, ok bool) {
	match := versionPattern.FindStringSubmatch(s)
	if match == nil {
		return nil, "", false
	}
	for _, p := range strings.Split(match[1], ".") {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, "", false
		}
		parts = append(parts, n)
	}
	return parts, match[2], true
}

// Newer reports whether latest is a newer release than current. A
// pre-release such as 1.55-dev19 is older than 1.55. Unparsable versions are
// never reported as newer.
func Newer(latest, current string) bool {
	l, lpre, lok := ParseVersion(latest)
	c, cpre, cok := ParseVersion(current)
	if !lok || !cok {
		return false
	}
	for i := 0; i < len(l) || i < len(c); i++ {
		var a, b int
		if i < len(l) {
			a = l[i]
		}
		if i < len(c) {
			b = c[i]
		}
		if a != b {
			return a > b
		}
	}
	return lpre == "" && cpre != ""
}

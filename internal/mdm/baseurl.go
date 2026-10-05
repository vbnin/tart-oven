package mdm

import (
	"errors"
	"net/url"
	"strings"
)

// NormalizeJamfBaseURL validates and normalizes a Jamf Pro base URL.
func NormalizeJamfBaseURL(raw string) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return "", errors.New("Jamf Pro base URL must be an http or https URL with a hostname")
	}
	return raw, nil
}

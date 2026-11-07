package mdm

import (
	"errors"
	"net/url"
	"strings"
)

// NormalizeJamfBaseURL validates and normalizes a Jamf server base URL. The
// shape is the same for Jamf Pro and Jamf School.
func NormalizeJamfBaseURL(raw string) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return "", errors.New("Jamf server base URL must be an http or https URL with a hostname")
	}
	return raw, nil
}

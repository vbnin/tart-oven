// Package ipsw lists macOS restore images that a Tart virtual machine can be
// created from, using the AppleDB API (github.com/littlebyteorg/appledb).
package ipsw

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// FeedURL is AppleDB's macOS firmware list (the gzip form is ~10x smaller).
const FeedURL = "https://api.appledb.dev/ios/macOS/main.json.gz"

// virtualMac is AppleDB's device identifier for Virtualization.framework guests.
const virtualMac = "VirtualMac2,1"

// Entry is one downloadable restore image.
type Entry struct {
	Version  string `json:"version"`  // "27.0.1", "27.2 beta 2", "27.0 RC"
	Build    string `json:"build"`    // "26A434"
	Released string `json:"released"` // YYYY-MM-DD
	Beta     bool   `json:"beta"`
	RC       bool   `json:"rc"`
	Size     int64  `json:"size"` // bytes
	URL      string `json:"url"`

	// Set by Annotate: the image is already in Tart's download cache.
	Downloaded bool   `json:"downloaded"`
	Path       string `json:"path,omitempty"` // where the cached file is
}

type feedEntry struct {
	Version  string          `json:"version"`
	Build    string          `json:"build"`
	Released string          `json:"released"`
	Beta     bool            `json:"beta"`
	RC       bool            `json:"rc"`
	Signed   json.RawMessage `json:"signed"` // true, or the device list Apple still signs
	Sources  []feedSource    `json:"sources"`
}

type feedSource struct {
	Type      string     `json:"type"`
	DeviceMap []string   `json:"deviceMap"`
	Size      int64      `json:"size"`
	Links     []feedLink `json:"links"`
}

type feedLink struct {
	URL       string `json:"url"`
	Active    bool   `json:"active"`
	Preferred bool   `json:"preferred"`
}

// Fetch downloads and parses the feed.
func Fetch(ctx context.Context, client *http.Client, url string) ([]Entry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "tart-oven")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	return Parse(resp.Body)
}

// DefaultClient suits the feed: about 5 MB compressed, so five minutes covers a
// link as slow as 150 kbit/s without letting it hang forever.
func DefaultClient() *http.Client { return &http.Client{Timeout: 5 * time.Minute} }

// Parse reads the feed (gzip or plain JSON) and returns the signed, active
// https restore images for virtual Macs, newest first. The raw feed is tens of
// MB, so it is decoded one firmware at a time.
func Parse(r io.Reader) ([]Entry, error) {
	br := bufio.NewReader(r)
	if magic, err := br.Peek(2); err == nil && magic[0] == 0x1f && magic[1] == 0x8b {
		zr, err := gzip.NewReader(br)
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		br = bufio.NewReader(zr)
	}
	dec := json.NewDecoder(br)
	if tok, err := dec.Token(); err != nil || tok != json.Delim('[') {
		return nil, fmt.Errorf("unexpected feed format")
	}
	byURL := map[string]Entry{}
	for dec.More() {
		var fe feedEntry
		if err := dec.Decode(&fe); err != nil {
			return nil, err
		}
		e, ok := fe.entry()
		if !ok {
			continue
		}
		// The same image is listed again under its release-candidate name;
		// keep the final release's entry.
		if old, dup := byURL[e.URL]; dup && !old.RC {
			continue
		}
		byURL[e.URL] = e
	}
	out := make([]Entry, 0, len(byURL))
	for _, e := range byURL {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Released != out[j].Released {
			return out[i].Released > out[j].Released
		}
		return out[i].Version > out[j].Version
	})
	return out, nil
}

// entry converts a firmware into an Entry, when a virtual Mac can install it.
func (fe feedEntry) entry() (Entry, bool) {
	if fe.Build == "" || !fe.signedForVirtualMac() {
		return Entry{}, false
	}
	for _, s := range fe.Sources {
		if s.Type != "ipsw" || !contains(s.DeviceMap, virtualMac) {
			continue
		}
		if url := bestLink(s.Links); url != "" {
			return Entry{Version: fe.Version, Build: fe.Build, Released: fe.Released,
				Beta: fe.Beta, RC: fe.RC, Size: s.Size, URL: url}, true
		}
	}
	return Entry{}, false
}

// signedForVirtualMac reports whether Apple still signs the build for virtual
// Macs; an unsigned image fails to install.
func (fe feedEntry) signedForVirtualMac() bool {
	var all bool
	if json.Unmarshal(fe.Signed, &all) == nil {
		return all
	}
	var devices []string
	return json.Unmarshal(fe.Signed, &devices) == nil && contains(devices, virtualMac)
}

// bestLink picks an active https download, preferring the feed's preferred one.
func bestLink(links []feedLink) string {
	best := ""
	for _, l := range links {
		if !l.Active || !strings.HasPrefix(l.URL, "https://") {
			continue
		}
		if l.Preferred {
			return l.URL
		}
		if best == "" {
			best = l.URL
		}
	}
	return best
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

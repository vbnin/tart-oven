package ipsw

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Annotate returns entries with Downloaded and Path set for every restore
// image already in dir, Tart's IPSW download cache (<TART_HOME>/cache/IPSWs).
//
// A cached file counts when it is named after the entry (the SHA-256 of its
// URL, or the URL's file name) or, failing that, when it is the exact size
// AppleDB lists for the entry. A named file whose size disagrees with AppleDB
// is treated as an unfinished download. A missing or unreadable dir leaves
// every entry as it was.
func Annotate(dir string, entries []Entry) []Entry {
	files, err := os.ReadDir(dir)
	if err != nil {
		return entries
	}
	sizes := map[string]int64{}  // lower-case name -> size
	names := map[string]string{} // lower-case name -> real name
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(strings.ToLower(f.Name()), ".ipsw") {
			continue
		}
		info, err := f.Info()
		if err != nil {
			continue
		}
		key := strings.ToLower(f.Name())
		sizes[key], names[key] = info.Size(), f.Name()
	}
	if len(sizes) == 0 {
		return entries
	}

	out := make([]Entry, len(entries))
	copy(out, entries)
	for i, e := range out {
		if name := cachedName(e, sizes, names); name != "" {
			out[i].Downloaded, out[i].Path = true, filepath.Join(dir, name)
		}
	}
	return out
}

func cachedName(e Entry, sizes map[string]int64, names map[string]string) string {
	sum := sha256.Sum256([]byte(e.URL))
	candidates := []string{hex.EncodeToString(sum[:]) + ".ipsw"}
	if u, err := url.Parse(e.URL); err == nil {
		if base := path.Base(u.Path); strings.HasSuffix(strings.ToLower(base), ".ipsw") {
			candidates = append(candidates, base)
		}
	}
	for _, c := range candidates {
		key := strings.ToLower(c)
		if size, ok := sizes[key]; ok && (e.Size == 0 || size == e.Size) {
			return names[key]
		}
	}
	if e.Size > 0 {
		for key, size := range sizes {
			if size == e.Size {
				return names[key]
			}
		}
	}
	return ""
}

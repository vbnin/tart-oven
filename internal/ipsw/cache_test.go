package ipsw

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func writeSized(t *testing.T, dir, name string, size int) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAnnotateMarksCachedImages(t *testing.T) {
	dir := t.TempDir()
	hashed := Entry{Version: "26.0", URL: "https://updates.cdn-apple.com/a/UniversalMac_26.0_Restore.ipsw", Size: 100}
	byName := Entry{Version: "15.6", URL: "https://updates.cdn-apple.com/b/UniversalMac_15.6_Restore.ipsw", Size: 200}
	bySize := Entry{Version: "14.7", URL: "https://example.test/download?id=7", Size: 300}
	partial := Entry{Version: "13.6", URL: "https://example.test/c/UniversalMac_13.6_Restore.ipsw", Size: 400}
	missing := Entry{Version: "12.7", URL: "https://example.test/d/UniversalMac_12.7_Restore.ipsw", Size: 500}

	sum := sha256.Sum256([]byte(hashed.URL))
	writeSized(t, dir, hex.EncodeToString(sum[:])+".ipsw", 100)
	writeSized(t, dir, "UniversalMac_15.6_Restore.ipsw", 200)
	writeSized(t, dir, "something-else.IPSW", 300)
	writeSized(t, dir, "UniversalMac_13.6_Restore.ipsw", 10) // unfinished: wrong size
	writeSized(t, dir, "notes.txt", 500)                     // not an image

	in := []Entry{hashed, byName, bySize, partial, missing}
	out := Annotate(dir, in)

	want := []bool{true, true, true, false, false}
	for i, e := range out {
		if e.Downloaded != want[i] {
			t.Errorf("%s: Downloaded = %v, want %v", e.Version, e.Downloaded, want[i])
		}
		if e.Downloaded && (e.Path == "" || filepath.Dir(e.Path) != dir) {
			t.Errorf("%s: Path = %q", e.Version, e.Path)
		}
		if !e.Downloaded && e.Path != "" {
			t.Errorf("%s: unexpected Path %q", e.Version, e.Path)
		}
	}
	if in[0].Downloaded {
		t.Error("Annotate changed its input")
	}
}

func TestAnnotateToleratesMissingDir(t *testing.T) {
	in := []Entry{{Version: "26.0", URL: "https://x/y.ipsw", Size: 1}}
	out := Annotate(filepath.Join(t.TempDir(), "nope"), in)
	if len(out) != 1 || out[0].Downloaded {
		t.Fatalf("out = %+v", out)
	}
}

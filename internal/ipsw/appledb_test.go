package ipsw

import (
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const sampleFeed = `[
 {"version":"27.0.1","build":"26A434","released":"2026-09-28","signed":["Mac17,2","VirtualMac2,1"],
  "sources":[{"type":"ota","deviceMap":["VirtualMac2,1"],"links":[{"url":"https://x/ota.aea","active":true,"preferred":true}]},
             {"type":"ipsw","deviceMap":["Mac17,2","VirtualMac2,1"],"size":26637307067,
              "links":[{"url":"http://x/a.ipsw","active":true},{"url":"https://x/a.ipsw","active":true,"preferred":true}]}]},
 {"version":"26.2 RC","build":"25C56","released":"2025-12-03","rc":true,"signed":true,
  "sources":[{"type":"ipsw","deviceMap":["VirtualMac2,1"],"size":5,"links":[{"url":"https://x/b.ipsw","active":true}]}]},
 {"version":"26.2","build":"25C56","released":"2025-12-12","signed":true,
  "sources":[{"type":"ipsw","deviceMap":["VirtualMac2,1"],"size":5,"links":[{"url":"https://x/b.ipsw","active":true}]}]},
 {"version":"27.2 beta","build":"26B5086k","released":"2026-09-16","beta":true,"signed":["VirtualMac2,1"],
  "sources":[{"type":"ipsw","deviceMap":["VirtualMac2,1"],"size":9,"links":[{"url":"https://x/c.ipsw","active":true}]}]},
 {"version":"15.0","build":"24A335","released":"2024-09-16",
  "sources":[{"type":"ipsw","deviceMap":["VirtualMac2,1"],"size":9,"links":[{"url":"https://x/unsigned.ipsw","active":true}]}]},
 {"version":"14.0","build":"23A344","released":"2023-09-26","signed":["Mac14,2"],
  "sources":[{"type":"ipsw","deviceMap":["Mac14,2"],"size":9,"links":[{"url":"https://x/no-vm.ipsw","active":true}]}]},
 {"version":"13.0","build":"22A380","released":"2022-10-24","signed":true,
  "sources":[{"type":"ipsw","deviceMap":["VirtualMac2,1"],"size":9,"links":[{"url":"https://x/dead.ipsw","active":false}]}]}
]`

func TestParseKeepsSignedActiveVirtualMacImages(t *testing.T) {
	got, err := Parse(strings.NewReader(sampleFeed))
	if err != nil {
		t.Fatal(err)
	}
	var versions []string
	for _, e := range got {
		versions = append(versions, e.Version)
	}
	// Newest first; the RC duplicate, unsigned, non-VM and inactive entries are dropped.
	want := []string{"27.0.1", "27.2 beta", "26.2"}
	if strings.Join(versions, "|") != strings.Join(want, "|") {
		t.Fatalf("versions = %v, want %v", versions, want)
	}
	first := got[0]
	if first.URL != "https://x/a.ipsw" || first.Size != 26637307067 || first.Build != "26A434" {
		t.Fatalf("first = %+v", first)
	}
	if got[2].RC || !got[1].Beta {
		t.Fatalf("flags wrong: %+v", got)
	}
}

func TestParseAcceptsGzip(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write([]byte(sampleFeed))
	zw.Close()
	got, err := Parse(&buf)
	if err != nil || len(got) != 3 {
		t.Fatalf("got %d entries, err %v", len(got), err)
	}
}

func TestParseRejectsNonArray(t *testing.T) {
	if _, err := Parse(strings.NewReader(`{"not":"a list"}`)); err == nil {
		t.Fatal("expected an error")
	}
}

func TestFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(sampleFeed))
	}))
	defer srv.Close()
	got, err := Fetch(context.Background(), srv.Client(), srv.URL)
	if err != nil || len(got) != 3 {
		t.Fatalf("got %d entries, err %v", len(got), err)
	}
	if _, err := Fetch(context.Background(), srv.Client(), srv.URL+"/missing"); err == nil {
		t.Fatal("expected an error for a 404")
	}
}

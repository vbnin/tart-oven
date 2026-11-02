package update

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewer(t *testing.T) {
	cases := []struct {
		latest, current string
		want            bool
	}{
		{"2.36.0", "2.35.0", true},
		{"2.35.0", "2.35.0", false},
		{"2.35.0", "2.36.1", false},
		{"2.35.1", "tart 2.35.0", true},
		{"v1.55", "1.55-dev19", true},
		{"v1.54", "1.55-dev19", false},
		{"v1.55", "1.55", false},
		{"v1.55.1", "1.55", true},
		{"v1.56-dev1", "1.55", true},
		{"garbage", "1.55", false},
		{"2.36.0", "", false},
	}
	for _, c := range cases {
		if got := Newer(c.latest, c.current); got != c.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.latest, c.current, got, c.want)
		}
	}
}

func TestFetchLatestReleaseReadsTagName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" {
			t.Error("missing User-Agent")
		}
		w.Write([]byte(`{"tag_name":"v9.9","name":"ignored"}`))
	}))
	defer srv.Close()
	got, err := FetchLatestRelease(srv.URL)
	if err != nil || got != "v9.9" {
		t.Fatalf("FetchLatestRelease = %q, %v", got, err)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", http.StatusForbidden)
	}))
	defer bad.Close()
	if _, err := FetchLatestRelease(bad.URL); err == nil {
		t.Fatal("expected an error for a non-200 response")
	}
}

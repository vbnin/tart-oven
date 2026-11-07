package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func postConfig(t *testing.T, m *Manager, body string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	m.handleConfig(rr, httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(body)))
	return rr
}

// A School profile holds a private key, so a plain http server is refused when the
// profile is saved, with a message that names the problem, not at the first copy.
func TestConfigRefusesAPlainHTTPJamfSchoolServer(t *testing.T) {
	m := newTestManager(t)
	for _, c := range []struct {
		name, body string
		wantCode   int
	}{
		{"School over http", `{"jamfProfiles":[{"id":"s","kind":"school","name":"S","baseUrl":"http://school.example","network":"n","location":"l"}]}`, http.StatusBadRequest},
		{"School over https", `{"jamfProfiles":[{"id":"s","kind":"school","name":"S","baseUrl":"https://school.example","network":"n","location":"l"}]}`, http.StatusOK},
		{"Pro over http is unchanged", `{"jamfProfiles":[{"id":"p","kind":"pro","name":"P","baseUrl":"http://jamf.example","invitationCode":"x"}]}`, http.StatusOK},
	} {
		rr := postConfig(t, m, c.body)
		if rr.Code != c.wantCode {
			t.Errorf("%s: status %d (%s), want %d", c.name, rr.Code, strings.TrimSpace(rr.Body.String()), c.wantCode)
		}
		if c.wantCode == http.StatusBadRequest && !strings.Contains(rr.Body.String(), "https") {
			t.Errorf("%s: the message should say https is required: %q", c.name, rr.Body.String())
		}
	}
}

// A client that omits the kind keeps the stored one, and so must not slip an http
// URL onto a stored School profile.
func TestConfigRefusesAnHTTPUrlOnAStoredSchoolProfileWhenTheKindIsOmitted(t *testing.T) {
	m := newTestManager(t)
	m.cfg.JamfProfiles = []JamfProfile{{ID: "s", Kind: JamfKindSchool, Name: "S", BaseURL: "https://school.example", Network: "n", Location: "l"}}

	rr := postConfig(t, m, `{"jamfProfiles":[{"id":"s","name":"S","baseUrl":"http://school.example"}]}`)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", rr.Code)
	}
}

// A Manager without a state file (every unit-test fixture) must not scatter a
// ".tmp" file into the directory the tests run in.
func TestSaveWithoutAStatePathWritesNothing(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	m := &Manager{cfg: defaultConfig(), vms: map[string]*VM{}}

	m.save()

	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("save() wrote %v into the working directory", entries)
	}
	_ = filepath.Join // keep the import set stable if the test grows
}

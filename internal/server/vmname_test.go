package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func takenSet(names ...string) func(string) bool {
	set := map[string]bool{}
	for _, n := range names {
		set[n] = true
	}
	return func(n string) bool { return set[n] }
}

func TestExpandNameTemplate(t *testing.T) {
	for _, c := range []struct {
		tpl  string
		num  int
		want string
	}{
		{"demo-$AUTONUM", 3, "demo-3"},
		{"$RAND8", 0, "AB12CD34"},
		{"mac-$RAND8-$AUTONUM", 7, "mac-AB12CD34-7"},
		{"$AUTONUM$AUTONUM", 2, "22"},
		{"$RAND8_vm", 0, "AB12CD34_vm"},
		{"plain", 0, "plain"},
		{"cost$5", 0, "cost$5"},
	} {
		if got := expandNameTemplate(c.tpl, c.num, "AB12CD34"); got != c.want {
			t.Errorf("expand(%q, %d) = %q, want %q", c.tpl, c.num, got, c.want)
		}
	}
}

func TestValidateNameTemplate(t *testing.T) {
	for _, ok := range []string{"", "  ", "$RAND8", "demo-$AUTONUM", "My_VM-$RAND8-$AUTONUM", "plain", "cost$5", "a b"} {
		if err := validateNameTemplate(ok); err != nil {
			t.Errorf("validateNameTemplate(%q) = %v", ok, err)
		}
	}
	for bad, wantErr := range map[string]string{
		"demo-$AUTONUMBER":       "$AUTONUMBER",
		"$FOO":                   "$FOO",
		"$rand8x$BAR":            "$BAR",
		"a/b":                    "/",
		"host:1":                 ":",
		"-rf":                    "start with",
		".hidden":                "start with",
		"../x":                   "can't contain",
		strings.Repeat("a", 120): "longer",
		"tab\tname":              "control",
	} {
		err := validateNameTemplate(bad)
		if err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Errorf("validateNameTemplate(%q) = %v, want error containing %q", bad, err, wantErr)
		}
	}
}

func newTestNamer(template string, taken ...string) *vmNamer {
	n := newVMNamer(template, takenSet(taken...))
	n.rand8 = func() string { return "AB12CD34" }
	return n
}

func TestNamerAutoNumIncrementsAndSkipsTakenNames(t *testing.T) {
	n := newTestNamer("demo-$AUTONUM", "demo-2")
	var got []string
	for i := 0; i < 3; i++ {
		name, err := n.next()
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, name)
	}
	if strings.Join(got, ",") != "demo-1,demo-3,demo-4" {
		t.Fatalf("names = %v", got)
	}
}

func TestNamerAppendsSuffixOnDuplicates(t *testing.T) {
	n := newTestNamer("demo", "demo", "demo-1")
	name, _ := n.next()
	if name != "demo-2" {
		t.Fatalf("name = %q, want demo-2", name)
	}
	n = newTestNamer("fresh")
	if name, _ := n.next(); name != "fresh" {
		t.Fatalf("name = %q, want fresh", name)
	}
}

func TestNamerBlankTemplateIsRand8(t *testing.T) {
	n := newTestNamer("  ")
	if name, _ := n.next(); name != "AB12CD34" {
		t.Fatalf("name = %q", name)
	}
	// Same random value again would collide: it gets a suffix.
	n = newTestNamer("$RAND8", "AB12CD34")
	if name, _ := n.next(); name != "AB12CD34-1" {
		t.Fatalf("name = %q", name)
	}
}

func TestNamerRand8AndAutoNumTogether(t *testing.T) {
	n := newTestNamer("vm-$RAND8-$AUTONUM")
	first, _ := n.next()
	second, _ := n.next()
	if first != "vm-AB12CD34-1" || second != "vm-AB12CD34-2" {
		t.Fatalf("names = %q, %q", first, second)
	}
}

func TestNamerRejectsInvalidNames(t *testing.T) {
	if _, err := newTestNamer("a/$AUTONUM").next(); err == nil {
		t.Fatal("accepted a name with a slash")
	}
}

func TestDefaultRand8LooksLikeToday(t *testing.T) {
	for i := 0; i < 50; i++ {
		id := shortID()
		if len(id) != 8 || id != strings.ToUpper(id) || strings.Trim(id, "0123456789ABCDEF") != "" {
			t.Fatalf("shortID() = %q", id)
		}
	}
}

func TestAllocVMNameAvoidsKnownReservedRunningAndOnDiskNames(t *testing.T) {
	m := newTestManager(t)
	dir := t.TempDir()
	m.cfg.VMStoragePath = dir
	os.MkdirAll(filepath.Join(dir, "vms", "lab-2"), 0o755) // created by tart, not yet in m.vms
	m.vms["lab-1"] = &VM{Name: "lab-1"}
	m.tasks = append(m.tasks, &Task{Target: "lab-3", Status: "running"}, &Task{Target: "lab-4", Status: "success"})

	a, err := m.allocVMName(newVMNamer("lab-$AUTONUM", nil))
	if err != nil || a != "lab-4" {
		t.Fatalf("first = %q, %v (lab-1..3 are taken; lab-4's task is finished)", a, err)
	}
	// A second batch running at the same time must not get the reserved name.
	b, err := m.allocVMName(newVMNamer("lab-$AUTONUM", nil))
	if err != nil || b != "lab-5" {
		t.Fatalf("second = %q, %v", b, err)
	}
	m.releaseVMName(a)
	c, _ := m.allocVMName(newVMNamer("lab-$AUTONUM", nil))
	if c != "lab-4" {
		t.Fatalf("after release = %q, want lab-4", c)
	}
}

func postCreate(t *testing.T, m *Manager, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	m.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/vm/create", strings.NewReader(body)))
	return rec
}

func TestCreateEndpointRejectsBadNamesAndProfiles(t *testing.T) {
	m := newTestManager(t)
	m.cfg.JamfProfiles = []JamfProfile{{ID: "prod", Name: "Production"}}
	for body, want := range map[string]string{
		`{"mode":"clone","source":"base","nameTemplate":"vm-$AUTONUMBER"}`:               "$AUTONUMBER",
		`{"mode":"clone","source":"base","nameTemplate":"a/b"}`:                          "/",
		`{"mode":"clone","source":"base","prefix":"../x"}`:                               "/",
		`{"mode":"clone","source":"base","nameTemplate":"ok","autoEnrollProfile":"zzz"}`: "unknown Jamf server profile",
	} {
		rec := postCreate(t, m, body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("%s → %d %q, want 400 containing %q", body, rec.Code, rec.Body.String(), want)
		}
	}
}

func TestCreateEndpointAcceptsBlankTemplate(t *testing.T) {
	m := newTestManager(t)
	rec := postCreate(t, m, `{"mode":"clone","source":"","nameTemplate":""}`)
	// Only the missing source should be reported, not the blank template.
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "clone requires a source VM") {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
}

func TestIconIsServed(t *testing.T) {
	m := newTestManager(t)
	rec := httptest.NewRecorder()
	m.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/icon.png", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("status %d, type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if body := rec.Body.Bytes(); len(body) < 1000 || string(body[1:4]) != "PNG" {
		t.Fatalf("not a PNG (%d bytes)", len(body))
	}
}

func TestRenameExpandsNameTemplates(t *testing.T) {
	m := newTestManager(t)
	m.vms["base"] = &VM{Name: "base", State: "stopped"}
	post := func(body string) map[string]string {
		rec := httptest.NewRecorder()
		m.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/vm/rename", strings.NewReader(body)))
		var out map[string]string
		json.Unmarshal(rec.Body.Bytes(), &out)
		return out
	}
	if out := post(`{"name":"base","newName":"vm-$AUTONUMBER"}`); !strings.Contains(out["error"], "$AUTONUMBER") {
		t.Errorf("typo not reported: %v", out)
	}
	if out := post(`{"name":"base","newName":"a/b"}`); out["error"] == "" {
		t.Errorf("slash accepted: %v", out)
	}
	if out := post(`{"name":"base","newName":"base"}`); out["error"] != "" || out["name"] != "base" {
		t.Errorf("renaming to the same name should be a no-op: %v", out)
	}
}

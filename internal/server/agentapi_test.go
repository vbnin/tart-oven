package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tart-oven/internal/auth"
)

// fakeTart stands in for the tart binary: VMs are directories under
// $TART_HOME/vms, `exec` runs the command locally, and `run`/`ip` fail so a
// boot never succeeds.
const fakeTart = `#!/bin/sh
case "$1" in
  list)
    printf '['; sep=''
    for d in "$TART_HOME"/vms/*/; do
      [ -d "$d" ] || continue
      printf '%s{"Source":"local","Name":"%s","Disk":50,"Size":10,"Accessed":"now","State":"stopped","Running":false}' "$sep" "$(basename "$d")"
      sep=','
    done
    printf ']\n';;
  clone) mkdir -p "$TART_HOME/vms/$3";;
  delete) rm -rf "$TART_HOME/vms/$2";;
  set|stop) ;;
  exec) shift; shift; exec "$@";;
  *) echo "fake tart: unsupported: $*" >&2; exit 1;;
esac
`

func newAgentTestManager(t *testing.T) (*Manager, http.Handler) {
	t.Helper()
	m := newTestManager(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "tart")
	if err := os.WriteFile(bin, []byte(fakeTart), 0o755); err != nil {
		t.Fatal(err)
	}
	m.cfg.TartAppPath = bin
	m.cfg.VMStoragePath = filepath.Join(dir, "home")
	m.cfg.SharedDir = filepath.Join(dir, "shared")
	m.cfg.BootTimeoutSec = 1
	m.cfg.AgentEnabled = true
	m.cfg.AgentTemplates = []string{"base-TEMPLATE"}
	m.cfg.AgentMaxTTLMin = 120
	m.tartJSON = true
	if err := os.MkdirAll(filepath.Join(m.cfg.VMStoragePath, "vms", "base-TEMPLATE"), 0o755); err != nil {
		t.Fatal(err)
	}
	m.vms["base-TEMPLATE"] = &VM{Name: "base-TEMPLATE", State: "stopped"}
	return m, m.handler()
}

func decode(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("bad JSON %q: %v", rec.Body.String(), err)
	}
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var e struct {
		Error struct{ Code string } `json:"error"`
	}
	decode(t, rec, &e)
	return e.Error.Code
}

func TestAgentAPIOffByDefault(t *testing.T) {
	m, h := newAgentTestManager(t)
	m.cfg.AgentEnabled = false
	for _, p := range []string{"/api/agent/info", "/api/agent/vms", "/api/agent/guide"} {
		rec := do(h, "GET", p, "", nil)
		if rec.Code != http.StatusForbidden || errCode(t, rec) != "agent_api_disabled" {
			t.Fatalf("%s = %d %s", p, rec.Code, rec.Body)
		}
	}
}

func TestAgentTokenOnlyReachesAgentAPI(t *testing.T) {
	m, h := newAgentTestManager(t)
	admin, _ := auth.NewToken()
	if err := m.auth().set(auth.Hash(admin)); err != nil {
		t.Fatal(err)
	}
	agent, err := auth.AddAgentToken(m.auth().dir, "claude")
	if err != nil {
		t.Fatal(err)
	}
	m.auth().invalidateAgentTokens()
	bearer := func(tok string) map[string]string { return map[string]string{"Authorization": "Bearer " + tok} }

	if rec := do(h, "GET", "/api/agent/info", "", bearer(agent)); rec.Code != 200 {
		t.Fatalf("agent token on /api/agent/info = %d %s", rec.Code, rec.Body)
	}
	for _, p := range []struct{ method, path string }{
		{"GET", "/api/vms"}, {"GET", "/api/config"}, {"POST", "/api/vm/delete"},
		{"POST", "/api/exec"}, {"POST", "/api/server/stop"}, {"POST", "/api/auth/token"},
		{"GET", "/api/auth/agent-tokens"}, {"POST", "/api/auth/agent-tokens"}, {"GET", "/events"},
	} {
		if rec := do(h, p.method, p.path, "{}", bearer(agent)); rec.Code != http.StatusForbidden {
			t.Errorf("agent token on %s %s = %d, want 403", p.method, p.path, rec.Code)
		}
	}
	if rec := do(h, "GET", "/api/agent/info", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token = %d", rec.Code)
	}
	if rec := do(h, "GET", "/api/agent/info", "", bearer("ta_wrong")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token = %d", rec.Code)
	}
	// The agent token must not open a dashboard session either.
	if rec := do(h, "POST", "/api/auth/login", `{"token":"`+agent+`"}`, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("agent token logged in = %d", rec.Code)
	}
	var st struct{ Authenticated bool }
	decode(t, do(h, "GET", "/api/auth/status", "", bearer(agent)), &st)
	if st.Authenticated {
		t.Fatal("/api/auth/status reports an agent token as fully authenticated")
	}
	if rec := do(h, "GET", "/api/config", "", bearer(admin)); rec.Code != 200 {
		t.Fatalf("admin token = %d", rec.Code)
	}
	// Revoking the dashboard token takes the agent tokens with it.
	if err := m.auth().set(""); err != nil {
		t.Fatal(err)
	}
	if toks, _ := auth.LoadAgentTokens(m.auth().dir); len(toks) != 0 {
		t.Fatalf("agent tokens survived revoking the dashboard token: %v", toks)
	}
}

func TestAgentTokenManagementRoutes(t *testing.T) {
	m, h := newAgentTestManager(t)
	if rec := do(h, "POST", "/api/auth/agent-tokens", `{"name":"x"}`, nil); rec.Code != http.StatusConflict {
		t.Fatalf("creating without a dashboard token = %d, want 409", rec.Code)
	}
	admin, _ := auth.NewToken()
	m.auth().set(auth.Hash(admin))
	hdr := map[string]string{"Authorization": "Bearer " + admin}
	rec := do(h, "POST", "/api/auth/agent-tokens", `{"name":"bot"}`, hdr)
	var created struct{ Token string }
	decode(t, rec, &created)
	if rec.Code != 200 || !strings.HasPrefix(created.Token, "ta_") {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	if do(h, "POST", "/api/auth/agent-tokens", `{"name":"bad name"}`, hdr).Code != http.StatusBadRequest {
		t.Fatal("invalid name accepted")
	}
	var list struct{ Tokens []struct{ ID, Name string } }
	decode(t, do(h, "GET", "/api/auth/agent-tokens", "", hdr), &list)
	if len(list.Tokens) != 1 || list.Tokens[0].Name != "bot" {
		t.Fatalf("list = %+v", list)
	}
	if strings.Contains(do(h, "GET", "/api/auth/agent-tokens", "", hdr).Body.String(), created.Token) {
		t.Fatal("list leaked the token")
	}
	if rec := do(h, "DELETE", "/api/auth/agent-tokens/"+list.Tokens[0].ID, "", hdr); rec.Code != 200 {
		t.Fatalf("revoke = %d", rec.Code)
	}
	if rec := do(h, "GET", "/api/agent/info", "", map[string]string{"Authorization": "Bearer " + created.Token}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked token still works: %d", rec.Code)
	}
}

func TestAgentCreateValidation(t *testing.T) {
	m, h := newAgentTestManager(t)
	create := func(body string) *httptest.ResponseRecorder { return do(h, "POST", "/api/agent/vms", body, nil) }

	cases := []struct {
		name, body, code string
		status           int
	}{
		{"missing template", `{}`, "invalid_request", 400},
		{"unknown field", `{"template":"base-TEMPLATE","ttl":5}`, "invalid_request", 400},
		{"not allowed", `{"template":"some-other-vm"}`, "template_not_allowed", 403},
		{"bad ttl", `{"template":"base-TEMPLATE","ttlMinutes":9999}`, "invalid_ttl", 400},
		{"bad cpu", `{"template":"base-TEMPLATE","cpu":9999}`, "invalid_request", 400},
	}
	for _, c := range cases {
		rec := create(c.body)
		if rec.Code != c.status || errCode(t, rec) != c.code {
			t.Errorf("%s: %d %s, want %d %s", c.name, rec.Code, rec.Body, c.status, c.code)
		}
	}

	m.cfg.AgentTemplates = []string{"base-TEMPLATE", "ghost"}
	if rec := create(`{"template":"ghost"}`); rec.Code != 404 || errCode(t, rec) != "template_not_found" {
		t.Errorf("ghost template: %d %s", rec.Code, rec.Body)
	}

	m.vms["base-TEMPLATE"].State = "running"
	if rec := create(`{"template":"base-TEMPLATE"}`); rec.Code != 409 || errCode(t, rec) != "template_busy" {
		t.Errorf("running template: %d %s", rec.Code, rec.Body)
	}

	m.vms["base-TEMPLATE"].State = "stopped"
	m.vms["a"] = &VM{Name: "a", State: "running"}
	m.vms["b"] = &VM{Name: "b", State: "starting"}
	rec := create(`{"template":"base-TEMPLATE"}`)
	if rec.Code != 409 || errCode(t, rec) != "capacity" {
		t.Errorf("full host: %d %s", rec.Code, rec.Body)
	}
	if len(m.agentOps) != 0 || len(m.reservedNames) != 0 {
		t.Errorf("a rejected create left state behind: ops=%v reserved=%v", m.agentOps, m.reservedNames)
	}
}

// A VM that never gets an IP must be cleaned up and the reason kept for the caller.
func TestAgentProvisionFailureCleansUp(t *testing.T) {
	m, h := newAgentTestManager(t)
	rec := do(h, "POST", "/api/agent/vms", `{"template":"base-TEMPLATE","label":"Build Job!"}`, map[string]string{})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	var v agentVMView
	decode(t, rec, &v)
	if !strings.HasPrefix(v.Name, "agent-build-job-") || v.Owner != "local" || v.Template != "base-TEMPLATE" {
		t.Fatalf("view = %+v", v)
	}
	if v.ExpiresAt == nil || v.SecondsRemaining < 59*60 {
		t.Fatalf("default lease should be about an hour: %+v", v)
	}
	if loc := rec.Header().Get("Location"); loc != "/api/agent/vms/"+v.Name {
		t.Fatalf("Location = %q", loc)
	}

	var final agentVMView
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		rec = do(h, "GET", "/api/agent/vms/"+v.Name+"/wait?timeout=20", "", nil)
		decode(t, rec, &final)
		if final.Phase == "failed" {
			break
		}
	}
	if final.Phase != "failed" || final.Ready || final.Error == nil || final.Error.Code == "" {
		t.Fatalf("expected a failed VM with a reason, got %+v", final)
	}
	if _, err := os.Stat(filepath.Join(m.cfg.VMStoragePath, "vms", v.Name)); !os.IsNotExist(err) {
		t.Fatal("the failed VM was not deleted")
	}
	m.mu.Lock()
	_, stillListed := m.vms[v.Name]
	m.mu.Unlock()
	if stillListed {
		t.Fatal("the failed VM is still tracked")
	}
}

func readyAgentVM(m *Manager, name, owner string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.vms[name] = &VM{Name: name, State: "running", AgentOK: true, IP: "192.168.64.5",
		Lease: &Lease{Owner: owner, Template: "base-TEMPLATE", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}}
}

func TestAgentVMOwnership(t *testing.T) {
	m, h := newAgentTestManager(t)
	admin, _ := auth.NewToken()
	m.auth().set(auth.Hash(admin))
	alice, _ := auth.AddAgentToken(m.auth().dir, "alice")
	bob, _ := auth.AddAgentToken(m.auth().dir, "bob")
	m.auth().invalidateAgentTokens()
	as := func(tok string) map[string]string { return map[string]string{"Authorization": "Bearer " + tok} }
	readyAgentVM(m, "agent-one", "alice")

	if rec := do(h, "GET", "/api/agent/vms/agent-one", "", as(alice)); rec.Code != 200 {
		t.Fatalf("owner = %d", rec.Code)
	}
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/agent/vms/agent-one"}, {"GET", "/api/agent/vms/agent-one/wait"},
		{"POST", "/api/agent/vms/agent-one/exec"}, {"POST", "/api/agent/vms/agent-one/extend"},
		{"DELETE", "/api/agent/vms/agent-one"},
	} {
		rec := do(h, c.method, c.path, `{"command":"id"}`, as(bob))
		if rec.Code != 404 || errCode(t, rec) != "not_found" {
			t.Errorf("another agent on %s %s = %d %s", c.method, c.path, rec.Code, rec.Body)
		}
	}
	var list struct{ VMs []agentVMView }
	decode(t, do(h, "GET", "/api/agent/vms", "", as(bob)), &list)
	if len(list.VMs) != 0 {
		t.Fatalf("bob sees %v", list.VMs)
	}
	decode(t, do(h, "GET", "/api/agent/vms", "", as(admin)), &list)
	if len(list.VMs) != 1 {
		t.Fatalf("admin sees %v", list.VMs)
	}
	// Hand-made VMs and templates are invisible to the API.
	if rec := do(h, "GET", "/api/agent/vms/base-TEMPLATE", "", as(admin)); rec.Code != 404 {
		t.Fatalf("template visible as agent VM: %d", rec.Code)
	}
	if rec := do(h, "DELETE", "/api/agent/vms/base-TEMPLATE", "", as(admin)); rec.Code != 404 {
		t.Fatalf("template deletable through the agent API: %d", rec.Code)
	}
}

func TestAgentExec(t *testing.T) {
	m, h := newAgentTestManager(t)
	readyAgentVM(m, "agent-x", "local")
	exec := func(body string) (*httptest.ResponseRecorder, agentExecResp) {
		rec := do(h, "POST", "/api/agent/vms/agent-x/exec", body, nil)
		var r agentExecResp
		if rec.Code == 200 {
			decode(t, rec, &r)
		}
		return rec, r
	}

	rec, r := exec(`{"command":"echo out; echo err >&2; exit 3"}`)
	if rec.Code != 200 || r.Stdout != "out\n" || r.Stderr != "err\n" || r.ExitCode != 3 || r.TimedOut {
		t.Fatalf("basic exec = %d %+v", rec.Code, r)
	}
	dir := t.TempDir()
	_, r = exec(`{"command":"echo \"$A:$B\"; pwd","cwd":` + jsonString(dir) + `,"env":{"A":"it's","B":"x y"}}`)
	if want := "it's:x y\n" + dir + "\n"; r.Stdout != want && r.Stdout != "it's:x y\n/private"+dir+"\n" {
		t.Fatalf("cwd/env: %q", r.Stdout)
	}
	if rec, r = exec(`{"command":"echo no","cwd":"/definitely/not/here"}`); r.ExitCode == 0 || strings.Contains(r.Stdout, "no") {
		t.Fatalf("a bad cwd must stop the command: %d %+v", rec.Code, r)
	}
	started := time.Now()
	_, r = exec(`{"command":"exec sleep 30","timeoutSec":1}`)
	if !r.TimedOut || r.ExitCode != -1 || time.Since(started) > 10*time.Second {
		t.Fatalf("timeout = %+v after %s", r, time.Since(started))
	}
	_, r = exec(`{"command":"head -c 3000000 /dev/zero | tr '\\0' a"}`)
	if !r.Truncated || len(r.Stdout) != agentOutputLimit {
		t.Fatalf("output cap: truncated=%v len=%d", r.Truncated, len(r.Stdout))
	}

	for body, code := range map[string]string{
		`{}`:                                 "invalid_request",
		`{"command":"x","timeoutSec":99999}`: "invalid_request",
		`{"command":"x","env":{"1bad":"v"}}`: "invalid_request",
		`{"command":"x","env":{"A;rm":"v"}}`: "invalid_request",
	} {
		if rec, _ := exec(body); rec.Code != 400 || errCode(t, rec) != code {
			t.Errorf("%s = %d %s", body, rec.Code, rec.Body)
		}
	}

	m.mu.Lock()
	m.vms["agent-x"].AgentOK = false
	m.mu.Unlock()
	if rec, _ := exec(`{"command":"id"}`); rec.Code != 409 || errCode(t, rec) != "not_ready" {
		t.Fatalf("exec before ready = %d %s", rec.Code, rec.Body)
	}
}

func jsonString(s string) string { b, _ := json.Marshal(s); return string(b) }

func TestAgentExtend(t *testing.T) {
	m, h := newAgentTestManager(t)
	readyAgentVM(m, "agent-x", "local")
	rec := do(h, "POST", "/api/agent/vms/agent-x/extend", `{"ttlMinutes":90}`, nil)
	var v agentVMView
	decode(t, rec, &v)
	if rec.Code != 200 || v.SecondsRemaining < 89*60 || v.SecondsRemaining > 90*60 {
		t.Fatalf("extend = %d %+v", rec.Code, v)
	}
	if rec := do(h, "POST", "/api/agent/vms/agent-x/extend", `{"ttlMinutes":121}`, nil); rec.Code != 400 || errCode(t, rec) != "invalid_ttl" {
		t.Fatalf("over max = %d %s", rec.Code, rec.Body)
	}
	m.mu.Lock()
	m.vms["agent-x"].Lease.ExpiresAt = time.Now().Add(-time.Minute)
	m.mu.Unlock()
	if rec := do(h, "POST", "/api/agent/vms/agent-x/extend", `{"ttlMinutes":10}`, nil); rec.Code != 409 || errCode(t, rec) != "lease_expired" {
		t.Fatalf("extend after expiry = %d %s", rec.Code, rec.Body)
	}
}

func makeVMDir(t *testing.T, m *Manager, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(m.cfg.VMStoragePath, "vms", name), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestReapLeasesDeletesOnlyExpired(t *testing.T) {
	m, _ := newAgentTestManager(t)
	for _, n := range []string{"agent-old", "agent-new", "handmade"} {
		makeVMDir(t, m, n)
	}
	m.vms["agent-old"] = &VM{Name: "agent-old", State: "stopped", Lease: &Lease{Owner: "x", ExpiresAt: time.Now().Add(-time.Minute)}}
	m.vms["agent-new"] = &VM{Name: "agent-new", State: "stopped", Lease: &Lease{Owner: "x", ExpiresAt: time.Now().Add(time.Hour)}}
	m.vms["handmade"] = &VM{Name: "handmade", State: "stopped"}

	m.reapLeases(time.Now())
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(m.cfg.VMStoragePath, "vms", "agent-old")); os.IsNotExist(err) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	for name, wantGone := range map[string]bool{"agent-old": true, "agent-new": false, "handmade": false, "base-TEMPLATE": false} {
		_, err := os.Stat(filepath.Join(m.cfg.VMStoragePath, "vms", name))
		if gone := os.IsNotExist(err); gone != wantGone {
			t.Errorf("%s: gone=%v, want %v", name, gone, wantGone)
		}
	}
}

func TestDestroyRefusesUnleasedVMs(t *testing.T) {
	m, _ := newAgentTestManager(t)
	if err := m.destroyLeasedVM("base-TEMPLATE"); err == nil {
		t.Fatal("destroyLeasedVM deleted a VM with no lease")
	}
	if _, err := os.Stat(filepath.Join(m.cfg.VMStoragePath, "vms", "base-TEMPLATE")); err != nil {
		t.Fatal("the template was touched")
	}
}

func TestSchedulerIgnoresLeasedVMs(t *testing.T) {
	leased := &VM{Name: "agent-x", State: "stopped", Lease: &Lease{}}
	if eligibleForScheduler(leased, false, map[string]bool{}, false) {
		t.Fatal("the scheduler may start a leased VM")
	}
	if !eligibleForScheduler(&VM{Name: "plain", State: "stopped"}, false, map[string]bool{}, false) {
		t.Fatal("a plain stopped VM should stay eligible")
	}
}

func TestPruneAgentSharedDirs(t *testing.T) {
	m, _ := newAgentTestManager(t)
	root := filepath.Join(m.cfg.SharedDir, agentSharedSub)
	for _, n := range []string{"agent-live", "agent-gone-old", "agent-gone-new", "keepme"} {
		os.MkdirAll(filepath.Join(root, n), 0o755)
	}
	m.vms["agent-live"] = &VM{Name: "agent-live"}
	old := time.Now().Add(-48 * time.Hour)
	for _, n := range []string{"agent-live", "agent-gone-old", "keepme"} {
		os.Chtimes(filepath.Join(root, n), old, old)
	}
	m.pruneAgentSharedDirs(time.Now())
	for n, wantExists := range map[string]bool{"agent-live": true, "agent-gone-old": false, "agent-gone-new": true, "keepme": true} {
		_, err := os.Stat(filepath.Join(root, n))
		if (err == nil) != wantExists {
			t.Errorf("%s exists=%v, want %v", n, err == nil, wantExists)
		}
	}
}

func TestAgentCommandQuoting(t *testing.T) {
	got, err := agentCommand("echo hi", "/a b/it's", map[string]string{"Z": "1", "A": "it's"})
	if err != nil {
		t.Fatal(err)
	}
	want := `export A='it'\''s'; export Z='1'; cd '/a b/it'\''s' || exit 1; echo hi`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if _, err := agentCommand("x", "", map[string]string{"A=B": "v"}); err == nil {
		t.Fatal("accepted a bad variable name")
	}
}

func TestAgentConfigSettings(t *testing.T) {
	m, h := newAgentTestManager(t)
	body := `{"agentEnabled":true,"agentTemplates":[" a ","a","","b"],"agentMaxTtlMin":999999}`
	if rec := do(h, "POST", "/api/config", body, nil); rec.Code != 200 {
		t.Fatalf("config = %d %s", rec.Code, rec.Body)
	}
	if got := strings.Join(m.cfg.AgentTemplates, ","); got != "a,b" {
		t.Fatalf("templates = %q", got)
	}
	if m.cfg.AgentMaxTTLMin != maxAgentTTLMin {
		t.Fatalf("max TTL = %d, want clamp to %d", m.cfg.AgentMaxTTLMin, maxAgentTTLMin)
	}
}

func TestShowAgentFeaturesSetting(t *testing.T) {
	m, h := newAgentTestManager(t)
	if m.cfg.ShowAgentFeatures {
		t.Fatal("the Agentic AI panel must be hidden by default")
	}
	if rec := do(h, "POST", "/api/config", `{"showAgentFeatures":true}`, nil); rec.Code != 200 || !m.cfg.ShowAgentFeatures {
		t.Fatalf("enabling: %d, show=%v", rec.Code, m.cfg.ShowAgentFeatures)
	}
	if rec := do(h, "POST", "/api/config", `{"showAgentFeatures":false}`, nil); rec.Code != 200 || m.cfg.ShowAgentFeatures {
		t.Fatalf("disabling: %d, show=%v", rec.Code, m.cfg.ShowAgentFeatures)
	}
	// Hiding the panel only hides the settings; it does not switch the API off.
	m.cfg.AgentEnabled = true
	do(h, "POST", "/api/config", `{"showAgentFeatures":false}`, nil)
	if !m.cfg.AgentEnabled {
		t.Fatal("hiding the panel turned agent access off")
	}
}

// A state file written before the toggle existed, with agent access already on,
// must keep showing its panel; an explicit "hidden" must stay hidden.
func TestShowAgentFeaturesOnUpgrade(t *testing.T) {
	load := func(config string) *Manager {
		m := newTestManager(t)
		if err := os.WriteFile(m.statePath, []byte(`{"config":{`+config+`}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		m.load()
		return m
	}
	if !load(`"agentEnabled":true`).cfg.ShowAgentFeatures {
		t.Error("agent access was on before the toggle existed, so its panel should be shown")
	}
	if load(`"agentEnabled":true,"showAgentFeatures":false`).cfg.ShowAgentFeatures {
		t.Error("an explicit false must stay hidden")
	}
	if load(`"agentEnabled":false`).cfg.ShowAgentFeatures {
		t.Error("agent access off: the panel stays hidden by default")
	}
}

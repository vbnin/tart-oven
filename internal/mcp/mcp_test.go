package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// session runs Serve against an in-memory pipe and gives the test a tiny RPC helper.
type session struct {
	t     *testing.T
	in    io.WriteCloser
	out   *bufio.Reader
	done  chan error
	id    int
	mu    sync.Mutex
	stash map[string]map[string]any // responses read ahead of the one being awaited
}

func start(t *testing.T, c *Client) *session {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	s := &session{t: t, in: inW, out: bufio.NewReader(outR), done: make(chan error, 1), stash: map[string]map[string]any{}}
	go func() { s.done <- Serve(context.Background(), inR, outW, c, "test"); outW.Close() }()
	t.Cleanup(func() { inW.Close() })
	return s
}

func (s *session) send(v any) {
	b, _ := json.Marshal(v)
	if _, err := s.in.Write(append(b, '\n')); err != nil {
		s.t.Fatal(err)
	}
}

func (s *session) read() map[string]any {
	s.t.Helper()
	ch := make(chan map[string]any, 1)
	go func() {
		line, err := s.out.ReadBytes('\n')
		if err != nil {
			ch <- nil
			return
		}
		var m map[string]any
		json.Unmarshal(line, &m)
		ch <- m
	}()
	select {
	case m := <-ch:
		if m == nil {
			s.t.Fatal("server closed the stream")
		}
		return m
	case <-time.After(10 * time.Second):
		s.t.Fatal("timed out waiting for a response")
		return nil
	}
}

// call sends a request and returns the response with the same id.
func (s *session) call(method string, params any) map[string]any {
	s.t.Helper()
	s.id++
	id := float64(s.id)
	s.send(map[string]any{"jsonrpc": "2.0", "id": s.id, "method": method, "params": params})
	for {
		m := s.read()
		if m["id"] == id {
			return m
		}
	}
}

func toolResult(t *testing.T, resp map[string]any) (string, bool) {
	t.Helper()
	res, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result in %v", resp)
	}
	content := res["content"].([]any)[0].(map[string]any)
	return content["text"].(string), res["isError"] == true
}

func TestInitializeAndListTools(t *testing.T) {
	s := start(t, &Client{BaseURL: "http://unused"})
	resp := s.call("initialize", map[string]any{"protocolVersion": "2025-03-26"})
	res := resp["result"].(map[string]any)
	if res["protocolVersion"] != "2025-03-26" {
		t.Errorf("should echo a supported version, got %v", res["protocolVersion"])
	}
	if res["serverInfo"].(map[string]any)["name"] != "tart-oven" {
		t.Errorf("serverInfo = %v", res["serverInfo"])
	}
	resp = s.call("initialize", map[string]any{"protocolVersion": "1999-01-01"})
	if got := resp["result"].(map[string]any)["protocolVersion"]; got != protocolVersions[0] {
		t.Errorf("unknown version should get the newest, got %v", got)
	}

	s.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}) // must not be answered
	resp = s.call("tools/list", nil)
	var names []string
	for _, tl := range resp["result"].(map[string]any)["tools"].([]any) {
		m := tl.(map[string]any)
		names = append(names, m["name"].(string))
		if m["description"] == "" || m["inputSchema"].(map[string]any)["type"] != "object" {
			t.Errorf("tool %v is missing a description or schema", m["name"])
		}
	}
	want := "tart_oven_info,create_vm,vm_status,list_vms,exec,extend_vm,destroy_vm"
	if strings.Join(names, ",") != want {
		t.Errorf("tools = %v, want %s", names, want)
	}
	if resp = s.call("nope/unknown", nil); resp["error"].(map[string]any)["code"] != float64(-32601) {
		t.Errorf("unknown method = %v", resp)
	}
	if resp = s.call("ping", nil); resp["error"] != nil {
		t.Errorf("ping = %v", resp)
	}
}

func TestParseErrorKeepsServing(t *testing.T) {
	s := start(t, &Client{BaseURL: "http://unused"})
	s.in.Write([]byte("{not json\n"))
	if m := s.read(); m["error"].(map[string]any)["code"] != float64(-32700) {
		t.Fatalf("parse error = %v", m)
	}
	if resp := s.call("ping", nil); resp["error"] != nil {
		t.Fatalf("server stopped serving: %v", resp)
	}
}

func TestCreateVMWaitsUntilReady(t *testing.T) {
	var mu sync.Mutex
	waits := 0
	var posted map[string]any
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ta_secret" {
			http.Error(w, "no", 401)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/agent/vms":
			json.NewDecoder(r.Body).Decode(&posted)
			w.WriteHeader(202)
			io.WriteString(w, `{"name":"agent-x","phase":"cloning","ready":false}`)
		case r.URL.Path == "/api/agent/vms/agent-x/wait":
			waits++
			if waits < 3 {
				io.WriteString(w, `{"name":"agent-x","phase":"booting","ready":false,"timedOut":true}`)
				return
			}
			io.WriteString(w, `{"name":"agent-x","phase":"ready","ready":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	s := start(t, &Client{BaseURL: api.URL, Token: "ta_secret"})
	resp := s.call("tools/call", map[string]any{"name": "create_vm", "arguments": map[string]any{
		"template": "base-TEMPLATE", "label": "build", "ttl_minutes": 30, "disk_size": 80, "headless": false,
	}})
	text, isErr := toolResult(t, resp)
	if isErr || !strings.Contains(text, `"ready": true`) {
		t.Fatalf("create_vm = %v %s", isErr, text)
	}
	if waits != 3 {
		t.Errorf("waited %d times, want 3", waits)
	}
	for k, want := range map[string]any{"template": "base-TEMPLATE", "label": "build", "ttlMinutes": float64(30), "diskSize": float64(80), "headless": false} {
		if posted[k] != want {
			t.Errorf("posted %s = %v, want %v", k, posted[k], want)
		}
	}
}

func TestCreateVMReportsFailureAndAPIErrors(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST":
			var b map[string]any
			json.NewDecoder(r.Body).Decode(&b)
			if b["template"] == "full" {
				w.WriteHeader(409)
				io.WriteString(w, `{"error":{"code":"capacity","message":"2 of 2 VM slots are in use"}}`)
				return
			}
			w.WriteHeader(202)
			io.WriteString(w, `{"name":"agent-y","phase":"cloning"}`)
		default:
			io.WriteString(w, `{"name":"agent-y","phase":"failed","ready":false,"error":{"code":"boot_failed","message":"no IP"}}`)
		}
	}))
	defer api.Close()
	s := start(t, &Client{BaseURL: api.URL})

	text, isErr := toolResult(t, s.call("tools/call", map[string]any{"name": "create_vm", "arguments": map[string]any{"template": "full"}}))
	if !isErr || !strings.Contains(text, "capacity") {
		t.Errorf("capacity error = %v %s", isErr, text)
	}
	text, isErr = toolResult(t, s.call("tools/call", map[string]any{"name": "create_vm", "arguments": map[string]any{"template": "ok"}}))
	if !isErr || !strings.Contains(text, "boot_failed") {
		t.Errorf("failed VM = %v %s", isErr, text)
	}
	text, isErr = toolResult(t, s.call("tools/call", map[string]any{"name": "create_vm", "arguments": map[string]any{"template": "ok", "wait": false}}))
	if isErr || !strings.Contains(text, "cloning") {
		t.Errorf("wait=false should return the accepted VM at once: %v %s", isErr, text)
	}
}

func TestExecMapsArgumentsAndKeepsNonZeroExitAsSuccess(t *testing.T) {
	var got map[string]any
	var path string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.EscapedPath()
		json.NewDecoder(r.Body).Decode(&got)
		io.WriteString(w, `{"stdout":"","stderr":"boom\n","exitCode":3,"timedOut":false}`)
	}))
	defer api.Close()
	s := start(t, &Client{BaseURL: api.URL})
	text, isErr := toolResult(t, s.call("tools/call", map[string]any{"name": "exec", "arguments": map[string]any{
		"name": "agent a/b", "command": "make test", "timeout_sec": 600, "cwd": "/tmp", "env": map[string]any{"CI": "1"},
	}}))
	if isErr || !strings.Contains(text, `"exitCode": 3`) {
		t.Fatalf("exec = %v %s", isErr, text)
	}
	if path != "/api/agent/vms/agent%20a%2Fb/exec" {
		t.Errorf("path = %s (names must be escaped)", path)
	}
	if got["command"] != "make test" || got["timeoutSec"] != float64(600) || got["cwd"] != "/tmp" || got["env"].(map[string]any)["CI"] != "1" {
		t.Errorf("body = %v", got)
	}
	if text, isErr = toolResult(t, s.call("tools/call", map[string]any{"name": "exec", "arguments": map[string]any{"command": "x"}})); !isErr {
		t.Errorf("missing name should be a tool error, got %s", text)
	}
}

func TestUnreachableServerIsAToolError(t *testing.T) {
	s := start(t, &Client{BaseURL: "http://127.0.0.1:1"})
	text, isErr := toolResult(t, s.call("tools/call", map[string]any{"name": "list_vms"}))
	if !isErr || !strings.Contains(text, "cannot reach Tart Oven") {
		t.Fatalf("got %v %s", isErr, text)
	}
	resp := s.call("tools/call", map[string]any{"name": "no_such_tool"})
	if resp["error"] == nil {
		t.Fatalf("unknown tool should be a protocol error: %v", resp)
	}
}

func TestSlowToolDoesNotBlockPing(t *testing.T) {
	release := make(chan struct{})
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		io.WriteString(w, `{"stdout":"done"}`)
	}))
	defer api.Close()
	defer close(release)
	s := start(t, &Client{BaseURL: api.URL})
	s.send(map[string]any{"jsonrpc": "2.0", "id": 100, "method": "tools/call", "params": map[string]any{"name": "exec", "arguments": map[string]any{"name": "x", "command": "sleep 1"}}})
	if resp := s.call("ping", nil); resp["error"] != nil {
		t.Fatalf("ping = %v", resp)
	}
}

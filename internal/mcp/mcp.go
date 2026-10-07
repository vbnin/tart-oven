// Package mcp is a small Model Context Protocol server (stdio transport) that
// exposes Tart Oven's agent API as tools. It speaks newline-delimited
// JSON-RPC 2.0 and holds no state of its own: every tool is one or more HTTP
// calls to a running Tart Oven, so it needs nothing from the server package.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// protocolVersions are the MCP revisions this server can speak, newest first.
var protocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

const instructions = `Tart Oven gives you disposable macOS virtual machines.
Use create_vm to get a ready VM cloned from an approved template, exec to run shell commands in it,
and destroy_vm as soon as you are done. A VM is deleted automatically when its lease ends.
Each exec is a fresh shell. Files move through the VM's shared folder (see sharedFolder in the VM).`

// Client calls a Tart Oven server's agent API.
type Client struct {
	BaseURL string // e.g. http://127.0.0.1:9000
	Token   string // agent-scope bearer token
	HTTP    *http.Client
}

// result is what a tool returns: text for the model, and whether it is an error.
type result struct {
	Text    string
	IsError bool
}

func (c *Client) do(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, rd)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("cannot reach Tart Oven at %s: %w", c.BaseURL, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return resp.StatusCode, raw, err
}

// call performs one API request and turns the answer into a tool result.
func (c *Client) call(ctx context.Context, method, path string, body any) result {
	status, raw, err := c.do(ctx, method, path, body)
	if err != nil {
		return result{err.Error(), true}
	}
	return result{prettyJSON(raw), status >= 400}
}

func prettyJSON(raw []byte) string {
	var buf bytes.Buffer
	if json.Indent(&buf, raw, "", "  ") == nil {
		return buf.String()
	}
	return strings.TrimSpace(string(raw))
}

type tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	run         func(ctx context.Context, c *Client, args map[string]any) result
}

func obj(required []string, props map[string]any) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

var (
	str  = func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	num  = func(desc string) map[string]any { return map[string]any{"type": "integer", "description": desc} }
	flag = func(desc string) map[string]any { return map[string]any{"type": "boolean", "description": desc} }
)

func argString(a map[string]any, k string) string { s, _ := a[k].(string); return s }
func argInt(a map[string]any, k string) int {
	switch v := a[k].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return 0
}

func vmPath(a map[string]any, suffix string) (string, bool) {
	name := argString(a, "name")
	if name == "" {
		return "", false
	}
	return "/api/agent/vms/" + url.PathEscape(name) + suffix, true
}

var needName = result{"name is required", true}

// createWaitLimit bounds how long create_vm waits for the VM to become ready.
const createWaitLimit = 6 * time.Minute

func tools() []tool {
	return []tool{
		{
			Name:        "tart_oven_info",
			Description: "Show which templates you may clone, how many VM slots are free (the host runs at most 2 VMs), and the limits that apply. Call this first.",
			InputSchema: obj(nil, map[string]any{}),
			run: func(ctx context.Context, c *Client, _ map[string]any) result {
				return c.call(ctx, "GET", "/api/agent/info", nil)
			},
		},
		{
			Name:        "create_vm",
			Description: "Create a fresh macOS VM from an approved template and wait until it is ready for commands. The VM is leased: it is stopped and deleted when the lease ends, or when you call destroy_vm. Fails with code 'capacity' when no VM slot is free.",
			InputSchema: obj([]string{"template"}, map[string]any{
				"template":    str("Name of an approved template, from tart_oven_info."),
				"label":       str("Short label used in the VM's name, e.g. 'build'."),
				"ttl_minutes": num("Lease length in minutes (default 60, capped by the server)."),
				"cpu":         num("CPU cores; omit to keep the template's."),
				"memory":      num("Memory in MB; omit to keep the template's."),
				"disk_size":   num("Disk size in GB (can only grow); omit to keep the template's."),
				"headless":    flag("Run without a display window (default true)."),
				"wait":        flag("Wait until the VM is ready (default true). Set false to return at once and poll with vm_status."),
			}),
			run: createVM,
		},
		{
			Name:        "vm_status",
			Description: "Show one VM's phase (cloning, booting, ready, failed, stopped), lease time left and shared folder paths.",
			InputSchema: obj([]string{"name"}, map[string]any{"name": str("VM name.")}),
			run: func(ctx context.Context, c *Client, a map[string]any) result {
				p, ok := vmPath(a, "")
				if !ok {
					return needName
				}
				return c.call(ctx, "GET", p, nil)
			},
		},
		{
			Name:        "list_vms",
			Description: "List the VMs you have created.",
			InputSchema: obj(nil, map[string]any{}),
			run: func(ctx context.Context, c *Client, _ map[string]any) result {
				return c.call(ctx, "GET", "/api/agent/vms", nil)
			},
		},
		{
			Name: "exec",
			Description: "Run a shell command in a ready VM and return stdout, stderr and the exit code (a non-zero exit is a normal result, not a tool error). " +
				"Every call is a fresh shell: use cwd and env, or chain with &&. Output over 1 MiB per stream is cut ('truncated'). " +
				"For large files or many results, write them to the VM's shared folder instead of printing them.",
			InputSchema: obj([]string{"name", "command"}, map[string]any{
				"name":        str("VM name."),
				"command":     str("Shell command (run with /bin/sh -c)."),
				"timeout_sec": num("Seconds before the command is abandoned (default 120, max 1800)."),
				"cwd":         str("Working directory inside the VM."),
				"env":         map[string]any{"type": "object", "description": "Environment variables to set.", "additionalProperties": map[string]any{"type": "string"}},
			}),
			run: execTool,
		},
		{
			Name:        "extend_vm",
			Description: "Set a VM's lease to expire this many minutes from now.",
			InputSchema: obj([]string{"name"}, map[string]any{
				"name":        str("VM name."),
				"ttl_minutes": num("Minutes from now (default 60, capped by the server)."),
			}),
			run: func(ctx context.Context, c *Client, a map[string]any) result {
				p, ok := vmPath(a, "/extend")
				if !ok {
					return needName
				}
				body := map[string]any{}
				if n := argInt(a, "ttl_minutes"); n != 0 {
					body["ttlMinutes"] = n
				}
				return c.call(ctx, "POST", p, body)
			},
		},
		{
			Name:        "destroy_vm",
			Description: "Stop and delete a VM. Always call this when you are finished; copy out anything you need from the shared folder first.",
			InputSchema: obj([]string{"name"}, map[string]any{"name": str("VM name.")}),
			run: func(ctx context.Context, c *Client, a map[string]any) result {
				p, ok := vmPath(a, "")
				if !ok {
					return needName
				}
				return c.call(ctx, "DELETE", p, nil)
			},
		},
	}
}

func createVM(ctx context.Context, c *Client, a map[string]any) result {
	body := map[string]any{"template": argString(a, "template")}
	for in, out := range map[string]string{"label": "label", "ttl_minutes": "ttlMinutes", "cpu": "cpu", "memory": "memory", "disk_size": "diskSize", "headless": "headless"} {
		if v, ok := a[in]; ok {
			body[out] = v
		}
	}
	status, raw, err := c.do(ctx, "POST", "/api/agent/vms", body)
	if err != nil {
		return result{err.Error(), true}
	}
	if status >= 400 {
		return result{prettyJSON(raw), true}
	}
	if w, ok := a["wait"].(bool); ok && !w {
		return result{prettyJSON(raw), false}
	}
	var vm struct {
		Name  string `json:"name"`
		Ready bool   `json:"ready"`
		Phase string `json:"phase"`
	}
	if json.Unmarshal(raw, &vm) != nil || vm.Name == "" {
		return result{prettyJSON(raw), false}
	}
	wctx, cancel := context.WithTimeout(ctx, createWaitLimit)
	defer cancel()
	last := raw
	for {
		status, raw, err = c.do(wctx, "GET", "/api/agent/vms/"+url.PathEscape(vm.Name)+"/wait?timeout=120", nil)
		if err != nil {
			if wctx.Err() != nil && ctx.Err() == nil {
				break // out of time; report where it got to
			}
			return result{err.Error(), true}
		}
		if status >= 400 {
			return result{prettyJSON(raw), true}
		}
		last = raw
		json.Unmarshal(raw, &vm)
		if vm.Ready {
			return result{prettyJSON(raw), false}
		}
		if vm.Phase == "failed" || vm.Phase == "stopped" {
			return result{prettyJSON(raw), true}
		}
	}
	return result{prettyJSON(last) + "\n\nThe VM is not ready yet. Poll vm_status, or destroy_vm to give up.", true}
}

func execTool(ctx context.Context, c *Client, a map[string]any) result {
	p, ok := vmPath(a, "/exec")
	if !ok {
		return needName
	}
	body := map[string]any{"command": argString(a, "command")}
	for in, out := range map[string]string{"timeout_sec": "timeoutSec", "cwd": "cwd", "env": "env"} {
		if v, ok := a[in]; ok {
			body[out] = v
		}
	}
	// Give the server a minute beyond the command's own timeout to answer.
	secs := argInt(a, "timeout_sec")
	if secs == 0 {
		secs = 120
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(secs+60)*time.Second)
	defer cancel()
	return c.call(ctx, "POST", p, body)
}

// ---------------------------------------------------------------------------
// JSON-RPC over stdio
// ---------------------------------------------------------------------------

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// Serve reads requests from in until it closes, answering on out. Requests are
// handled concurrently, so a long exec never blocks a ping.
func Serve(ctx context.Context, in io.Reader, out io.Writer, c *Client, version string) error {
	var (
		wmu sync.Mutex
		wg  sync.WaitGroup
	)
	write := func(r response) {
		r.JSONRPC = "2.0"
		b, err := json.Marshal(r)
		if err != nil {
			return
		}
		wmu.Lock()
		defer wmu.Unlock()
		out.Write(append(b, '\n'))
	}
	list := tools()
	byName := make(map[string]tool, len(list))
	for _, t := range list {
		byName[t.Name] = t
	}

	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			write(response{ID: json.RawMessage("null"), Error: &rpcError{-32700, "parse error"}})
			continue
		}
		isNotification := len(req.ID) == 0
		switch req.Method {
		case "initialize":
			var p struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			json.Unmarshal(req.Params, &p)
			ver := protocolVersions[0]
			for _, v := range protocolVersions {
				if v == p.ProtocolVersion {
					ver = v
				}
			}
			write(response{ID: req.ID, Result: map[string]any{
				"protocolVersion": ver,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "tart-oven", "version": version},
				"instructions":    instructions,
			}})
		case "ping":
			write(response{ID: req.ID, Result: map[string]any{}})
		case "tools/list":
			write(response{ID: req.ID, Result: map[string]any{"tools": list}})
		case "tools/call":
			wg.Add(1)
			go func(req request) {
				defer wg.Done()
				var p struct {
					Name      string         `json:"name"`
					Arguments map[string]any `json:"arguments"`
				}
				if err := json.Unmarshal(req.Params, &p); err != nil {
					write(response{ID: req.ID, Error: &rpcError{-32602, "invalid params"}})
					return
				}
				t, ok := byName[p.Name]
				if !ok {
					write(response{ID: req.ID, Error: &rpcError{-32602, "unknown tool " + p.Name}})
					return
				}
				if p.Arguments == nil {
					p.Arguments = map[string]any{}
				}
				res := t.run(ctx, c, p.Arguments)
				write(response{ID: req.ID, Result: map[string]any{
					"content": []map[string]any{{"type": "text", "text": res.Text}},
					"isError": res.IsError,
				}})
			}(req)
		default:
			if !isNotification {
				write(response{ID: req.ID, Error: &rpcError{-32601, "method not found: " + req.Method}})
			}
		}
	}
	wg.Wait()
	return sc.Err()
}

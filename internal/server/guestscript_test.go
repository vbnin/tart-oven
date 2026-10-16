package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// runScript executes the wrapper locally the way a guest shell would.
func runScript(t *testing.T, script string, withPassword bool) (string, int) {
	t.Helper()
	mode := sudoAsIs
	if withPassword {
		mode = sudoStdin
	}
	cmd, err := scriptCommand(script, mode)
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("/bin/sh", "-c", cmd).CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), code
}

func TestScriptCommandRunsMultiLineScripts(t *testing.T) {
	out, code := runScript(t, "echo one\necho two\nexit 3\n", false)
	if strings.TrimSpace(out) != "one\ntwo" || code != 3 {
		t.Fatalf("out=%q code=%d", out, code)
	}
}

func TestScriptCommandHonoursShebang(t *testing.T) {
	out, _ := runScript(t, "#!/bin/zsh\nprint -r -- $ZSH_VERSION | head -c1 | grep -q . && echo zsh-ok\n", false)
	if !strings.Contains(out, "zsh-ok") {
		t.Fatalf("shebang interpreter was not used: %q", out)
	}
	// Without a shebang the script runs in bash, not the login shell.
	out, _ = runScript(t, "echo ${BASH_VERSION:+bash-ok}\n", false)
	if !strings.Contains(out, "bash-ok") {
		t.Fatalf("default interpreter is not bash: %q", out)
	}
}

func TestScriptCommandNormalisesLineEndings(t *testing.T) {
	out, code := runScript(t, "#!/bin/sh\r\necho windows\r\n", false)
	if strings.TrimSpace(out) != "windows" || code != 0 {
		t.Fatalf("out=%q code=%d", out, code)
	}
}

func TestScriptCommandLeavesNoFileBehind(t *testing.T) {
	out, _ := runScript(t, "echo $0 > /tmp/tart-oven-script-test-path\n", false)
	_ = out
	path, err := exec.Command("/bin/cat", "/tmp/tart-oven-script-test-path").Output()
	if err != nil {
		t.Fatal(err)
	}
	exec.Command("/bin/rm", "-f", "/tmp/tart-oven-script-test-path").Run()
	if err := exec.Command("/bin/test", "-e", strings.TrimSpace(string(path))).Run(); err == nil {
		t.Fatalf("temporary script %s was not removed", path)
	}
}

func TestScriptCommandRewritesSudoForStdin(t *testing.T) {
	cmd, err := scriptCommand("sudo whoami\n", sudoStdin)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(cmd, "sudo") {
		t.Fatal("the wrapper must carry the script encoded, not in clear")
	}
	// The script travels encoded, so check by running it with a stand-in sudo.
	script := "sudo() { echo \"args: $*\"; }\nsudo whoami\n"
	if out, _ := runScript(t, script, true); !strings.Contains(out, "args: -S -p  whoami") {
		t.Fatalf("sudo was not rewritten to read stdin: %q", out)
	}
	if out, _ := runScript(t, script, false); !strings.Contains(out, "args: whoami") {
		t.Fatalf("sudo was rewritten although no password was given: %q", out)
	}
}

func TestScriptCommandRejectsBadInput(t *testing.T) {
	for name, script := range map[string]string{
		"empty":      "  \n\n",
		"binary":     "echo hi\x00",
		"too large":  strings.Repeat("echo hi\n", maxScriptBytes/4),
		"only space": "\t",
	} {
		if _, err := scriptCommand(script, sudoAsIs); err == nil {
			t.Errorf("%s: script was accepted", name)
		}
	}
}

func TestRepeatedSudoPassword(t *testing.T) {
	got := repeatSudoPassword("pw")
	if strings.Count(got, "pw") != sudoPasswordLines || strings.HasSuffix(got, "\n") {
		t.Fatalf("got %d lines, trailing newline=%v", strings.Count(got, "pw"), strings.HasSuffix(got, "\n"))
	}
}

func TestExecEndpointRunsScriptsAndReportsUserAndTransport(t *testing.T) {
	m, h := newAgentTestManager(t)
	m.vms["vm1"] = &VM{Name: "vm1", State: "running", SSHUser: "recovery"}

	body, _ := json.Marshal(map[string]string{
		"name": "vm1", "scriptName": "demo.sh",
		"script": "echo first\necho second >&2\nexit 4\n",
	})
	rec := do(h, "POST", "/api/exec", string(body), nil)
	var res execResult
	decode(t, rec, &res)
	if res.Error != "" || res.ExitCode != 4 || !strings.Contains(res.Stdout, "first") || !strings.Contains(res.Stderr, "second") {
		t.Fatalf("result = %+v", res)
	}
	if res.User != "recovery" || res.Via != "agent" {
		t.Fatalf("user=%q via=%q, want recovery/agent", res.User, res.Via)
	}

	// The default account is admin.
	m.vms["vm1"].SSHUser = ""
	rec = do(h, "POST", "/api/exec", `{"name":"vm1","command":"true"}`, nil)
	decode(t, rec, &res)
	if res.User != "admin" {
		t.Fatalf("default user = %q", res.User)
	}

	// An empty or unreadable script is refused with a message, not run.
	rec = do(h, "POST", "/api/exec", `{"name":"vm1","script":"   "}`, nil)
	decode(t, rec, &res)
	if res.Error == "" {
		t.Fatalf("empty script was run: %+v", res)
	}
}

func TestExecEndpointStopsWhenRequestIsAborted(t *testing.T) {
	m, h := newAgentTestManager(t)
	m.vms["vm1"] = &VM{Name: "vm1", State: "running"}

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("POST", "/api/exec", strings.NewReader(`{"name":"vm1","command":"sleep 30"}`)).WithContext(ctx)
	req.RemoteAddr = "192.0.2.10:5555"
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(httptest.NewRecorder(), req)
		close(done)
	}()
	time.Sleep(300 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the command kept running after the request was aborted")
	}
}

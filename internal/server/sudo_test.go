package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestRewriteSudoNoPromptNeverLeavesSudoAbleToWait(t *testing.T) {
	cases := map[string]string{
		"sudo whoami":                          "sudo -n whoami",
		"echo hi; sudo ls && sudo -u bob id":   "echo hi; sudo -n ls && sudo -n -u bob id",
		"sudo -n true":                         "sudo -n true",
		"sudo -S -p '' cat":                    "sudo -S -p '' cat",
		"sudo -A true":                         "sudo -A true",
		"sudoku is not sudo":                   "sudoku is not sudo", // a word, not the command
		"FOO=1 sudo /usr/local/bin/jamf recon": "FOO=1 sudo -n /usr/local/bin/jamf recon",
		"echo (sudo whoami)":                   "echo (sudo -n whoami)",
	}
	for in, want := range cases {
		if got := rewriteSudoNoPrompt(in); got != want {
			t.Errorf("rewriteSudoNoPrompt(%q) = %q, want %q", in, got, want)
		}
	}
}

// echoSudo defines a stand-in sudo that reports its arguments and what arrives on
// stdin, so a test can see which flags and which password a command got.
const echoSudo = `sudo() { echo "sudo-args:$*"; cat; }; `

func TestTerminalSudoPasswordOrder(t *testing.T) {
	m, h := newAgentTestManager(t)
	m.cfg.SSHPassword = "default-pw"
	m.vms["vm1"] = &VM{Name: "vm1", State: "running"}

	run := func(body map[string]string) execResult {
		t.Helper()
		b, _ := json.Marshal(body)
		var res execResult
		decode(t, do(h, "POST", "/api/exec", string(b), nil), &res)
		return res
	}
	cmd := echoSudo + "sudo true"

	// 1. what is typed in the terminal wins
	m.vms["vm1"].SSHPassword = "vm-pw"
	res := run(map[string]string{"name": "vm1", "command": cmd, "sudoPassword": "typed-pw"})
	if !strings.Contains(res.Stdout, "typed-pw") || strings.Contains(res.Stdout, "vm-pw") || res.SudoFrom != "terminal" {
		t.Fatalf("typed password: %+v", res)
	}
	if !strings.Contains(res.Stdout, "-S") {
		t.Fatalf("sudo was not told to read stdin: %+v", res)
	}

	// 2. blank: the VM's own password
	res = run(map[string]string{"name": "vm1", "command": cmd})
	if !strings.Contains(res.Stdout, "vm-pw") || res.SudoFrom != "VM setting" {
		t.Fatalf("VM password: %+v", res)
	}

	// 3. blank and none on the VM: the default one
	m.vms["vm1"].SSHPassword = ""
	res = run(map[string]string{"name": "vm1", "command": cmd})
	if !strings.Contains(res.Stdout, "default-pw") || res.SudoFrom != "default" {
		t.Fatalf("default password: %+v", res)
	}

	// 4. no password anywhere: sudo is told never to prompt, instead of waiting
	m.cfg.SSHPassword = ""
	res = run(map[string]string{"name": "vm1", "command": cmd})
	if !strings.Contains(res.Stdout, "sudo-args:-n true") || res.SudoFrom != "" {
		t.Fatalf("no password: %+v", res)
	}

	// 5. a command without sudo does not get a saved password pushed in
	m.cfg.SSHPassword = "default-pw"
	res = run(map[string]string{"name": "vm1", "command": "cat; echo done"})
	if strings.Contains(res.Stdout, "default-pw") || res.SudoFrom != "" {
		t.Fatalf("password leaked into a command without sudo: %+v", res)
	}
}

func TestScriptsUseTheSamePasswordOrder(t *testing.T) {
	m, h := newAgentTestManager(t)
	m.cfg.SSHPassword = "default-pw"
	m.vms["vm1"] = &VM{Name: "vm1", State: "running", SSHPassword: "vm-pw"}
	script := "#!/bin/bash\n" + echoSudo + "sudo true\n"
	b, _ := json.Marshal(map[string]string{"name": "vm1", "script": script})
	var res execResult
	decode(t, do(h, "POST", "/api/exec", string(b), nil), &res)
	if !strings.Contains(res.Stdout, "vm-pw") || res.SudoFrom != "VM setting" {
		t.Fatalf("script: %+v", res)
	}

	m.vms["vm1"].SSHPassword = ""
	m.cfg.SSHPassword = ""
	decode(t, do(h, "POST", "/api/exec", string(b), nil), &res)
	if !strings.Contains(res.Stdout, "sudo-args:-n true") {
		t.Fatalf("script without any password must not let sudo prompt: %+v", res)
	}
}

// Stop must end the command inside the guest too, not just abandon waiting for it.
func TestAbortedCommandIsKilledInTheGuest(t *testing.T) {
	m, h := newAgentTestManager(t)
	m.vms["vm1"] = &VM{Name: "vm1", State: "running"}
	const marker = "sleep 29.4137"

	// A guest outlives the client talking to it. Make the fake behave that way:
	// the command runs in its own process, so killing the fake tart (which is
	// all an abort can do) leaves it running, as it does in a real VM.
	detached := strings.Replace(fakeTart, `exec) shift; [ "$1" = "-i" ] && shift; shift; exec "$@";;`,
		`exec) shift; [ "$1" = "-i" ] && shift; shift; "$@" & wait $!;;`, 1)
	if detached == fakeTart {
		t.Fatal("the fake tart changed; update this test")
	}
	if err := os.WriteFile(m.cfg.TartAppPath, []byte(detached), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("POST", "/api/exec", strings.NewReader(`{"name":"vm1","command":"`+marker+`; echo not-reached"}`)).WithContext(ctx)
	req.RemoteAddr = "192.0.2.10:5555"
	done := make(chan struct{})
	go func() { h.ServeHTTP(httptest.NewRecorder(), req); close(done) }()

	// Anchored, so only the sleep itself counts, not the shell whose command
	// line merely contains the text.
	alive := func() bool { return exec.Command("/usr/bin/pgrep", "-f", "^"+marker).Run() == nil }
	deadline := time.Now().Add(5 * time.Second)
	for !alive() {
		if time.Now().After(deadline) {
			t.Fatal("the command never started")
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the handler did not return after the abort")
	}
	deadline = time.Now().Add(5 * time.Second)
	for alive() {
		if time.Now().After(deadline) {
			exec.Command("/usr/bin/pkill", "-f", "^"+marker).Run()
			t.Fatal("the aborted command is still running")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

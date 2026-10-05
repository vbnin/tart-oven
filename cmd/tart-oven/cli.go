package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"tart-oven/internal/auth"
)

const (
	agentLabel  = "com.tartoven.agent"
	agentPlist  = "/Library/LaunchAgents/com.tartoven.agent.plist"
	defaultAddr = "127.0.0.1:9000"
)

const usage = `Usage: tart-oven [command]

Commands:
  (none), serve   run the server in the foreground (what the LaunchAgent does)
  start           start the server in the background
  stop            stop the server
  restart         restart the server
  status          show whether the server is running
  open            start the server if needed, then open the dashboard
  token generate  create (or rotate) the dashboard access token; printed once
  token revoke    remove the token so the dashboard needs no login
  token status    show whether a token is set
  help            show this help

Flags (serve only):
  -listen host:port   address to bind (overrides config)
  -state path         path to state.json
  -version            print version and exit
`

// cli carries what the client commands need: where the server listens and
// how to reach launchd.
type cli struct {
	statePath string
	url       string
	client    *http.Client
}

func newCLI(statePath string) *cli {
	scheme := "http"
	transport := &http.Transport{}
	if tlsEnabled(statePath) {
		scheme = "https"
		// The CLI only ever talks to this host's own server, which may use
		// a self-signed certificate, so the chain is not verified here.
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return &cli{
		statePath: statePath,
		url:       scheme + "://" + clientAddr(listenAddr(statePath)),
		client:    &http.Client{Timeout: 3 * time.Second, Transport: transport},
	}
}

// post sends a control request (stop, restart). The control key proves this
// is a local process of the same user, so it works while a token is set.
func (c *cli) post(path string) error {
	req, err := http.NewRequest(http.MethodPost, c.url+path, nil)
	if err != nil {
		return err
	}
	if key := auth.ReadControlKey(filepath.Dir(c.statePath)); key != "" {
		req.Header.Set("X-Tart-Oven-Control", key)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return errors.New("the server refused the request (401); it may predate token support, restart it with launchctl")
	}
	return nil
}

// tlsEnabled reports whether state.json turns HTTPS on.
func tlsEnabled(statePath string) bool {
	data, err := os.ReadFile(statePath)
	if err != nil {
		return false
	}
	var p struct {
		Config struct {
			TLSEnabled bool `json:"tlsEnabled"`
		} `json:"config"`
	}
	return json.Unmarshal(data, &p) == nil && p.Config.TLSEnabled
}

// token handles `tart-oven token generate|rotate|revoke|status`. It edits
// auth.json beside state.json, which the running server notices on its own,
// so it works whether or not the server is up and needs no existing token.
func (c *cli) token(args []string) error {
	dir := filepath.Dir(c.statePath)
	sub := "status"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "generate", "rotate":
		tok, err := auth.NewToken()
		if err != nil {
			return err
		}
		if err := auth.Save(dir, auth.Hash(tok)); err != nil {
			return err
		}
		fmt.Println("New dashboard access token (shown once, store it safely):")
		fmt.Println()
		fmt.Println("  " + tok)
		fmt.Println()
		fmt.Println("All earlier tokens and signed-in browsers stop working.")
	case "revoke":
		if err := auth.Remove(dir); err != nil {
			return err
		}
		fmt.Println("Access token removed; the dashboard no longer asks for a login.")
	case "status":
		if hash, err := auth.Load(dir); err != nil {
			fmt.Println("Access token file is damaged; the dashboard is locked. Run `tart-oven token generate` or `token revoke`.")
		} else if hash != "" {
			fmt.Println("Access token is set.")
		} else {
			fmt.Println("No access token; the dashboard needs no login.")
		}
	default:
		return fmt.Errorf("unknown token command %q (use generate, rotate, revoke or status)", sub)
	}
	return nil
}

// runCommand dispatches a client subcommand and returns the process exit code.
func runCommand(cmd, statePath string, args ...string) int {
	c := newCLI(statePath)
	var err error
	switch cmd {
	case "start":
		err = c.start()
	case "stop":
		err = c.stop()
	case "restart":
		err = c.restart()
	case "status":
		return c.status()
	case "open":
		if err = c.start(); err == nil {
			err = exec.Command("open", c.url).Run()
		}
	case "token":
		err = c.token(args)
	case "help", "-h", "--help":
		fmt.Print(usage)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "tart-oven: unknown command %q\n\n%s", cmd, usage)
		return 2
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "tart-oven:", err)
		return 1
	}
	return 0
}

// listenAddr reads the configured listen address from state.json.
func listenAddr(statePath string) string {
	data, err := os.ReadFile(statePath)
	if err != nil {
		return defaultAddr
	}
	var p struct {
		Config struct {
			Listen string `json:"listen"`
		} `json:"config"`
	}
	if json.Unmarshal(data, &p) != nil || strings.TrimSpace(p.Config.Listen) == "" {
		return defaultAddr
	}
	return strings.TrimSpace(p.Config.Listen)
}

// clientAddr turns a listen address into one a client can dial: a wildcard
// or empty host becomes the loopback address.
func clientAddr(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return defaultAddr
	}
	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// version returns the running server's version, or "" when it doesn't answer.
func (c *cli) version() string {
	// /api/health needs no token; servers older than token support only have /api/vms.
	for _, path := range []string{"/api/health", "/api/vms"} {
		resp, err := c.client.Get(c.url + path)
		if err != nil {
			return ""
		}
		var s struct {
			Version string `json:"version"`
		}
		ok := resp.StatusCode == http.StatusOK && json.NewDecoder(resp.Body).Decode(&s) == nil
		resp.Body.Close()
		if ok {
			if s.Version == "" {
				return "unknown"
			}
			return s.Version
		}
	}
	return ""
}

func (c *cli) running() bool { return c.version() != "" }

// waitFor polls until the server's reachability matches want.
func (c *cli) waitFor(want bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if c.running() == want {
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return c.running() == want
}

func launchctl(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "launchctl", args...).CombinedOutput()
	if ctx.Err() != nil {
		return string(out), fmt.Errorf("launchctl %s timed out", args[0])
	}
	return string(out), err
}

func agentTarget() string { return fmt.Sprintf("gui/%d/%s", os.Getuid(), agentLabel) }

// agentDisabled reports whether "Launch at login" is off, which also stops
// launchd from loading the job by hand.
func agentDisabled() bool {
	out, err := launchctl("print-disabled", fmt.Sprintf("gui/%d", os.Getuid()))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, `"`+agentLabel+`"`) && strings.Contains(line, "disabled") {
			return true
		}
	}
	return false
}

func (c *cli) start() error {
	if v := c.version(); v != "" {
		fmt.Printf("Tart Oven %s is already running at %s\n", v, c.url)
		return nil
	}
	if _, err := os.Stat(agentPlist); err == nil && !agentDisabled() {
		if _, err := launchctl("print", agentTarget()); err == nil {
			// Loaded but not answering: kick it. bootout + bootstrap also
			// clears a crash-loop throttle and re-registers a replaced binary.
			launchctl("bootout", agentTarget())
		}
		if out, err := launchctl("bootstrap", fmt.Sprintf("gui/%d", os.Getuid()), agentPlist); err != nil {
			return fmt.Errorf("launchctl bootstrap failed: %v %s", err, strings.TrimSpace(out))
		}
	} else if err := c.spawn(); err != nil {
		return err
	}
	if !c.waitFor(true, 20*time.Second) {
		return fmt.Errorf("started, but nothing answers at %s yet; check %s", c.url, logHint())
	}
	fmt.Printf("Tart Oven %s is running at %s\n", c.version(), c.url)
	return nil
}

// spawn starts the server detached from this terminal, for source builds and
// for hosts where the LaunchAgent is turned off.
func (c *cli) spawn() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logPath := filepath.Join(filepath.Dir(c.statePath), "server.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer logFile.Close()
	cmd := exec.Command(exe, "serve", "-state", c.statePath)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	fmt.Printf("Started without launchd (log: %s)\n", logPath)
	return cmd.Process.Release()
}

func (c *cli) stop() error {
	if c.running() {
		if err := c.post("/api/server/stop"); err != nil {
			return err
		}
		if !c.waitFor(false, 10*time.Second) {
			return errors.New("the server is still answering after a stop request")
		}
	} else {
		launchctl("bootout", agentTarget()) // a crash-looping job counts as stopped too
	}
	fmt.Println("Tart Oven is stopped")
	return nil
}

func (c *cli) restart() error {
	if !c.running() {
		return c.start()
	}
	if err := c.post("/api/server/restart"); err != nil {
		return err
	}
	time.Sleep(time.Second) // the server re-execs itself after replying
	if !c.waitFor(true, 20*time.Second) {
		return fmt.Errorf("nothing answers at %s after the restart; check %s", c.url, logHint())
	}
	fmt.Printf("Tart Oven %s restarted at %s\n", c.version(), c.url)
	return nil
}

// status prints the server state; exit code 0 when running, 3 when stopped.
func (c *cli) status() int {
	if v := c.version(); v != "" {
		fmt.Printf("Tart Oven %s is running at %s\n", v, c.url)
		return 0
	}
	fmt.Printf("Tart Oven is not running (expected at %s)\n", c.url)
	if out, err := launchctl("print", agentTarget()); err == nil && strings.Contains(out, "last exit code") {
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, "last exit code") {
				fmt.Println("LaunchAgent", strings.TrimSpace(line))
			}
		}
	}
	return 3
}

func logHint() string {
	if _, err := os.Stat(agentPlist); err == nil {
		return "/Users/Shared/tart-oven.err.log"
	}
	return "~/.tart-oven/server.log"
}

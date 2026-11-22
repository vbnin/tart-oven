package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"time"

	"tart-oven/internal/auth"
)

// crashClientTimeout covers the guest command; the server bounds it at 60 s.
const crashClientTimeout = 2 * time.Minute

// guestGet sends a GET to the running server with the local control key.
func (c *cli) guestGet(path string, query url.Values) (int, []byte, error) {
	req, err := http.NewRequest(http.MethodGet, c.url+path+"?"+query.Encode(), nil)
	if err != nil {
		return 0, nil, err
	}
	if key := auth.ReadControlKey(filepath.Dir(c.statePath)); key != "" {
		req.Header.Set("X-Tart-Oven-Control", key)
	}
	client := &http.Client{Timeout: crashClientTimeout, Transport: c.client.Transport}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return resp.StatusCode, body, err
}

// serverError turns a non-200 reply into an error, using the server's message.
func serverError(status int, body []byte) error {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error != "" {
		return errors.New(e.Error)
	}
	if status == http.StatusUnauthorized {
		return errors.New("the server refused the request (401); the local control key was not accepted")
	}
	return fmt.Errorf("the server answered HTTP %d", status)
}

// runCrashReports lists a VM's crash reports, newest first, optionally only
// those of one app.
func (c *cli) runCrashReports(args []string, out io.Writer) error {
	if len(args) < 1 || len(args) > 2 {
		return errors.New("usage: tart-oven crash-reports <vm> [app name]")
	}
	query := url.Values{"name": {args[0]}}
	if len(args) == 2 {
		query.Set("app", args[1])
	}
	status, body, err := c.guestGet("/api/vm/crash-reports", query)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return serverError(status, body)
	}
	var reply struct {
		Reports []struct {
			Name     string    `json:"name"`
			Modified time.Time `json:"modified"`
		} `json:"reports"`
	}
	if err := json.Unmarshal(body, &reply); err != nil {
		return fmt.Errorf("unreadable reply from the server: %v", err)
	}
	if len(reply.Reports) == 0 {
		fmt.Fprintln(out, "no crash reports")
		return nil
	}
	for _, r := range reply.Reports {
		fmt.Fprintf(out, "%s  %s\n", r.Modified.Local().Format("2006-01-02 15:04:05"), r.Name)
	}
	return nil
}

// runCrashReport prints one crash report, named as crash-reports lists it.
func (c *cli) runCrashReport(args []string, out io.Writer) error {
	if len(args) != 2 {
		return errors.New("usage: tart-oven crash-report <vm> <report file name>")
	}
	status, body, err := c.guestGet("/api/vm/crash-report", url.Values{"name": {args[0]}, "file": {args[1]}})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return serverError(status, body)
	}
	_, err = out.Write(body)
	return err
}

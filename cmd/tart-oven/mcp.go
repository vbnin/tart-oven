package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	tartoven "tart-oven"
	"tart-oven/internal/mcp"
)

// runMCP serves the agent API as MCP tools over stdin/stdout, for an AI client
// that launches `tart-oven mcp` as a subprocess. Nothing but protocol messages
// may reach stdout.
//
// TART_OVEN_URL and TART_OVEN_TOKEN point it at a server and an agent token;
// without a URL it talks to the local server from state.json. A self-signed
// certificate is accepted for the local server, and for a custom URL only when
// TART_OVEN_INSECURE=1.
func runMCP(statePath string) int {
	base, token := os.Getenv("TART_OVEN_URL"), os.Getenv("TART_OVEN_TOKEN")
	skipVerify := os.Getenv("TART_OVEN_INSECURE") == "1"
	if base == "" {
		base = newCLI(statePath).url
		skipVerify = tlsEnabled(statePath)
	}
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment}
	if skipVerify {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	client := &mcp.Client{BaseURL: base, Token: token, HTTP: &http.Client{Transport: transport}}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := mcp.Serve(ctx, os.Stdin, os.Stdout, client, tartoven.Version); err != nil {
		fmt.Fprintln(os.Stderr, "tart-oven mcp:", err)
		return 1
	}
	return 0
}

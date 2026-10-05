package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	tartoven "tart-oven"
	"tart-oven/internal/server"
)

func main() {
	home, _ := os.UserHomeDir()
	defaultState := filepath.Join(home, ".tart-oven", "state.json")

	// An optional leading command ("start", "status", ...) comes before flags.
	cmd, args := "serve", os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}

	// `token generate -state path`: the subcommand precedes the flags too.
	var sub []string
	if cmd == "token" && len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[:1], args[1:]
	}

	flags := flag.NewFlagSet("tart-oven", flag.ExitOnError)
	flags.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	listenFlag := flags.String("listen", "", "host:port to bind (overrides config)")
	stateFlag := flags.String("state", defaultState, "path to state.json")
	versionFlag := flags.Bool("version", false, "print version and exit")
	flags.Parse(args)
	if *versionFlag {
		fmt.Println(tartoven.Version)
		return
	}
	if cmd != "serve" {
		os.Exit(runCommand(cmd, *stateFlag, sub...))
	}

	if err := os.MkdirAll(filepath.Dir(*stateFlag), 0o755); err != nil {
		log.Fatalf("cannot create state dir: %v", err)
	}

	if err := server.Run(*stateFlag, *listenFlag); err != nil {
		log.Fatalf("server: %v", err)
	}
}

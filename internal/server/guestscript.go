package server

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// maxScriptBytes caps a script sent from the terminal. It travels as one
// command-line argument (base64 inflates it by a third), well inside macOS's
// 1 MB argument limit for both `tart exec` and ssh.
const maxScriptBytes = 200 * 1024

// sudoPasswordLines is how many times the sudo password is fed on stdin when a
// script runs: `sudo -S` reads one line per call, and a script may call sudo
// several times. A single command needs only one.
const sudoPasswordLines = 50

// repeatSudoPassword returns the password repeated once per line, without a
// trailing newline (the exec paths add the final one).
func repeatSudoPassword(pw string) string {
	return strings.TrimSuffix(strings.Repeat(pw+"\n", sudoPasswordLines), "\n")
}

// sudoMode says how sudo inside a script or command gets its password.
type sudoMode int

const (
	sudoAsIs     sudoMode = iota // leave sudo alone
	sudoStdin                    // read the password from stdin (-S)
	sudoNoPrompt                 // never ask (-n): there is no password to give
)

// scriptCommand turns a script's text into one shell command that writes it to
// a temporary file in the guest, runs it and removes it. A script starting with
// a shebang runs under that interpreter; any other runs in bash (the login
// shell may be zsh, and `sh -c` would be the wrong dialect for a .sh file).
// Writing it to a file keeps stdin free for the sudo password.
func scriptCommand(script string, sudo sudoMode) (string, error) {
	script = strings.ReplaceAll(script, "\r\n", "\n")
	script = strings.ReplaceAll(script, "\r", "\n")
	switch {
	case strings.TrimSpace(script) == "":
		return "", errors.New("the script is empty")
	case len(script) > maxScriptBytes:
		return "", fmt.Errorf("the script is larger than %d KB", maxScriptBytes/1024)
	case strings.ContainsRune(script, 0):
		return "", errors.New("the file doesn't look like a text script")
	}
	if !strings.HasSuffix(script, "\n") {
		script += "\n"
	}
	switch sudo {
	case sudoStdin:
		script = rewriteSudoForStdin(script)
	case sudoNoPrompt:
		script = rewriteSudoNoPrompt(script)
	}
	run := `"$f"`
	if !strings.HasPrefix(script, "#!") {
		run = `/bin/bash "$f"`
	}
	enc := base64.StdEncoding.EncodeToString([]byte(script))
	return fmt.Sprintf(`f=$(/usr/bin/mktemp /tmp/tart-oven-script.XXXXXX) || exit 1; `+
		`printf %%s '%s' | /usr/bin/base64 -D > "$f" && /bin/chmod 700 "$f" && %s; `+
		`rc=$?; /bin/rm -f "$f"; exit $rc`, enc, run), nil
}

package server

import "strings"

// applyTerminalOutput appends chunk to out the way a terminal would show it, for
// the few control sequences progress bars use. Tart redraws its percentage with
// "ESC[1A", "\r", "ESC[J" (cursor up, return, erase) before each new line; a
// plain append would leave the "\r" behind, which the browser renders as an
// extra blank line.
//
// Handled: cursor up (ESC[nA) and erase line (ESC[K) drop the lines they would
// overwrite, a lone "\r" drops the current line so the next text replaces it,
// and "\r\n" is a plain newline. Every other escape sequence is ignored.
//
// carry holds an escape sequence or "\r" split across two writes; pass the
// returned value back in with the next chunk.
func applyTerminalOutput(out, carry, chunk string) (newOut, newCarry string) {
	data := carry + chunk
	buf := []byte(out)
	dropLine := func() {
		if i := lastIndexByte(buf, '\n'); i >= 0 {
			buf = buf[:i+1]
		} else {
			buf = buf[:0]
		}
	}
	for i := 0; i < len(data); i++ {
		c := data[i]
		switch {
		case c == 0x1b:
			if i+1 >= len(data) {
				return string(buf), data[i:]
			}
			if data[i+1] != '[' {
				continue // lone ESC: ignore it
			}
			j := i + 2
			for j < len(data) && (data[j] >= '0' && data[j] <= '9' || data[j] == ';') {
				j++
			}
			if j >= len(data) {
				return string(buf), data[i:] // sequence continues in the next write
			}
			params, final := data[i+2:j], data[j]
			switch final {
			case 'A':
				dropLine()
				for n := atoiDefault(params, 1); n > 0 && len(buf) > 0; n-- {
					buf = buf[:len(buf)-1] // the newline ending the line above
					dropLine()
				}
			case 'K':
				dropLine()
			}
			i = j
		case c == '\r':
			if i+1 >= len(data) {
				return string(buf), "\r" // may be the first half of "\r\n"
			}
			if data[i+1] != '\n' {
				dropLine()
			}
		default:
			buf = append(buf, c)
		}
	}
	return string(buf), ""
}

func lastIndexByte(b []byte, c byte) int {
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] == c {
			return i
		}
	}
	return -1
}

// atoiDefault parses the first number of an escape sequence's parameters.
func atoiDefault(params string, def int) int {
	if i := strings.IndexByte(params, ';'); i >= 0 {
		params = params[:i]
	}
	n := 0
	if params == "" {
		return def
	}
	for _, r := range params {
		n = n*10 + int(r-'0')
		if n > 1000 {
			return 1000
		}
	}
	return n
}

package server

import "testing"

func TestApplyTerminalOutputReplacesTartProgress(t *testing.T) {
	// What `tart create --from-ipsw` writes while downloading (captured with od).
	chunks := []string{
		"Fetching UniversalMac_26.6.2_25G83_Restore.ipsw...\n0%\n",
		"\x1b[1A\r\x1b[J1%\n",
		"\x1b[1A\r\x1b[J2%\n",
	}
	out, carry := "$ tart create vm --from-ipsw x\n", ""
	for _, c := range chunks {
		out, carry = applyTerminalOutput(out, carry, c)
	}
	want := "$ tart create vm --from-ipsw x\nFetching UniversalMac_26.6.2_25G83_Restore.ipsw...\n2%\n"
	if out != want || carry != "" {
		t.Fatalf("out = %q (carry %q), want %q", out, carry, want)
	}
}

func TestApplyTerminalOutputSplitSequences(t *testing.T) {
	whole := "Fetching...\n0%\n\x1b[1A\r\x1b[J1%\n"
	for cut := 1; cut < len(whole); cut++ {
		out, carry := applyTerminalOutput("", "", whole[:cut])
		out, carry = applyTerminalOutput(out, carry, whole[cut:])
		if out != "Fetching...\n1%\n" || carry != "" {
			t.Fatalf("split at %d: out = %q carry = %q", cut, out, carry)
		}
	}
}

func TestApplyTerminalOutputCarriageReturns(t *testing.T) {
	for in, want := range map[string]string{
		"a\r\nb\r\n":         "a\nb\n",
		"10%\r50%\r100%\n":   "100%\n",
		"keep\nold\rnew\n":   "keep\nnew\n",
		"\x1b[31mred\x1b[0m": "red",
		"plain\n":            "plain\n",
		"héllo\r\nwörld\n":   "héllo\nwörld\n",
		"a\n\x1b[2Ab":        "b",
		"x\n\x1b[Ky":         "x\ny",
	} {
		if got, carry := applyTerminalOutput("", "", in); got != want || carry != "" {
			t.Errorf("%q → %q (carry %q), want %q", in, got, carry, want)
		}
	}
}

func TestAppendTaskOutputUsesTerminalRules(t *testing.T) {
	m := newTestManager(t)
	task := &Task{}
	m.appendTaskOutput(task, "0%\n")
	m.appendTaskOutput(task, "\x1b[1A\r\x1b[J1%\n")
	if task.Output != "1%\n" {
		t.Fatalf("output = %q", task.Output)
	}
}

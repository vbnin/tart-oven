package server

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The SecurityAgent dialog's button is "Enrol" on non-US English guests; the
// script matched only "Enroll" and never clicked it.
func TestAutoEnrollScriptClicksBothEnrolSpellings(t *testing.T) {
	for _, label := range []string{`"Enroll"`, `"Enrol"`} {
		if !strings.Contains(autoEnrollScript, label) {
			t.Errorf("autoEnrollScript does not try the %s button label", label)
		}
	}
}

func TestAutoEnrollScriptIsValidBash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "enroll.sh")
	if err := os.WriteFile(path, []byte(autoEnrollScript), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("/bin/bash", "-n", path).CombinedOutput(); err != nil {
		t.Fatalf("bash -n: %v\n%s", err, out)
	}
}

// scriptSection returns the text of autoEnrollScript from the line that starts
// with start up to, and including, the first line after it that is exactly end.
func scriptSection(t *testing.T, start, end string) string {
	t.Helper()
	i := strings.Index(autoEnrollScript, start)
	if i < 0 {
		t.Fatalf("autoEnrollScript has no %q", start)
	}
	rest := autoEnrollScript[i:]
	j := strings.Index(rest, "\n"+end+"\n")
	if j < 0 {
		t.Fatalf("no %q after %q", end, start)
	}
	return rest[:j+1+len(end)+1] // through the end line and its newline
}

// runSheetWait runs the script's own log() and wait_for_real_sheet() with a fake
// osascript that answers with answers[i] on its i-th call (repeating the last
// one), and returns the exit status, the number of osascript calls and the log.
func runSheetWait(t *testing.T, waitSecs int, answers ...string) (int, int, string) {
	t.Helper()
	dir := t.TempDir()
	answersFile := filepath.Join(dir, "answers")
	if err := os.WriteFile(answersFile, []byte(strings.Join(answers, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	harness := `set -u
LOG="` + filepath.Join(dir, "log") + `"
CALLS="` + filepath.Join(dir, "calls") + `"
: > "$CALLS"
osascript() {
  echo x >> "$CALLS"
  n=$(wc -l < "$CALLS" | tr -d ' ')
  line=$(sed -n "${n}p" "` + answersFile + `")
  [ -n "$line" ] || line=$(grep -v '^$' "` + answersFile + `" | tail -1)
  printf '%s\n' "$line"
}
sleep() { :; }
` + scriptSection(t, "log() {", "}") + scriptSection(t, "wait_for_real_sheet() {", "}") + `
SHEET_WAIT_SECS=` + strconv.Itoa(waitSecs) + `
wait_for_real_sheet
echo "status=$?"
echo "calls=$(wc -l < "$CALLS" | tr -d ' ')"
`
	out, err := exec.Command("/bin/bash", "-c", harness).CombinedOutput()
	if err != nil {
		t.Fatalf("harness: %v\n%s", err, out)
	}
	text := string(out)
	status, calls := -1, -1
	for _, line := range strings.Split(text, "\n") {
		if v, ok := strings.CutPrefix(line, "status="); ok {
			status, _ = strconv.Atoi(v)
		}
		if v, ok := strings.CutPrefix(line, "calls="); ok {
			calls, _ = strconv.Atoi(v)
		}
	}
	return status, calls, text
}

// The Jamf School sheet has no element named "Rights", so waiting for that word
// alone always ran out: 2 min 45 s of the 2026-11-07 run were spent in this wait.
func TestRealSheetWaitRecognisesTheJamfSchoolSheetByItsButtons(t *testing.T) {
	status, calls, out := runSheetWait(t, 60,
		"rights=0 anon=4", // bootstrap sheet still up
		"rights=0 anon=2", // real sheet: Cancel and Install
		"rights=0 anon=2",
	)
	if status != 0 || calls != 3 {
		t.Fatalf("status=%d calls=%d, want the sheet found on the 2nd stable look (3 calls)\n%s", status, calls, out)
	}
	if !strings.Contains(out, "detected (2 unnamed buttons)") {
		t.Fatalf("the log should say how the sheet was recognised:\n%s", out)
	}
}

func TestRealSheetWaitStillAcceptsTheJamfProMarkerAtOnce(t *testing.T) {
	status, calls, out := runSheetWait(t, 60, "rights=1 anon=7")
	if status != 0 || calls != 1 || !strings.Contains(out, "(Rights)") {
		t.Fatalf("status=%d calls=%d\n%s", status, calls, out)
	}
}

// One look at 1 or 2 buttons is not enough: it has to hold, so a passing state
// between the two sheets is not taken for the real one.
func TestRealSheetWaitNeedsTwoLooksInARow(t *testing.T) {
	status, calls, out := runSheetWait(t, 60,
		"rights=0 anon=2",
		"rights=0 anon=30", // main window again: start over
		"rights=0 anon=1",
		"rights=0 anon=1",
	)
	if status != 0 || calls != 4 {
		t.Fatalf("status=%d calls=%d, want a reset after the interruption (4 calls)\n%s", status, calls, out)
	}
}

// No buttons (the accessibility tree still settling), the bootstrap sheet, or no
// answer at all never count as ready; the wait gives up at its limit and says so.
func TestRealSheetWaitGivesUpAtItsLimitWhenTheSheetNeverShows(t *testing.T) {
	for _, answer := range []string{"rights=0 anon=0", "rights=0 anon=4", "execution error: System Events got an error"} {
		status, calls, out := runSheetWait(t, 1, answer)
		if status != 1 {
			t.Errorf("%q: status=%d, want 1 (gave up)\n%s", answer, status, out)
		}
		if calls < 1 || !strings.Contains(out, "not detected after 1 s; trying Install anyway") {
			t.Errorf("%q: calls=%d, the log should say it gave up\n%s", answer, calls, out)
		}
	}
}

// The AppleScript helpers are only written out and run inside the guest, so a
// syntax error would surface only there. osacompile (macOS only) checks them
// here without running them.
func TestAutoEnrollScriptAppleScriptsCompile(t *testing.T) {
	if _, err := exec.LookPath("osacompile"); err != nil {
		t.Skip("osacompile not available")
	}
	for _, name := range []string{"sheetstate", "clickgeneric", "dumpproc", "setcheckbox", "clickanyrole"} {
		src := scriptSection(t, "cat > /tmp/ae/"+name+".scpt << 'EOF'", "EOF")
		lines := strings.Split(src, "\n")
		body := strings.Join(lines[1:len(lines)-2], "\n") // drop the cat line and the EOF line
		dir := t.TempDir()
		in := filepath.Join(dir, name+".applescript")
		if err := os.WriteFile(in, []byte(body+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("osacompile", "-o", filepath.Join(dir, name+".scpt"), in).CombinedOutput(); err != nil {
			t.Errorf("%s.scpt does not compile: %v\n%s", name, err, out)
		}
	}
	// notice.js is JavaScript for Automation, so it compiles with -l JavaScript.
	src := scriptSection(t, "cat > /tmp/ae/notice.js << 'EOF'", "EOF")
	lines := strings.Split(src, "\n")
	body := strings.Join(lines[1:len(lines)-2], "\n")
	dir := t.TempDir()
	in := filepath.Join(dir, "notice.js")
	if err := os.WriteFile(in, []byte(body+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("osacompile", "-l", "JavaScript", "-o", filepath.Join(dir, "notice.scpt"), in).CombinedOutput(); err != nil {
		t.Errorf("notice.js does not compile: %v\n%s", err, out)
	}
}

// The Siri panes of Setup Assistant (macOS 26.6.x) stalled the script: "Enable
// Ask Siri" is checked by default, and on "Improve Siri & Dictation" and
// "Select a Siri Voice" Continue stays disabled until a radio button or voice
// is picked, so the generic Continue fallback never found an enabled button.
func TestAutoEnrollScriptHandlesSiriPanes(t *testing.T) {
	for _, want := range []string{
		`"Enable Ask Siri"`,          // uncheck the box
		`"Improve Siri & Dictation"`, // pick Not Now so Continue enables
		`"Not Now"`,
		`"Choose For Me"`,   // voice pane: let Setup Assistant pick
		`clickanyrole.scpt`, // radio buttons are not AXButton
		`setcheckbox.scpt`,
	} {
		if !strings.Contains(autoEnrollScript, want) {
			t.Errorf("autoEnrollScript does not handle the Siri pane piece %s", want)
		}
	}
}

// The guest shows progress and the final result in a small non-activating
// window (notice.js, run with osascript -l JavaScript), so the user at the VM
// screen can see what the script is doing and whether it worked.
func TestAutoEnrollScriptShowsGuestNotices(t *testing.T) {
	for _, want := range []string{
		"notice.js",
		`notice "Enrollment in progress"`,
		`notice "Enrollment complete"`,
		`notice "Enrollment failed"`,
		"NSPanel",
		"NSWindowStyleMaskNonactivatingPanel",
	} {
		if !strings.Contains(autoEnrollScript, want) {
			t.Errorf("autoEnrollScript lacks the guest notice piece %q", want)
		}
	}
}

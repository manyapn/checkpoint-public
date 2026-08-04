//go:build linux

package e2e

// Protection-lifecycle guarantees: the four protection states, the "Last
// complete checkpoint" line, capture arming before the first scan completes,
// restart-does-not-re-run-setup, and protect idempotency. Each of these is a
// promise the CLI makes in its own output, so each is asserted against the real
// binary rather than against the library that implements it.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// agoLine matches the documented "Last complete checkpoint: N ago" rendering.
var agoLine = regexp.MustCompile(`Last complete checkpoint: [^\n]*\d[^\n]* ago`)

func (e *env) statusText() string {
	e.t.Helper()
	return e.MustRun("status", "--root", e.WS, "--store", e.Store)
}

// assertState asserts the human-readable status reports `want` as its
// protection state and NONE of the other three. The states are exclusive, so
// a regression that leaks two of them at once must fail here.
func assertState(t *testing.T, out, want string) {
	t.Helper()
	all := []string{
		"Protection: Protected\n",
		"Protection: Not protected",
		"Protection: Limited protection",
		"Protection: Setting up",
	}
	for _, s := range all {
		has := strings.Contains(out, s)
		if s == want && !has {
			t.Fatalf("status should report %q:\n%s", want, out)
		}
		if s != want && has {
			t.Fatalf("status reports %q as well as %q (states must be exclusive):\n%s", s, want, out)
		}
	}
}

// TestProtectionStatesAndLastCheckpoint pins the protection-state contract:
//   - status reports exactly one of the four protection states;
//   - "Not protected" before any daemon, and status still works, reporting the
//     store's checkpoint count honestly;
//   - "Protected" while the daemon runs;
//   - a stopped Checkpoint reads "Not protected" again (there is no "Paused");
//   - "Last complete checkpoint: N ago" appears WITH and WITHOUT a live daemon
//     and reflects a real checkpoint time (last_ckpt_ns > 0).
func TestProtectionStatesAndLastCheckpoint(t *testing.T) {
	e := newEnv(t)
	e.Write("a.txt", "hello\n")

	// (1) No daemon has ever run: honest "Not protected", zero checkpoints,
	// and no fabricated last-checkpoint time.
	before := e.statusText()
	assertState(t, before, "Protection: Not protected")
	if !strings.Contains(before, "Last complete checkpoint: none yet") {
		t.Fatalf("with no checkpoints the last-checkpoint line must say none yet:\n%s", before)
	}
	st := e.StatusJSON()
	if st.Protected {
		t.Fatalf("protected=true with no daemon: %+v", st)
	}
	if st.Checkpoints != 0 || st.LastCkptNS != 0 {
		t.Fatalf("empty store must report 0 checkpoints and no last ckpt: %+v", st)
	}

	// (2) Protected while the daemon runs.
	e.Protect()
	live := e.statusText()
	assertState(t, live, "Protection: Protected\n")
	if !agoLine.MatchString(live) {
		t.Fatalf("status with a live daemon must show \"Last complete checkpoint: N ago\":\n%s", live)
	}
	st = e.StatusJSON()
	if !st.Protected || st.Checkpoints < 1 || st.LastCkptNS <= 0 {
		t.Fatalf("live daemon status wrong (want protected, >=1 ckpt, real last_ckpt_ns): %+v", st)
	}
	liveCkpts := len(e.HistoryJSON())

	// (3) Stopped: back to "Not protected", never "Paused" and never a
	// fabricated Protected state, while the store's history is still
	// reported honestly, including the last-checkpoint age.
	stopOut := e.MustRun("protect", "--stop", "--store", e.Store, e.WS)
	if !strings.Contains(stopOut, "protection stopped") {
		t.Fatalf("protect --stop must confirm teardown:\n%s", stopOut)
	}
	after := e.statusText()
	assertState(t, after, "Protection: Not protected")
	if strings.Contains(after, "Paused") {
		t.Fatalf("there is no Paused state:\n%s", after)
	}
	if !agoLine.MatchString(after) {
		t.Fatalf("status without a daemon must still show \"Last complete checkpoint: N ago\":\n%s", after)
	}
	st = e.StatusJSON()
	if st.Protected {
		t.Fatalf("protected=true after --stop: %+v", st)
	}
	if st.LastCkptNS <= 0 {
		t.Fatalf("last_ckpt_ns must survive daemon shutdown: %+v", st)
	}
	hist := e.HistoryJSON()
	if len(hist) < liveCkpts {
		t.Fatalf("checkpoints disappeared across shutdown: %d then %d", liveCkpts, len(hist))
	}
	if st.Checkpoints != len(hist) {
		t.Fatalf("status counts %d checkpoints, history lists %d", st.Checkpoints, len(hist))
	}
	if !strings.Contains(after, fmt.Sprintf("Checkpoints on record: %d", len(hist))) {
		t.Fatalf("text status must report the real checkpoint count (%d):\n%s", len(hist), after)
	}
	// "Limited protection" is a real fourth state; it is driven by detected
	// loss and is pinned by TestDaemonIncompleteBaselineDegradedThenRescan
	// (daemon). It is not forced here; assertState only proves it never
	// appears in the three states above.
}

// TestChangesDuringFirstTimeSetupAreCaptured pins the setup-window guarantee:
// changes made during first-time setup are captured and recoverable, because
// watching begins when setup starts, before the initial scan completes.
//
// The workspace is large enough that the first scan takes real time; the file
// is written the instant `protect` returns (which it does as soon as the daemon
// answers, typically mid-scan). Whether the scan was still running is logged;
// the ASSERTION is the guarantee itself: the file is in the checkpoint.
func TestChangesDuringFirstTimeSetupAreCaptured(t *testing.T) {
	e := newEnv(t)
	// A tree big enough that the first scan is still running when protect
	// returns: 20k files of 4 KiB, so the scan has real hashing to do.
	body := strings.Repeat("x", 4095) + "\n"
	for d := 0; d < 100; d++ {
		dir := filepath.Join(e.WS, fmt.Sprintf("pkg%02d", d))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for f := 0; f < 200; f++ {
			p := filepath.Join(dir, fmt.Sprintf("f%03d.txt", f))
			if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	// protect WITHOUT waiting for the setup checkpoint: the whole point is to
	// write while the first scan may still be running. (e.Protect would wait.)
	out, err := e.Run("protect", "--store", e.Store, e.WS)
	if err != nil {
		if strings.Contains(out, "CAP_SYS_ADMIN") || strings.Contains(out, "operation not permitted") {
			t.Skipf("fanotify unavailable in this environment:\n%s", out)
		}
		t.Fatalf("protect failed:\n%s", out)
	}
	t.Logf("protect reported: %s", strings.TrimSpace(out))
	// Mid-scan status queries are answered, and the state shown is
	// "Setting up" (with the running files-scanned count), never a premature
	// "Protected". Asserted only when the scan is observably still running; the
	// scan can legitimately finish first, so the branch is logged, and the
	// arming guarantee below is asserted unconditionally either way.
	mid := e.statusText()
	settingUp := strings.Contains(mid, "Protection: Setting up")
	if settingUp {
		assertState(t, mid, "Protection: Setting up")
		if !strings.Contains(mid, "files scanned so far") {
			t.Fatalf("mid-scan status must report the running files-scanned count:\n%s", mid)
		}
	}
	e.Write("during-setup.txt", "written while setting up\n")
	if settingUp {
		t.Log("first scan was still running when the write landed (the arming window under test)")
	} else {
		t.Logf("first scan finished before the write landed; it must still be captured:\n%s", mid)
	}

	e.WaitCheckpoints(1, 60*time.Second) // the setup checkpoint
	e.MustRun("save", "--root", e.WS, "--store", e.Store)

	hist := e.HistoryJSON() // newest first
	if len(hist) == 0 {
		t.Fatal("no checkpoints after setup + save")
	}
	latest := hist[0].ID

	// Restore the file out of the latest checkpoint into a clean directory:
	// if capture missed it, restore refuses ("not in checkpoint N") and this
	// fails loudly rather than quietly passing.
	target := t.TempDir()
	e.MustRun("restore", "--store", e.Store, "--only", "during-setup.txt", fmt.Sprint(latest), target)
	got, err := os.ReadFile(filepath.Join(target, "during-setup.txt"))
	if err != nil {
		t.Fatalf("file written during setup is not recoverable from checkpoint %d: %v", latest, err)
	}
	if string(got) != "written while setting up\n" {
		t.Fatalf("checkpoint %d holds the wrong content: %q", latest, string(got))
	}
}

// TestRestartDoesNotRerunSetup pins two related rules: a restart over an
// existing store cuts no new setup checkpoint, and checkpoint ids continue
// across a restart rather than restarting: no id is ever reused.
func TestRestartDoesNotRerunSetup(t *testing.T) {
	e := newEnv(t)
	e.Write("a.txt", "one\n")
	e.Protect()
	e.Write("b.txt", "two\n")
	e.MustRun("save", "--root", e.WS, "--store", e.Store)

	first := e.HistoryJSON() // newest first
	if len(first) < 1 {
		t.Fatal("no checkpoints before restart")
	}
	e.MustRun("protect", "--stop", "--store", e.Store, e.WS)
	beforeRestart := e.HistoryJSON()
	maxBefore := -1 // ids start at 0
	seen := map[int]int64{}
	for _, c := range beforeRestart {
		seen[c.ID] = c.TimeNS
		if c.ID > maxBefore {
			maxBefore = c.ID
		}
	}

	// Restart over the SAME store.
	e.Protect()
	e.MustRun("save", "--name", "after-restart", "--root", e.WS, "--store", e.Store)
	afterRestart := e.HistoryJSON()

	setups := 0
	ids := map[int]bool{}
	for _, c := range afterRestart {
		if c.Source == "setup" {
			setups++
		}
		if ids[c.ID] {
			t.Fatalf("checkpoint id %d appears twice; ids must never be reused:\n%+v", c.ID, afterRestart)
		}
		ids[c.ID] = true
	}
	if setups != 1 {
		t.Fatalf("want exactly one setup checkpoint over the store's life, got %d:\n%+v", setups, afterRestart)
	}
	// history is newest-first: ids must strictly decrease down the list.
	for i := 1; i < len(afterRestart); i++ {
		if afterRestart[i-1].ID <= afterRestart[i].ID {
			t.Fatalf("ids are not strictly ordered newest-first: %d then %d", afterRestart[i-1].ID, afterRestart[i].ID)
		}
	}
	// Pre-restart checkpoints survive untouched, and the new one continues the
	// numbering above the old maximum instead of restarting at 1.
	newest := afterRestart[0]
	if newest.Name != "after-restart" {
		t.Fatalf("newest checkpoint should be the post-restart save, got %+v", newest)
	}
	if newest.ID <= maxBefore {
		t.Fatalf("numbering restarted: new checkpoint id %d is not above the pre-restart max %d", newest.ID, maxBefore)
	}
	for id, ns := range seen {
		found := false
		for _, c := range afterRestart {
			if c.ID == id {
				found = true
				if c.TimeNS != ns {
					t.Fatalf("checkpoint %d was rewritten across the restart (time %d -> %d)", id, ns, c.TimeNS)
				}
			}
		}
		if !found {
			t.Fatalf("checkpoint %d vanished across the restart:\n%+v", id, afterRestart)
		}
	}
}

// TestProtectIsIdempotent pins that `protect` is idempotent when already
// protected: a second protect succeeds, says so, and does not start a second
// daemon or disturb the store.
func TestProtectIsIdempotent(t *testing.T) {
	e := newEnv(t)
	e.Write("a.txt", "hello\n")
	e.Protect()

	pidBefore, err := os.ReadFile(filepath.Join(e.Store, "daemon.pid"))
	if err != nil {
		t.Fatalf("standing protection must record a pidfile: %v", err)
	}
	sinceBefore := statusSince(e)
	ckptsBefore := len(e.HistoryJSON())

	out := e.MustRun("protect", "--store", e.Store, e.WS)
	if !strings.Contains(out, "already protected") {
		t.Fatalf("a second protect must report it is already protected:\n%s", out)
	}

	pidAfter, err := os.ReadFile(filepath.Join(e.Store, "daemon.pid"))
	if err != nil {
		t.Fatalf("reading pidfile after second protect: %v", err)
	}
	if string(pidAfter) != string(pidBefore) {
		t.Fatalf("a second daemon was started: pidfile %q -> %q", strings.TrimSpace(string(pidBefore)), strings.TrimSpace(string(pidAfter)))
	}
	if since := statusSince(e); since != sinceBefore {
		t.Fatalf("the daemon was replaced: protecting-since %d -> %d", sinceBefore, since)
	}
	st := e.StatusJSON()
	if !st.Protected {
		t.Fatalf("still-protected status wrong after idempotent protect: %+v", st)
	}
	if got := len(e.HistoryJSON()); got != ckptsBefore {
		t.Fatalf("an idempotent protect cut checkpoints: %d -> %d", ckptsBefore, got)
	}
	if st.Checkpoints != ckptsBefore {
		t.Fatalf("status checkpoint count changed after idempotent protect: %d -> %d", ckptsBefore, st.Checkpoints)
	}
}

// statusSince reads the daemon's protecting-since stamp; a new daemon would
// carry a new one, so it distinguishes "same daemon" from "restarted".
func statusSince(e *env) int64 {
	e.t.Helper()
	var doc struct {
		SinceUnixNS int64 `json:"since_unix_ns"`
	}
	line := e.RawJSON("status", "--json", "--root", e.WS, "--store", e.Store)
	if err := json.Unmarshal([]byte(line), &doc); err != nil {
		e.t.Fatalf("status --json: %v\n%s", err, line)
	}
	return doc.SinceUnixNS
}

//go:build linux

package e2e

// Recovery guarantees, end to end: undo, restore, and salvage. These are the
// promises a user bets their work on: that undoing is itself undoable, that
// restoring is itself undoable, that inspecting a checkpoint elsewhere cannot
// cost them their live tree, that `rm -rf` is survivable into either location,
// and that recovery never writes over something that is still there.
//
// Everything is asserted from outside the product: bytes on disk, the CLI's own
// output, and the documented --json fields.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestRecoveryUndoCutsReachablePreUndoCheckpoint pins the rule that undo cuts a
// pre-undo snapshot before reverting, so the pre-undo state remains
// REACHABLE. Reachable is the load-bearing word, because a snapshot nobody
// can name is not a safety net, so the test finds the id the way a client does
// (history --json, source "pre-undo") and restores it.
func TestRecoveryUndoCutsReachablePreUndoCheckpoint(t *testing.T) {
	e := newEnv(t)
	e.Write("a.txt", "human v1\n")
	e.Protect()

	e.Agent("printf 'agent v2\\n' > " + filepath.Join(e.WS, "a.txt"))
	e.WaitCheckpoints(2, 15*time.Second) // setup + the wrapped turn
	if got := e.Read("a.txt"); got != "agent v2\n" {
		t.Fatalf("agent write did not land: a.txt = %q", got)
	}

	out := e.MustRun("undo", "--root", e.WS, "--store", e.Store)
	if !strings.Contains(out, "pre-undo checkpoint") {
		t.Fatalf("undo never announced a pre-undo checkpoint:\n%s", out)
	}
	if got := e.Read("a.txt"); got != "human v1\n" {
		t.Fatalf("undo did not revert the agent write: a.txt = %q\nundo output:\n%s", got, out)
	}

	pre, ok := recBySource(e.HistoryJSON(), "pre-undo")
	if !ok {
		t.Fatalf("no checkpoint with source \"pre-undo\" in history:\n%s", recHistoryDump(e.HistoryJSON()))
	}
	// The announced id and the one a client can find must be the same id.
	if !strings.Contains(out, fmt.Sprintf("pre-undo checkpoint %d saved", pre.ID)) {
		t.Fatalf("history reports pre-undo checkpoint %d but undo announced something else:\n%s", pre.ID, out)
	}

	// The whole point: the state undo threw away is still recoverable.
	rout := e.MustRun("restore", "--store", e.Store, strconv.Itoa(pre.ID), e.WS)
	if got := e.Read("a.txt"); got != "agent v2\n" {
		t.Fatalf("restoring pre-undo checkpoint %d did not bring the pre-undo state back: a.txt = %q\n%s",
			pre.ID, got, rout)
	}
}

// TestRecoveryRestoreAutoCheckpointsPresentState pins the rule that
// `restore <id>` onto a non-empty tree first auto-creates a checkpoint of the
// present state before overwriting, so the pre-restore state remains
// reachable. Restoring an old checkpoint is destructive; this is what makes it
// reversible, including the extra files a whole-checkpoint restore removes.
func TestRecoveryRestoreAutoCheckpointsPresentState(t *testing.T) {
	e := newEnv(t)
	e.Write("keep.txt", "v1\n")
	e.Protect()
	baseID := e.HistoryJSON()[0].ID // the setup checkpoint holds v1

	e.Write("keep.txt", "v2\n")
	e.Write("later.txt", "made after the baseline\n")
	e.MustRun("save", "--root", e.WS, "--store", e.Store, "--name", "present")
	e.WaitCheckpoints(2, 15*time.Second)

	out := e.MustRun("restore", "--store", e.Store, strconv.Itoa(baseID), e.WS)
	if !strings.Contains(out, "pre-restore checkpoint") {
		t.Fatalf("restore onto a non-empty tree never announced a pre-restore checkpoint:\n%s", out)
	}
	if got := e.Read("keep.txt"); got != "v1\n" {
		t.Fatalf("restore did not roll keep.txt back: %q\n%s", got, out)
	}
	if e.Exists("later.txt") {
		t.Fatalf("whole-checkpoint restore left a path the checkpoint does not contain:\n%s", out)
	}

	pre, ok := recBySource(e.HistoryJSON(), "pre-restore")
	if !ok {
		t.Fatalf("no checkpoint with source \"pre-restore\" in history:\n%s", recHistoryDump(e.HistoryJSON()))
	}
	if !strings.Contains(out, fmt.Sprintf("pre-restore checkpoint %d saved", pre.ID)) {
		t.Fatalf("history reports pre-restore checkpoint %d but restore announced something else:\n%s", pre.ID, out)
	}

	// Undoing the restore: the present state, including the file the restore
	// removed, comes back whole.
	rout := e.MustRun("restore", "--store", e.Store, strconv.Itoa(pre.ID), e.WS)
	if got := e.Read("keep.txt"); got != "v2\n" {
		t.Fatalf("pre-restore checkpoint %d did not restore keep.txt: %q\n%s", pre.ID, got, rout)
	}
	if !e.Exists("later.txt") {
		t.Fatalf("pre-restore checkpoint %d did not bring back the removed file later.txt\n%s", pre.ID, rout)
	}
	if got := e.Read("later.txt"); got != "made after the baseline\n" {
		t.Fatalf("later.txt came back with the wrong bytes: %q", got)
	}
}

// TestRecoveryRestoreIntoNewDirLeavesWorkspaceUntouched pins the rule that
// `restore <id> <new-dir>` materializes the checkpoint into the new directory
// and leaves the current workspace unmodified. Inspecting an old checkpoint
// must never cost the user their live tree, so the assertion is the whole
// workspace fingerprint (content, mode, kind, symlink targets) before and
// after, not just the one file the checkpoint happens to differ on.
func TestRecoveryRestoreIntoNewDirLeavesWorkspaceUntouched(t *testing.T) {
	e := newEnv(t)
	e.Write("src/main.go", "package main // v1\n")
	e.Write("notes.md", "notes v1\n")
	e.Protect()
	baseID := e.HistoryJSON()[0].ID

	// Diverge the live tree from the checkpoint, so a restore that leaked into
	// the workspace would be unmistakable.
	e.Write("src/main.go", "package main // LIVE\n")
	e.Write("live-only.txt", "must survive\n")
	if err := os.Remove(filepath.Join(e.WS, "notes.md")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)

	before := recFingerprint(t, e.WS)
	// Guard against a vacuous comparison: the live tree must actually hold the
	// divergences, or "unchanged" would prove nothing.
	if before["live-only.txt"] == "" || before["src/main.go"] == "" || before["notes.md"] != "" {
		t.Fatalf("live tree is not in the expected pre-restore shape: %v", before)
	}
	dest := filepath.Join(e.dir, "elsewhere")
	out := e.MustRun("restore", "--store", e.Store, strconv.Itoa(baseID), dest)
	after := recFingerprint(t, e.WS)
	if d := recDiff(before, after); d != "" {
		t.Fatalf("restore into %s modified the live workspace:\n%s\nrestore output:\n%s", dest, d, out)
	}

	// …and it really did materialize the checkpoint over there.
	got := recFingerprint(t, dest)
	if b, err := os.ReadFile(filepath.Join(dest, "src/main.go")); err != nil || string(b) != "package main // v1\n" {
		t.Fatalf("checkpoint %d not materialized in %s: src/main.go = %q (err %v)\n%s", baseID, dest, string(b), err, out)
	}
	if _, err := os.ReadFile(filepath.Join(dest, "notes.md")); err != nil {
		t.Fatalf("checkpoint %d did not materialize notes.md in %s: %v\n%s", baseID, dest, err, out)
	}
	if _, ok := got["live-only.txt"]; ok {
		t.Fatalf("%s holds a file the checkpoint never contained:\n%s", dest, out)
	}
	// An empty target has nothing to save, so no pre-restore checkpoint is due.
	if strings.Contains(out, "pre-restore checkpoint") {
		t.Fatalf("restore into an empty new directory cut a pre-restore checkpoint:\n%s", out)
	}
}

// TestRecoveryRmRfRestoresIntoBothLocations pins the headline guarantee:
// deletion never affects recoverability. A recursively deleted project is
// restorable byte-exact from its latest recoverable checkpoint, into the
// original location or a new one. Both halves are exercised through the real
// CLI against a live daemon, because that is how a user meets them.
func TestRecoveryRmRfRestoresIntoBothLocations(t *testing.T) {
	e := newEnv(t)
	// A tree with the things git alone cannot carry, built BEFORE protection so
	// the setup scan captures all of it.
	recMkTree(t, e.WS)
	e.Protect()
	h := e.WaitCheckpoints(1, 15*time.Second)
	id := h[0].ID
	before := recFingerprint(t, e.WS)
	if len(before) < 6 {
		t.Fatalf("fixture tree looks wrong: %v", before)
	}

	// The destructive step, with protection live: the store is out of tree, so
	// the safety net is not part of what gets deleted.
	if err := os.RemoveAll(e.WS); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(e.WS); !os.IsNotExist(err) {
		t.Fatalf("workspace should be gone, stat err = %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	// 1. Back into the original location.
	out := e.MustRun("restore", "--store", e.Store, strconv.Itoa(id), e.WS)
	if d := recDiff(before, recFingerprint(t, e.WS)); d != "" {
		t.Fatalf("restore of checkpoint %d into the original location is not byte-exact:\n%s\n%s", id, d, out)
	}

	// 2. And into a fresh location, from the same checkpoint.
	fresh := filepath.Join(e.dir, "rebuilt")
	out = e.MustRun("restore", "--store", e.Store, strconv.Itoa(id), fresh)
	if d := recDiff(before, recFingerprint(t, fresh)); d != "" {
		t.Fatalf("restore of checkpoint %d into a new location is not byte-exact:\n%s\n%s", id, d, out)
	}
}

// TestRecoveryRecoverSkipsPathsLiveOnDisk pins the rule that `recover` skips
// paths still live on disk and never overwrites an existing file, together
// with the rule that a path any manifest holds is
// restore-by-id territory rather than a recovered file. A file created and
// deleted between checkpoints exists only in the version log, so recover is the
// only way back. But the moment something occupies that path again, recovery
// must step aside instead of writing over it.
func TestRecoveryRecoverSkipsPathsLiveOnDisk(t *testing.T) {
	e := newEnv(t)
	e.Write("tracked.txt", "in the baseline\n")
	e.Protect()

	// Never checkpointed: written after the setup baseline, then deleted.
	e.Write("transient.txt", "only ever in the version log\n")
	transient := filepath.Join(e.WS, "transient.txt")
	if err := os.Remove(transient); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)

	out, ok := recPollFor(t, e, 5*time.Second, func(s string) bool { return strings.Contains(s, transient) })
	if !ok {
		t.Fatalf("recover never offered the deleted transient file %s:\n%s", transient, out)
	}
	// A path the baseline checkpoint holds is not a "recovered file".
	if strings.Contains(out, filepath.Join(e.WS, "tracked.txt")) {
		t.Fatalf("recover offered a path a checkpoint already holds:\n%s", out)
	}

	// Something occupies that path again: recovery must not offer to write over
	// it, and must not touch the bytes that are there.
	const live = "DIFFERENT bytes the user put back\n"
	if err := os.WriteFile(transient, []byte(live), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)

	out2 := e.MustRun("recover", "--store", e.Store, e.WS)
	if strings.Contains(out2, transient) {
		t.Fatalf("recover still offers %s although it is live on disk again:\n%s", transient, out2)
	}
	if !strings.Contains(out2, "no recoverable files") {
		t.Fatalf("expected recover to report nothing recoverable once every path is live:\n%s", out2)
	}
	if got := e.Read("transient.txt"); got != live {
		t.Fatalf("the live file was overwritten by recovery: %q", got)
	}
}

// --- helpers (prefixed rec* to stay out of the other e2e files' way) ---

// recBySource returns the newest checkpoint carrying the given source label.
func recBySource(h []Checkpoint, source string) (Checkpoint, bool) {
	for _, c := range h { // history is newest-first
		if c.Source == source {
			return c, true
		}
	}
	return Checkpoint{}, false
}

func recHistoryDump(h []Checkpoint) string {
	var b strings.Builder
	for _, c := range h {
		fmt.Fprintf(&b, "  #%d source=%q name=%q badge=%q\n", c.ID, c.Source, c.Name, c.Badge)
	}
	return b.String()
}

// recPollFor reruns `recover` until pred holds or the deadline passes, so the
// test waits on the observable condition rather than on a guessed sleep.
func recPollFor(t *testing.T, e *env, within time.Duration, pred func(string) bool) (string, bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	var out string
	for {
		out = e.MustRun("recover", "--store", e.Store, e.WS)
		if pred(out) {
			return out, true
		}
		if time.Now().After(deadline) {
			return out, false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// recMkTree builds a fixture exercising the kinds a checkpoint must carry:
// nested files, an exec bit, odd permissions, an empty directory, a symlink.
func recMkTree(t *testing.T, root string) {
	t.Helper()
	write := func(rel, content string, mode os.FileMode) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil { // umask must not decide the fixture
			t.Fatal(err)
		}
	}
	write("README.md", "# project\n", 0o644)
	write("bin/run.sh", "#!/bin/sh\necho go\n", 0o755)
	write("src/main.go", "package main\n", 0o644)
	write("src/secret.go", "package main // 0600\n", 0o600)
	write("data/blob.bin", strings.Repeat("\x00\x01\x02\x03", 4096), 0o644)
	if err := os.MkdirAll(filepath.Join(root, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("src/main.go", filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)
}

// recFingerprint records every path under root as the user would compare it:
// kind, permission bits, content hash, symlink target. Mtimes are deliberately
// excluded, because checkpoint does not promise them.
func recFingerprint(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			out[rel] = "symlink -> " + target
		case fi.IsDir():
			out[rel] = fmt.Sprintf("dir mode=%04o", fi.Mode().Perm())
		default:
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(b)
			out[rel] = fmt.Sprintf("file mode=%04o sha=%s", fi.Mode().Perm(), hex.EncodeToString(sum[:8]))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("fingerprinting %s: %v", root, err)
	}
	return out
}

// recDiff renders the difference between two fingerprints, "" when identical.
func recDiff(want, got map[string]string) string {
	keys := map[string]bool{}
	for k := range want {
		keys[k] = true
	}
	for k := range got {
		keys[k] = true
	}
	var all []string
	for k := range keys {
		all = append(all, k)
	}
	sort.Strings(all)
	var b strings.Builder
	for _, k := range all {
		w, okW := want[k]
		g, okG := got[k]
		switch {
		case !okG:
			fmt.Fprintf(&b, "  missing: %s (%s)\n", k, w)
		case !okW:
			fmt.Fprintf(&b, "  unexpected: %s (%s)\n", k, g)
		case w != g:
			fmt.Fprintf(&b, "  changed: %s: %s -> %s\n", k, w, g)
		}
	}
	return b.String()
}

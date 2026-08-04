//go:build linux

package e2e

// Contract and safety guarantees: capture is passive and out-of-tree,
// checkpoint numbering survives a restart, and the --json output is a stable
// client contract served by a local-socket-only daemon. These are promises a
// user reads in the docs and a client program depends on, so they are asserted
// against observable state only: raw JSON bytes, the set of paths on disk, file
// contents, and the socket inode.

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// listFieldsAlwaysArrays checks the wire form of one --json document. A Go
// client unmarshalling into []string cannot tell `[]`, `null` and an absent key
// apart; a JS client doing `doc.outside.length` throws on the last two. The
// contract is therefore about the BYTES, not the decoded value.
func listFieldsAlwaysArrays(t *testing.T, what, raw string, fields ...string) {
	t.Helper()
	if raw == "" {
		t.Fatalf("%s: no JSON document in output", what)
	}
	if strings.Contains(raw, ":null") {
		t.Errorf("%s: JSON contains a null field (client contract: list fields are always arrays)\n%s", what, raw)
	}
	for _, f := range fields {
		if !strings.Contains(raw, `"`+f+`":[`) {
			t.Errorf("%s: list field %q is not present as an array (absent or null breaks a JS client)\n%s", what, f, raw)
		}
	}
}

// TestJSONListFieldsAreAlwaysArrays pins the wire contract: `checkpoint status
// --json` and `checkpoint history --json` emit stable JSON in which list fields
// are always present arrays, never null. Checked on BOTH an empty store (nothing
// protected, no checkpoints) and a populated one, because the empty case is
// exactly where a nil slice leaks onto the wire.
func TestJSONListFieldsAreAlwaysArrays(t *testing.T) {
	e := newEnv(t)

	// --- empty store: no daemon, no checkpoints ---
	listFieldsAlwaysArrays(t, "status --json (empty store)",
		e.RawJSON("status", "--json", "--root", e.WS, "--store", e.Store),
		"protected_dirs", "outside")
	listFieldsAlwaysArrays(t, "history --json (empty store)",
		e.RawJSON("history", "--json", "--root", e.WS, "--store", e.Store),
		"checkpoints")

	// --- populated store: protected, with agent and human writes checkpointed ---
	e.Write("a.txt", "hello\n")
	e.Protect()
	e.Agent("echo agent > " + filepath.Join(e.WS, "agent.txt"))
	e.Write("human.txt", "human\n")
	e.MustRun("save", "--name", "labelled", "--root", e.WS, "--store", e.Store)
	e.WaitCheckpoints(2, 10*time.Second)

	listFieldsAlwaysArrays(t, "status --json (protected)",
		e.RawJSON("status", "--json", "--root", e.WS, "--store", e.Store),
		"protected_dirs", "outside")
	rawHist := e.RawJSON("history", "--json", "--root", e.WS, "--store", e.Store)
	listFieldsAlwaysArrays(t, "history --json (populated)", rawHist, "checkpoints", "exceptions")
}

// wsPaths returns every path under the workspace, relative and sorted, so the
// set of files on disk can be compared against the set the user created.
func wsPaths(t *testing.T, root string) []string {
	t.Helper()
	var got []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if d.IsDir() {
			rel += "/"
		}
		got = append(got, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walking workspace: %v", err)
	}
	sort.Strings(got)
	return got
}

// TestCaptureNeverWritesIntoProtectedFolder pins passivity: while watching,
// checkpoint never writes or modifies any file in the protected folders; every
// recording write goes to its out-of-tree store. The full lifecycle runs
// (protect, agent write, human write, save, undo) and then the workspace must
// contain nothing but what the user put there.
func TestCaptureNeverWritesIntoProtectedFolder(t *testing.T) {
	e := newEnv(t)
	e.Write("a.txt", "hello\n")
	e.Write("sub/nested.txt", "nested\n")
	e.Protect()

	e.Write("human.txt", "human\n")
	e.MustRun("save", "--root", e.WS, "--store", e.Store)
	e.Agent("echo agent > " + filepath.Join(e.WS, "agent.txt"))
	e.WaitCheckpoints(3, 10*time.Second)
	e.MustRun("undo", "--root", e.WS, "--store", e.Store)

	// Everything the user created; undo may legitimately remove agent.txt, so
	// it is allowed-but-not-required. Nothing ELSE may appear.
	allowed := map[string]bool{
		"a.txt": true, "sub/": true, "sub/nested.txt": true,
		"human.txt": true, "agent.txt": true,
	}
	for _, rel := range wsPaths(t, e.WS) {
		if !allowed[rel] {
			t.Errorf("checkpoint created %q inside the protected folder; all bookkeeping belongs in the out-of-tree store", rel)
		}
	}
	// Named bookkeeping artifacts, checked explicitly so a regression names
	// itself. Every entry is a name the product really writes into a store
	// (daemon.sock/pid/log, store.json, versionlog/, manifests/, objects/,
	// operation.json), plus the in-tree store directory the CLI can name
	// (.checkpoint-store) and two plausible dot-directory variants. None of
	// them may ever appear in a user's workspace.
	for _, forbidden := range []string{
		"daemon.sock", "daemon.pid", "daemon.log", "store.json",
		"versionlog", "manifests", "objects", "operation.json",
		".checkpoint", ".checkpoint-store", ".checkpoints",
	} {
		if e.Exists(forbidden) {
			t.Errorf("bookkeeping artifact %q appeared inside the workspace", forbidden)
		}
	}
	// The store's crash-safety temp files (`tmp-`, `meta-`, `op-`, `vlog-`)
	// carry random suffixes, so they are matched by prefix rather than by name.
	for _, rel := range wsPaths(t, e.WS) {
		for _, prefix := range []string{"tmp-", "meta-", "op-", "vlog-"} {
			if strings.HasPrefix(filepath.Base(rel), prefix) {
				t.Errorf("store temp file %q appeared inside the workspace", rel)
			}
		}
	}
	// The human's own files must still be there and untouched by the lifecycle.
	if !e.Exists("a.txt") || e.Read("a.txt") != "hello\n" {
		t.Errorf("pre-existing human file a.txt = %q, want %q", e.Read("a.txt"), "hello\n")
	}
	if !e.Exists("human.txt") || e.Read("human.txt") != "human\n" {
		t.Errorf("human-written human.txt was modified or removed")
	}
	// And the store really is out of tree.
	if strings.HasPrefix(filepath.Clean(e.Store)+string(filepath.Separator), filepath.Clean(e.WS)+string(filepath.Separator)) {
		t.Fatalf("test setup wrong: store %s is inside workspace %s", e.Store, e.WS)
	}
}

// TestWorkspaceChangesOnlyOnExplicitUndoRestore pins the other half of
// passivity: workspace files are rewritten or deleted only during an explicit,
// user-invoked undo/restore command; capture never modifies them. Protection
// runs, writes happen, and NO checkpoint command is invoked, so this one
// cannot use WaitCheckpoints (running the CLI would destroy the premise) and
// sleeps past the settle window instead.
func TestWorkspaceChangesOnlyOnExplicitUndoRestore(t *testing.T) {
	e := newEnv(t)
	e.Write("keep.txt", "original\n")
	e.Write("doomed.txt", "goes away\n")
	e.Protect()

	// Writes of every shape capture reacts to: modify, create, create-in-subdir,
	// delete. None of them is a checkpoint command.
	e.Write("keep.txt", "rewritten by the human\n")
	e.Write("fresh.txt", "brand new\n")
	e.Write("sub/deep.txt", "deep\n")
	if err := os.Remove(filepath.Join(e.WS, "doomed.txt")); err != nil {
		t.Fatal(err)
	}

	// Well past the 250ms settle window and its 2s hard ceiling, and far short
	// of the 5m autosave interval: a boundary would fire here if capture were
	// going to touch anything on its own.
	time.Sleep(3 * time.Second)

	want := map[string]string{
		"keep.txt":     "rewritten by the human\n",
		"fresh.txt":    "brand new\n",
		"sub/deep.txt": "deep\n",
	}
	for rel, content := range want {
		if !e.Exists(rel) {
			t.Errorf("%s was deleted by passive capture", rel)
			continue
		}
		if got := e.Read(rel); got != content {
			t.Errorf("%s = %q, want %q; passive capture rewrote a workspace file", rel, got, content)
		}
	}
	if e.Exists("doomed.txt") {
		t.Error("doomed.txt was resurrected without an explicit undo/restore")
	}
	allowed := map[string]bool{"keep.txt": true, "fresh.txt": true, "sub/": true, "sub/deep.txt": true}
	for _, rel := range wsPaths(t, e.WS) {
		if !allowed[rel] {
			t.Errorf("passive capture added %q to the workspace", rel)
		}
	}
}

// TestCheckpointIDsSurviveDaemonRestart pins that checkpoint numbering survives
// a daemon restart unchanged. Ids must keep climbing across the
// restart and must never be reused, because a reused id would silently
// overwrite recoverable history.
func TestCheckpointIDsSurviveDaemonRestart(t *testing.T) {
	e := newEnv(t)
	e.Write("a.txt", "one\n")
	e.Protect()

	// --name always cuts, so the count is deterministic regardless of whether
	// the window looks empty.
	e.MustRun("save", "--name", "before-1", "--root", e.WS, "--store", e.Store)
	e.Write("b.txt", "two\n")
	e.MustRun("save", "--name", "before-2", "--root", e.WS, "--store", e.Store)
	before := ids(e.HistoryJSON())
	if len(before) < 3 { // setup + two named
		t.Fatalf("expected at least 3 checkpoints before restart, got %v", before)
	}

	e.StopProtect()
	e.Protect() // same store, fresh daemon process

	e.Write("c.txt", "three\n")
	e.MustRun("save", "--name", "after-1", "--root", e.WS, "--store", e.Store)
	e.Write("d.txt", "four\n")
	e.MustRun("save", "--name", "after-2", "--root", e.WS, "--store", e.Store)
	after := ids(e.HistoryJSON())

	// Every pre-restart checkpoint still exists, with its id unchanged.
	seen := map[int]bool{}
	for _, id := range after {
		if seen[id] {
			t.Fatalf("checkpoint id %d appears twice after restart: %v", id, after)
		}
		seen[id] = true
	}
	maxBefore := 0
	for _, id := range before {
		if !seen[id] {
			t.Errorf("checkpoint %d vanished across the daemon restart (ids after: %v)", id, after)
		}
		if id > maxBefore {
			maxBefore = id
		}
	}
	// The two post-restart saves got fresh ids above every pre-restart id.
	fresh := 0
	for _, id := range after {
		if id > maxBefore {
			fresh++
		}
	}
	if fresh < 2 {
		t.Errorf("expected >=2 new ids above the pre-restart max %d; ids after restart: %v", maxBefore, after)
	}
	// Ids are strictly increasing over the whole history (history is newest-first).
	for i := 1; i < len(after); i++ {
		if after[i-1] <= after[i] {
			t.Fatalf("ids not strictly ordered newest-first: %v", after)
		}
	}
	// Named checkpoints from before the restart are still addressable by name.
	hist := e.HistoryJSON()
	names := map[string]bool{}
	for _, c := range hist {
		names[c.Name] = true
	}
	for _, n := range []string{"before-1", "before-2", "after-1", "after-2"} {
		if !names[n] {
			t.Errorf("named checkpoint %q missing after restart", n)
		}
	}
}

func ids(cps []Checkpoint) []int {
	out := make([]int, 0, len(cps))
	for _, c := range cps {
		out = append(out, c.ID)
	}
	return out
}

// TestDaemonServesOnlyALocalUnixSocket pins the transport: the CLI and TUI talk
// to the daemon over a local Unix-domain socket, and no network port is opened.
// Network absence is not directly observable from a test that must stay
// deterministic and sandbox-safe, so this asserts what IS observable: the
// daemon's only endpoint is a Unix socket inode inside the out-of-tree store,
// it is reachable enough to answer status, it lives nowhere in the workspace,
// and stopping protection removes it.
func TestDaemonServesOnlyALocalUnixSocket(t *testing.T) {
	e := newEnv(t)
	e.Write("a.txt", "hello\n")
	e.Protect()

	sock := filepath.Join(e.Store, "daemon.sock")
	fi, err := os.Lstat(sock)
	if err != nil {
		t.Fatalf("expected a Unix socket at %s while protected: %v", sock, err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		t.Fatalf("%s is not a socket (mode %s)", sock, fi.Mode())
	}
	if fi.Mode()&os.ModeType != os.ModeSocket {
		t.Fatalf("%s has unexpected file type (mode %s)", sock, fi.Mode())
	}
	// The socket is live: status over it reports protection, which is only
	// answerable by the daemon on the other end.
	if st := e.StatusJSON(); !st.Protected {
		t.Fatalf("daemon socket exists but status does not report protection: %+v", st)
	}

	// No socket (and no pidfile) anywhere inside the protected workspace.
	err = filepath.WalkDir(e.WS, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		if info.Mode()&os.ModeSocket != 0 {
			t.Errorf("daemon socket %q found inside the protected workspace", p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking workspace: %v", err)
	}

	e.StopProtect()
	if _, err := os.Lstat(sock); !os.IsNotExist(err) {
		t.Errorf("daemon.sock still present after `protect --stop` (lstat err: %v)", err)
	}
	if _, err := os.Lstat(filepath.Join(e.Store, "daemon.pid")); !os.IsNotExist(err) {
		t.Errorf("daemon.pid still present after `protect --stop` (lstat err: %v)", err)
	}
	// And the CLI now honestly reports the project as unprotected.
	if st := e.StatusJSON(); st.Protected {
		t.Errorf("status still claims protection after --stop: %+v", st)
	}
}

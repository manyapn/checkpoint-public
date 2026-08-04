//go:build linux

package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/manyapn/checkpoint-public/internal/capture"
	"github.com/manyapn/checkpoint-public/internal/objstore"
	"github.com/manyapn/checkpoint-public/internal/provenance"
	"github.com/manyapn/checkpoint-public/internal/store"
	"github.com/manyapn/checkpoint-public/internal/versionlog"
)

func startDaemonOrSkip(t *testing.T, cfg Config) (stop func()) {
	t.Helper()
	ready := make(chan struct{})
	stopCh := make(chan struct{})
	errCh := make(chan error, 1)
	go func() { errCh <- Serve(cfg, ready, stopCh) }()
	select {
	case <-ready:
	case err := <-errCh:
		if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOSYS) {
			t.Skipf("fanotify unavailable in this environment: %v", err)
		}
		t.Fatalf("Serve: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not become ready")
	}
	return func() { close(stopCh); <-errCh }
}

// setSettle overrides the settle constants for a test and restores them after.
func setSettle(t *testing.T, quiet, ceiling time.Duration) {
	t.Helper()
	oq, oc := settleQuiet, hardCeiling
	settleQuiet, hardCeiling = quiet, ceiling
	t.Cleanup(func() { settleQuiet, hardCeiling = oq, oc })
}

// TestDaemonWrappedCommandOneCheckpointByteExact: a boundary request (as
// `checkpoint run` sends on command exit) produces exactly one checkpoint; the
// settle absorbs a write that closes shortly after the boundary; and after
// `rm -rf` the checkpoint restores byte-exact.
func TestDaemonWrappedCommandOneCheckpointByteExact(t *testing.T) {
	setSettle(t, 200*time.Millisecond, 2*time.Second)
	root := t.TempDir()
	storeDir := t.TempDir()
	stop := startDaemonOrSkip(t, Config{Workspace: root, StoreDir: storeDir})

	// The "wrapped command": several writes and a delete (a transient).
	writeFile(t, filepath.Join(root, "a.txt"), "alpha\n")
	writeFile(t, filepath.Join(root, "sub/b.txt"), "beta\n")
	writeFile(t, filepath.Join(root, "scratch.tmp"), "temp\n")
	if err := os.Remove(filepath.Join(root, "scratch.tmp")); err != nil { // gone before the boundary
		t.Fatal(err)
	}

	// A write that closes shortly AFTER the boundary request: the settle must
	// absorb it (reset the quiet timer), so it lands in this checkpoint.
	go func() {
		time.Sleep(80 * time.Millisecond)
		writeFile(t, filepath.Join(root, "trailing.txt"), "arrived during settle\n")
	}()

	resp, err := RequestCheckpoint(SocketPath(storeDir), "run: build")
	if err != nil {
		t.Fatalf("RequestCheckpoint: %v", err)
	}
	// ID 0 is the automatic setup checkpoint (streaming setup); the boundary is 1.
	if resp.ID != 1 || resp.SettleTimedOut || resp.Coverage != "DURABLE" {
		t.Fatalf("unexpected response: %+v", resp)
	}
	// Exactly one checkpoint per boundary: setup + this one. Asserted BEFORE the
	// shutdown, because a clean shutdown flushes its own final checkpoint
	// (TestShutdownCutsFinalCheckpoint). What is pinned here is the count
	// produced by the BOUNDARY REQUEST, which is what this test is about.
	if next, _ := store.NextID(storeDir); next != 2 {
		t.Fatalf("expected setup + one boundary checkpoint (next id 2), got next id %d", next)
	}

	stop() // freeze: no more capture while we mutate + restore

	// The trailing write was absorbed by the settle.
	if _, err := os.Stat(filepath.Join(root, "trailing.txt")); err != nil {
		t.Fatalf("trailing.txt should exist on disk: %v", err)
	}
	m, err := store.Load(storeDir, resp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Entries["trailing.txt"]; !ok {
		t.Fatal("settle should have absorbed trailing.txt into the checkpoint")
	}
	if _, ok := m.Entries["scratch.tmp"]; ok {
		t.Fatal("a file deleted before the boundary must not be in the manifest")
	}
	if m.Source != "run: build" {
		t.Fatalf("checkpoint source not recorded: %q", m.Source)
	}

	before := fingerprint(t, root)

	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	oc, err := objstore.Open(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Restore(m, oc, root); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	after := fingerprint(t, root)
	if diff := diffFP(before, after); diff != "" {
		t.Fatalf("restored tree not byte-exact:\n%s", diff)
	}
}

// TestDaemonSettleTimeoutHonesty: a writer that stays active past the hard
// ceiling yields a checkpoint marked settle-timed-out, rather than pretending the
// boundary was fully quiescent.
func TestDaemonSettleTimeoutHonesty(t *testing.T) {
	setSettle(t, 200*time.Millisecond, 600*time.Millisecond)
	root := t.TempDir()
	storeDir := t.TempDir()
	stop := startDaemonOrSkip(t, Config{Workspace: root, StoreDir: storeDir})
	defer stop()

	// A writer that never lets the workspace go quiet for 200ms.
	stopWriter := make(chan struct{})
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		i := 0
		for {
			select {
			case <-stopWriter:
				return
			default:
			}
			writeFile(t, filepath.Join(root, "busy.txt"), time.Now().String())
			i++
			// Well under the 200ms quiet window: under full-suite load a
			// goroutine sleeping 80ms can be starved past 200ms, faking quiet
			// and turning this into a flaky assertion.
			time.Sleep(30 * time.Millisecond)
		}
	}()

	resp, err := RequestCheckpoint(SocketPath(storeDir), "run: never-settles")
	close(stopWriter)
	<-writerDone
	if err != nil {
		t.Fatalf("RequestCheckpoint: %v", err)
	}
	if !resp.SettleTimedOut {
		t.Fatal("a writer active past the ceiling must yield settle_timed_out=true")
	}
	m, err := store.Load(storeDir, resp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !m.SettleTimedOut {
		t.Fatal("the persisted manifest must record settle_timed_out")
	}
}

// TestBoundaryCarriesAgentTurnSource proves the boundary protocol is
// source-agnostic: an agent-turn label (what a Claude Code Stop hook would pass
// via `checkpoint save --source`) flows through the same request onto the
// manifest. The agent adapter is just another caller, with no separate semantics.
func TestBoundaryCarriesAgentTurnSource(t *testing.T) {
	setSettle(t, 100*time.Millisecond, 2*time.Second)
	root := t.TempDir()
	storeDir := t.TempDir()
	stop := startDaemonOrSkip(t, Config{Workspace: root, StoreDir: storeDir})
	defer stop()

	writeFile(t, filepath.Join(root, "edited.go"), "package main\n")

	const src = "Claude Code · turn"
	resp, err := RequestCheckpoint(SocketPath(storeDir), src)
	if err != nil {
		t.Fatalf("RequestCheckpoint: %v", err)
	}
	m, err := store.Load(storeDir, resp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if m.Source != src {
		t.Fatalf("agent-turn source not recorded on manifest: got %q, want %q", m.Source, src)
	}
}

// writeViaChild writes content to path from a short-lived CHILD of this test
// process, then sleeps briefly so the writer's /proc entry is still alive when
// the daemon captures the close-write (avoiding the one-shot lineage race in the
// test). The writing process is a descendant of this test process.
func writeViaChild(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	// bash performs the redirect (open/write/close) itself, then lingers.
	cmd := exec.Command("bash", "-c", "printf %s \"$1\" > \"$2\"; sleep 0.4", "bash", content, path)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
}

func writerFor(t *testing.T, storeDir, suffix string) versionlog.Version {
	t.Helper()
	vs, err := versionlog.Read(filepath.Join(storeDir, "versionlog"))
	if err != nil {
		t.Fatal(err)
	}
	for i := len(vs) - 1; i >= 0; i-- {
		if strings.HasSuffix(vs[i].Path, suffix) {
			return vs[i]
		}
	}
	t.Fatalf("no version captured for %s (have %d versions)", suffix, len(vs))
	return versionlog.Version{}
}

func selfStart(t *testing.T) uint64 {
	t.Helper()
	s, ok := provenance.StartTime(os.Getpid())
	if !ok {
		t.Fatal("could not read own start-time")
	}
	return s
}

// TestDaemonClassifiesAgentDescendantAgent: a write from a descendant of the
// registered agent root attributes to the agent by lineage, via the socket
// registration path a `checkpoint run` uses.
func TestDaemonClassifiesAgentDescendantAgent(t *testing.T) {
	setSettle(t, 100*time.Millisecond, 2*time.Second)
	root := t.TempDir()
	storeDir := t.TempDir()
	stop := startDaemonOrSkip(t, Config{Workspace: root, StoreDir: storeDir})
	defer stop()

	// This test process is the agent root; the writer below descends from it.
	if err := RegisterAgentRoot(SocketPath(storeDir), os.Getpid(), selfStart(t)); err != nil {
		t.Fatal(err)
	}
	writeViaChild(t, filepath.Join(root, "agent.txt"), "by the agent\n")

	if _, err := RequestCheckpoint(SocketPath(storeDir), "run: agent"); err != nil {
		t.Fatal(err)
	}
	if w := writerFor(t, storeDir, "/agent.txt").Writer; w != "agent" {
		t.Fatalf("descendant of the agent root must classify agent, got %q", w)
	}
}

// TestDaemonClassifiesNonDescendantHuman: with an UNRELATED process registered as
// the agent root, a concurrent write from a non-descendant attributes to human.
// Boundary membership does not make it agent.
func TestDaemonClassifiesNonDescendantHuman(t *testing.T) {
	setSettle(t, 100*time.Millisecond, 2*time.Second)
	root := t.TempDir()
	storeDir := t.TempDir()
	stop := startDaemonOrSkip(t, Config{Workspace: root, StoreDir: storeDir})
	defer stop()

	// Register a decoy "agent" that is NOT an ancestor of the writer.
	decoy := exec.Command("sleep", "5")
	if err := decoy.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { decoy.Process.Kill(); decoy.Wait() }()
	decoyStart, _ := provenance.StartTime(decoy.Process.Pid)
	if err := RegisterAgentRoot(SocketPath(storeDir), decoy.Process.Pid, decoyStart); err != nil {
		t.Fatal(err)
	}

	writeViaChild(t, filepath.Join(root, "human.txt"), "by a human, concurrently\n")

	if _, err := RequestCheckpoint(SocketPath(storeDir), "run: decoy"); err != nil {
		t.Fatal(err)
	}
	if w := writerFor(t, storeDir, "/human.txt").Writer; w != "human" {
		t.Fatalf("a non-descendant of the agent root must classify human, got %q", w)
	}
}

// TestProvenancePersistsAcrossRestart: the provenance verdict is durable. After
// the daemon stops, the version log still carries the writer classification.
func TestProvenancePersistsAcrossRestart(t *testing.T) {
	setSettle(t, 100*time.Millisecond, 2*time.Second)
	root := t.TempDir()
	storeDir := t.TempDir()
	stop := startDaemonOrSkip(t, Config{Workspace: root, StoreDir: storeDir})

	if err := RegisterAgentRoot(SocketPath(storeDir), os.Getpid(), selfStart(t)); err != nil {
		t.Fatal(err)
	}
	writeViaChild(t, filepath.Join(root, "persisted.txt"), "durable provenance\n")
	if _, err := RequestCheckpoint(SocketPath(storeDir), "run: persist"); err != nil {
		t.Fatal(err)
	}
	stop() // daemon down; the ledger is now on disk only

	// A fresh read (as a restarted daemon or `recover` would do) still has it.
	if w := writerFor(t, storeDir, "/persisted.txt").Writer; w != "agent" {
		t.Fatalf("writer verdict must survive restart, got %q", w)
	}
}

// TestDaemonStatusOp: the status op reports live protection state over the
// socket: protected root, checkpoint count, and the number of registered agent
// sessions (rising on register, falling on unregister). A store with no daemon
// is "not protected": RequestStatus must fail, not fabricate an answer.
func TestDaemonStatusOp(t *testing.T) {
	setSettle(t, 100*time.Millisecond, 2*time.Second)
	root := t.TempDir()
	storeDir := t.TempDir()
	stop := startDaemonOrSkip(t, Config{Workspace: root, StoreDir: storeDir})

	// The daemon answers status DURING its setup scan (that is the point of the
	// "Setting up" state), so wait for the automatic setup checkpoint to finish.
	var st Status
	deadline := time.Now().Add(5 * time.Second)
	for {
		var err error
		st, err = RequestStatus(SocketPath(storeDir))
		if err != nil {
			t.Fatalf("RequestStatus: %v", err)
		}
		if st.SettingUp && st.Checkpoints != 0 {
			t.Fatalf("during setup there is no complete checkpoint yet: %+v", st)
		}
		if !st.SettingUp {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("setup never finished: %+v", st)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !st.Protected || st.Root != root || st.Checkpoints != 1 || st.AgentSessions != 0 || st.Limited {
		t.Fatalf("fresh daemon status wrong: %+v", st)
	}
	if st.SinceUnixNS <= 0 || st.LastCkptNS <= 0 {
		t.Fatalf("protecting-since / last-checkpoint not stamped: %+v", st)
	}

	if err := RegisterAgentRoot(SocketPath(storeDir), os.Getpid(), selfStart(t)); err != nil {
		t.Fatal(err)
	}
	if st, _ = RequestStatus(SocketPath(storeDir)); st.AgentSessions != 1 {
		t.Fatalf("after register, want 1 agent session, got %+v", st)
	}

	writeFile(t, filepath.Join(root, "s.txt"), "status\n")
	if _, err := RequestCheckpoint(SocketPath(storeDir), "run: status"); err != nil {
		t.Fatal(err)
	}
	if st, _ = RequestStatus(SocketPath(storeDir)); st.Checkpoints != 2 {
		t.Fatalf("after setup + one boundary, want 2 checkpoints, got %+v", st)
	}

	if err := UnregisterAgentRoot(SocketPath(storeDir), os.Getpid(), selfStart(t)); err != nil {
		t.Fatal(err)
	}
	if st, _ = RequestStatus(SocketPath(storeDir)); st.AgentSessions != 0 {
		t.Fatalf("after unregister, want 0 agent sessions, got %+v", st)
	}

	stop()
	if _, err := RequestStatus(SocketPath(storeDir)); err == nil {
		t.Fatal("status against a stopped daemon must error (caller reports Not protected)")
	}
}

// TestDaemonNamesScanExceptions: an item the boundary scan cannot represent (a
// fifo) lands as a NAMED exception on the cut checkpoint, surfaced rather than
// silently omitted, and the checkpoint still restores (DURABLE).
func TestDaemonNamesScanExceptions(t *testing.T) {
	setSettle(t, 100*time.Millisecond, 2*time.Second)
	root := t.TempDir()
	storeDir := t.TempDir()
	stop := startDaemonOrSkip(t, Config{Workspace: root, StoreDir: storeDir})
	defer stop()

	writeFile(t, filepath.Join(root, "code.go"), "package x\n")
	if err := unix.Mkfifo(filepath.Join(root, "pipe"), 0o644); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}

	resp, err := RequestCheckpoint(SocketPath(storeDir), "run: with-fifo")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Coverage != "DURABLE" {
		t.Fatalf("bounded, named loss must stay DURABLE, got %+v", resp)
	}
	m, err := store.Load(storeDir, resp.ID)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, ex := range m.Exceptions {
		if ex.Path == "pipe" && strings.Contains(ex.Reason, "unsupported") {
			found = true
		}
	}
	if !found {
		t.Fatalf("fifo must be a named exception on the manifest, got %+v", m.Exceptions)
	}
	if _, ok := m.Entries["code.go"]; !ok {
		t.Fatal("the rest of the tree must still be captured")
	}
}

// TestDaemonProtectsExtraFolders: a write in an extra protected folder is
// captured (salvage) and lands in the checkpoint under Manifest.Extra; after
// deleting the extra folder, RestoreExtras puts it back in place byte-exact.
// Nested protected roots are rejected at startup.
func TestDaemonProtectsExtraFolders(t *testing.T) {
	setSettle(t, 100*time.Millisecond, 2*time.Second)
	root := t.TempDir()
	extra := t.TempDir()
	storeDir := t.TempDir()
	stop := startDaemonOrSkip(t, Config{Workspace: root, Extra: []string{extra}, StoreDir: storeDir})

	writeFile(t, filepath.Join(root, "in-ws.txt"), "workspace\n")
	writeFile(t, filepath.Join(extra, "app.conf"), "outside the workspace\n")

	resp, err := RequestCheckpoint(SocketPath(storeDir), "run: extra")
	if err != nil {
		t.Fatal(err)
	}
	m, err := store.Load(storeDir, resp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Entries["in-ws.txt"]; !ok {
		t.Fatal("workspace file must be in the manifest")
	}
	if e, ok := m.Extra[extra]["app.conf"]; !ok || e.Kind != store.KindFile {
		t.Fatalf("extra-folder file must be in Manifest.Extra, got %+v", m.Extra)
	}
	// The extra-root write was also captured as a version (provenance ledger).
	if v := writerFor(t, storeDir, "/app.conf"); v.Ref == "" {
		t.Fatal("extra-folder close-write must be captured as a version")
	}
	stop()

	if err := os.RemoveAll(extra); err != nil {
		t.Fatal(err)
	}
	oc, err := objstore.Open(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RestoreExtras(m, oc); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(extra, "app.conf"))
	if err != nil || string(b) != "outside the workspace\n" {
		t.Fatalf("extra folder must restore in place byte-exact: %q err=%v", b, err)
	}

	// Nested roots are a config error, not a silent double-count.
	err = Serve(Config{Workspace: root, Extra: []string{filepath.Join(root, "sub")}, StoreDir: t.TempDir()}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "must not nest") {
		t.Fatalf("nested protected folders must be rejected, got %v", err)
	}
}

// TestDaemonStreamingSetup: a fresh store gets its first full checkpoint at
// daemon START (source "setup"), not at the first boundary request, so protection
// begins immediately. A restart over an existing store cuts nothing new, and
// status carries the last-complete-checkpoint time either way.
func TestDaemonStreamingSetup(t *testing.T) {
	setSettle(t, 100*time.Millisecond, 2*time.Second)
	root := t.TempDir()
	storeDir := t.TempDir()
	writeFile(t, filepath.Join(root, "preexisting.txt"), "here before the daemon\n")

	stop := startDaemonOrSkip(t, Config{Workspace: root, StoreDir: storeDir})

	// The setup checkpoint appears without any boundary request.
	deadline := time.Now().Add(5 * time.Second)
	var ids []int
	for time.Now().Before(deadline) {
		ids, _ = store.IDs(storeDir)
		if len(ids) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(ids) != 1 {
		t.Fatalf("a fresh store must get exactly one setup checkpoint, got ids %v", ids)
	}
	m, err := store.Load(storeDir, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if m.Source != "setup" {
		t.Fatalf("initial checkpoint source must be %q, got %q", "setup", m.Source)
	}
	if _, ok := m.Entries["preexisting.txt"]; !ok {
		t.Fatal("the setup scan must cover the pre-daemon tree")
	}
	st, err := RequestStatus(SocketPath(storeDir))
	if err != nil {
		t.Fatal(err)
	}
	if st.SettingUp || st.LastCkptNS != m.TimeNS {
		t.Fatalf("after setup: SettingUp=false, LastCkptNS=%d; got %+v", m.TimeNS, st)
	}
	stop()

	// The clean shutdown flushes the pending window as its own "shutdown"
	// checkpoint (pinned by TestShutdownCutsFinalCheckpoint), so the baseline
	// for the restart assertions is whatever exists after the stop. The
	// invariant under test is that the RESTART itself cuts nothing new, and
	// that only ever ONE "setup" checkpoint exists for this store.
	idsAfterStop, _ := store.IDs(storeDir)
	last, err := store.Load(storeDir, idsAfterStop[len(idsAfterStop)-1])
	if err != nil {
		t.Fatal(err)
	}

	// Restart over the same store: no second setup checkpoint.
	stop2 := startDaemonOrSkip(t, Config{Workspace: root, StoreDir: storeDir})
	defer stop2()
	st2, err := RequestStatus(SocketPath(storeDir))
	if err != nil {
		t.Fatal(err)
	}
	if st2.LastCkptNS != last.TimeNS {
		t.Fatalf("restart must report the existing last checkpoint, got %+v", st2)
	}
	time.Sleep(150 * time.Millisecond) // give a wrong impl a moment to cut one
	ids2, _ := store.IDs(storeDir)
	if len(ids2) != len(idsAfterStop) {
		t.Fatalf("restart over an existing store must not cut a new checkpoint, got %v (had %v)", ids2, idsAfterStop)
	}
	setups := 0
	for _, id := range ids2 {
		mm, err := store.Load(storeDir, id)
		if err != nil {
			t.Fatal(err)
		}
		if mm.Source == "setup" {
			setups++
		}
	}
	if setups != 1 {
		t.Fatalf("exactly one setup checkpoint must ever exist for a store, found %d in %v", setups, ids2)
	}
}

// ext4Dir provisions a loopback-ext4 mount: container root filesystems are
// usually overlayfs, which refuses the change-feed's filesystem marks.
// Skips when the environment cannot mount.
func ext4Dir(t *testing.T) string {
	t.Helper()
	img := filepath.Join(t.TempDir(), "ext4.img")
	if out, err := exec.Command("dd", "if=/dev/zero", "of="+img, "bs=1M", "count=64").CombinedOutput(); err != nil {
		t.Skipf("dd: %v: %s", err, out)
	}
	if out, err := exec.Command("mkfs.ext4", "-q", img).CombinedOutput(); err != nil {
		t.Skipf("mkfs.ext4: %v: %s", err, out)
	}
	mnt := filepath.Join(t.TempDir(), "mnt")
	if err := os.Mkdir(mnt, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("mount", "-o", "loop", img, mnt).CombinedOutput(); err != nil {
		t.Skipf("cannot mount loopback ext4 (needs privilege): %v: %s", err, out)
	}
	t.Cleanup(func() { exec.Command("umount", mnt).Run() })
	return mnt
}

// TestDaemonDeleteProvenanceExt4: with the change-feed live (ext4), a deletion
// by a descendant of the registered agent root lands in the version log as
// OpDelete writer=agent, the ledger entry that makes an agent-deleted file a
// restorable undo candidate, and the boundary manifest (cut via the fold)
// reflects the deletion. Status reports the feed active.
func TestDaemonDeleteProvenanceExt4(t *testing.T) {
	setSettle(t, 100*time.Millisecond, 2*time.Second)
	mnt := ext4Dir(t)
	root := filepath.Join(mnt, "ws")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	storeDir := filepath.Join(mnt, "store")
	writeFile(t, filepath.Join(root, "doomed.txt"), "the agent will rm this\n")
	writeFile(t, filepath.Join(root, "stays.txt"), "untouched\n")

	stop := startDaemonOrSkip(t, Config{Workspace: root, StoreDir: storeDir})
	defer stop()

	st, err := RequestStatus(SocketPath(storeDir))
	if err != nil {
		t.Fatal(err)
	}
	if !st.FeedActive {
		t.Fatal("the change-feed must be active on ext4")
	}

	if err := RegisterAgentRoot(SocketPath(storeDir), os.Getpid(), selfStart(t)); err != nil {
		t.Fatal(err)
	}
	// The agent (a descendant of this test process) deletes the file, then
	// lingers so its /proc entry is alive when the event is classified.
	// The DELETING process must itself stay alive past the event, or this test
	// measures scheduler luck instead of delete provenance: `bash -c "rm f;
	// sleep"` keeps BASH alive while `rm`, the actual writer, exits
	// immediately and can be reaped before its event is classified, yielding
	// Unknown. That degradation is real, documented, and SAFE by design (an
	// unresolvable writer is never credited to the agent), but it is a
	// different property than the one under test here. Whether it bites is a
	// matter of scheduling, so it appears on some machines and never on others.
	// Hence one process, which unlinks and then lingers:
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable: needed for a single-process deleter that outlives its own event")
	}
	cmd := exec.Command(py, "-c",
		"import os,sys,time; os.remove(sys.argv[1]); time.sleep(0.6)", filepath.Join(root, "doomed.txt"))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()

	resp, err := RequestCheckpoint(SocketPath(storeDir), "run: agent-rm")
	if err != nil {
		t.Fatal(err)
	}
	m, err := store.Load(storeDir, resp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Entries["doomed.txt"]; ok {
		t.Fatal("deleted file must be gone from the boundary manifest (fold)")
	}
	if _, ok := m.Entries["stays.txt"]; !ok {
		t.Fatal("untouched file must persist in the boundary manifest")
	}

	v := writerFor(t, storeDir, "/doomed.txt")
	if v.Op != versionlog.OpDelete || v.Writer != "agent" {
		t.Fatalf("agent deletion must be journaled as OpDelete writer=agent, got %+v", v)
	}
}

// TestDaemonIncompleteBaselineDegradedThenRescan: an overflowed first scan must
// NOT read as "Protected": status stays Limited with baseline_complete=false
// and the setup checkpoint grades Incomplete. Once the backlog drains, the
// daemon auto-runs a clean rescan, after which the baseline is complete and
// status recovers. The overflow itself is
// injected via the overflowCheck seam (the kernel condition itself is
// exercised for real at capture level by TestQueueOverflowDetected; driving a
// genuine overflow all the way through the daemon is not reliably
// reproducible).
func TestDaemonIncompleteBaselineDegradedThenRescan(t *testing.T) {
	setSettle(t, 100*time.Millisecond, 2*time.Second)
	origCheck, origBackoff := overflowCheck, rescanBackoff
	fired := false
	overflowCheck = func(w *capture.Watcher) bool {
		if !fired {
			fired = true
			return true // the setup window is holed
		}
		return w.Overflowed()
	}
	// Long enough that the degraded window is reliably observable before the
	// first rescan fires: the degraded state is itself under assertion here,
	// so it must not be raced away by an immediate rescan.
	rescanBackoff = 400 * time.Millisecond
	defer func() { overflowCheck, rescanBackoff = origCheck, origBackoff }()

	root := t.TempDir()
	storeDir := t.TempDir()
	writeFile(t, filepath.Join(root, "f.txt"), "content\n")
	stop := startDaemonOrSkip(t, Config{Workspace: root, StoreDir: storeDir})
	defer stop()

	// Setup lands as PARTIAL: degraded, no complete baseline, badge Incomplete.
	deadline := time.Now().Add(5 * time.Second)
	var st Status
	for time.Now().Before(deadline) {
		st, _ = RequestStatus(SocketPath(storeDir))
		if !st.SettingUp {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	m0, err := store.Load(storeDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if m0.Coverage != store.PARTIAL {
		t.Fatalf("holed setup scan must persist as PARTIAL, got %s", m0.Coverage)
	}
	// The DEGRADED window itself: daemon running must not read as protected.
	if st.BaselineComplete || !st.Limited {
		t.Fatalf("a holed baseline must read Limited with baseline_complete=false, got %+v", st)
	}

	// The daemon auto-rescans once quiet; the clean rescan completes the
	// baseline and status recovers.
	for time.Now().Before(deadline) {
		st, _ = RequestStatus(SocketPath(storeDir))
		if st.BaselineComplete {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !st.BaselineComplete || st.Limited {
		t.Fatalf("after a clean rescan the baseline must be complete and status recovered, got %+v", st)
	}
	m1, err := store.Load(storeDir, 1)
	if err != nil {
		t.Fatal(err)
	}
	if m1.Source != "setup-rescan" || m1.Coverage != store.DURABLE {
		t.Fatalf("the rescan checkpoint must be a DURABLE setup-rescan, got %+v", m1)
	}
	if _, ok := m1.Entries["f.txt"]; !ok {
		t.Fatal("the rescan must cover the tree")
	}
}

// --- helpers ---

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fingerprint(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == root {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		fi, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			tgt, _ := os.Readlink(p)
			out[rel] = "symlink:" + tgt
		case fi.IsDir():
			out[rel] = "dir"
		default:
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(b)
			out[rel] = "file:" + hex.EncodeToString(sum[:])
		}
		return nil
	})
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	return out
}

func diffFP(before, after map[string]string) string {
	keys := map[string]bool{}
	for k := range before {
		keys[k] = true
	}
	for k := range after {
		keys[k] = true
	}
	var sorted []string
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	out := ""
	for _, k := range sorted {
		if before[k] != after[k] {
			out += "  " + k + ": before=" + before[k] + " after=" + after[k] + "\n"
		}
	}
	return out
}

// TestFeedOverflowAloneCannotManufactureACheckpoint pins that a checkpoint is
// only recorded when it records something. The change feed marks the whole
// filesystem, so unrelated activity elsewhere on the disk can overflow its
// queue. That overflow is honest input: it means "changes here may have been
// missed", and it correctly forces a full scan rather than a fold. What it must
// NOT do is manufacture history. Once the scan has established that the tree is
// identical to the previous checkpoint, there is nothing to record, and cutting
// anyway is how an idle project on a busy machine fills a disk with duplicates.
func TestFeedOverflowAloneCannotManufactureACheckpoint(t *testing.T) {
	setSettle(t, 100*time.Millisecond, 2*time.Second)
	root := t.TempDir()
	storeDir := t.TempDir()
	stop := startDaemonOrSkip(t, Config{Workspace: root, StoreDir: storeDir})
	defer stop()
	awaitSetupDone(t, SocketPath(storeDir))

	writeFile(t, filepath.Join(root, "work.txt"), "turn output\n")
	first, err := RequestCheckpoint(SocketPath(storeDir), "run: turn")
	if err != nil {
		t.Fatal(err)
	}
	if first.SkippedEmpty {
		t.Fatal("a window with a real write must be recorded")
	}

	// Nothing in the workspace changes. Ask again: the window is provably empty,
	// so no new checkpoint, whatever the counters said on the way in.
	second, err := RequestCheckpoint(SocketPath(storeDir), "run: quiet turn")
	if err != nil {
		t.Fatal(err)
	}
	if !second.SkippedEmpty || second.ID != first.ID {
		ids, _ := store.IDs(storeDir)
		t.Fatalf("an unchanged tree must not be recorded again: got id=%d skipped=%v, ids %v",
			second.ID, second.SkippedEmpty, ids)
	}
	ids, err := store.IDs(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 { // setup + the one real turn
		t.Fatalf("expected exactly the setup and the one recorded turn, got %v", ids)
	}
}

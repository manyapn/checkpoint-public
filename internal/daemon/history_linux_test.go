//go:build linux

// History & retention, daemon half: skip-empty boundaries, named
// checkpoints, and the autosave cadence. Everything here is driven through the
// daemon's external surface (Serve + the socket protocol) and verified against
// the store, with no reaching into event-loop internals.
package daemon

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manyapn/checkpoint-public/internal/store"
)

// requestNamedCheckpoint sends a boundary request carrying a Name, which is what
// `checkpoint save --name LABEL` puts on the wire. A named request is an
// ordinary checkpoint request plus the label; there is no separate protocol.
func requestNamedCheckpoint(t *testing.T, sockPath, source, name string) Response {
	t.Helper()
	c, err := net.DialTimeout("unix", sockPath, 5*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", sockPath, err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(30 * time.Second))
	if err := json.NewEncoder(c).Encode(Request{Op: "checkpoint", Source: source, Name: name}); err != nil {
		t.Fatal(err)
	}
	var resp Response
	if err := json.NewDecoder(c).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error != "" {
		t.Fatalf("daemon: %s", resp.Error)
	}
	return resp
}

// awaitSetupDone waits out the streaming first scan so tests start from a
// settled store (setup checkpoint = id 0) and returns the first post-setup
// status.
func awaitSetupDone(t *testing.T, sockPath string) Status {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st, err := RequestStatus(sockPath)
		if err != nil {
			t.Fatalf("RequestStatus: %v", err)
		}
		if !st.SettingUp {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("setup never finished: %+v", st)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// setAutosaveInterval shrinks the autosave cadence for a test and restores it.
func setAutosaveInterval(t *testing.T, d time.Duration) {
	t.Helper()
	old := autosaveInterval
	autosaveInterval = d
	t.Cleanup(func() { autosaveInterval = old })
}

// awaitCheckpointCount polls the store until at least n manifests exist.
func awaitCheckpointCount(t *testing.T, storeDir string, n int, within time.Duration) []int {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		ids, _ := store.IDs(storeDir)
		if len(ids) >= n {
			return ids
		}
		if time.Now().After(deadline) {
			t.Fatalf("wanted %d checkpoints within %v, have %v", n, within, ids)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestSkipEmptyBoundary: a boundary request over a provably unchanged window is
// answered with the EXISTING latest id (skipped_empty=true) and mints no
// manifest. But emptiness is only provable with the change-feed active. A
// delete (no close-write; feed-dirty only) must NOT skip; a NAMED request must
// NOT skip even when empty; and in scan mode (no feed) NOTHING ever skips,
// because deletes are invisible to a scan diff.
func TestSkipEmptyBoundary(t *testing.T) {
	setSettle(t, 100*time.Millisecond, 2*time.Second)
	mnt := ext4Dir(t)
	root := filepath.Join(mnt, "ws")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	storeDir := filepath.Join(mnt, "store")
	stop := startDaemonOrSkip(t, Config{Workspace: root, StoreDir: storeDir})
	defer stop()

	st := awaitSetupDone(t, SocketPath(storeDir))
	if !st.FeedActive {
		t.Fatal("the change-feed must be active on ext4")
	}

	// A real change cuts a real checkpoint (setup is id 0).
	writeFile(t, filepath.Join(root, "a.txt"), "content\n")
	resp1, err := RequestCheckpoint(SocketPath(storeDir), "run: one")
	if err != nil {
		t.Fatal(err)
	}
	if resp1.SkippedEmpty || resp1.ID != 1 {
		t.Fatalf("a non-empty boundary must cut a checkpoint, got %+v", resp1)
	}

	// Nothing changed since: the request is answered with the existing latest
	// id and its coverage, skipped_empty=true, and no new manifest appears.
	resp2, err := RequestCheckpoint(SocketPath(storeDir), "run: empty")
	if err != nil {
		t.Fatal(err)
	}
	if !resp2.SkippedEmpty || resp2.ID != resp1.ID || resp2.Coverage != "DURABLE" {
		t.Fatalf("an empty boundary must skip to the existing latest, got %+v", resp2)
	}
	if next, _ := store.NextID(storeDir); next != 2 {
		t.Fatalf("an empty boundary must not mint a manifest, next id %d", next)
	}

	// A delete has no close-write, so "zero captured versions" alone would call
	// this window empty; the feed's dirty set must force the cut, and the new
	// manifest must drop the file.
	if err := os.Remove(filepath.Join(root, "a.txt")); err != nil {
		t.Fatal(err)
	}
	resp3, err := RequestCheckpoint(SocketPath(storeDir), "run: rm")
	if err != nil {
		t.Fatal(err)
	}
	if resp3.SkippedEmpty || resp3.ID != 2 {
		t.Fatalf("a delete-only window is NOT empty, got %+v", resp3)
	}
	m3, err := store.Load(storeDir, resp3.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m3.Entries["a.txt"]; ok {
		t.Fatal("the deleted file must be gone from the cut manifest")
	}

	// A NAMED request never skips: the user asked for this exact moment to be
	// pinned, empty window or not.
	resp4 := requestNamedCheckpoint(t, SocketPath(storeDir), "save: milestone", "milestone")
	if resp4.SkippedEmpty || resp4.ID != 3 {
		t.Fatalf("a named request must always cut, got %+v", resp4)
	}
	m4, err := store.Load(storeDir, resp4.ID)
	if err != nil {
		t.Fatal(err)
	}
	if m4.Name != "milestone" {
		t.Fatalf("the request's name must land on the manifest, got %q", m4.Name)
	}

	// The skip answer tracks the CURRENT latest, whichever cut produced it.
	resp5, err := RequestCheckpoint(SocketPath(storeDir), "run: empty-again")
	if err != nil {
		t.Fatal(err)
	}
	if !resp5.SkippedEmpty || resp5.ID != resp4.ID {
		t.Fatalf("skip must name the current latest checkpoint, got %+v", resp5)
	}

	// Scan mode (no feed): a REQUESTED boundary is never skipped. The event
	// counters cannot prove emptiness here, because deletes never reach capture,
	// and a requested boundary marks session structure that is worth recording
	// even when its net effect was nothing.
	root2, storeDir2 := t.TempDir(), t.TempDir()
	stop2 := startDaemonOrSkip(t, Config{Workspace: root2, StoreDir: storeDir2})
	defer stop2()
	st2 := awaitSetupDone(t, SocketPath(storeDir2))
	if st2.FeedActive {
		t.Log("tmpdir filesystem supports the change-feed; scan-mode half not exercisable here")
		return
	}
	respScan, err := RequestCheckpoint(SocketPath(storeDir2), "run: empty-scan")
	if err != nil {
		t.Fatal(err)
	}
	if respScan.SkippedEmpty || respScan.ID != 1 {
		t.Fatalf("scan mode must never skip a REQUESTED boundary, got %+v", respScan)
	}
}

// TestNamedCheckpointRecorded: the Name on a save request is copied verbatim
// onto the cut manifest (additive field), alongside the source label; an
// unnamed boundary stays unnamed, since the label is per-checkpoint, never sticky.
func TestNamedCheckpointRecorded(t *testing.T) {
	setSettle(t, 100*time.Millisecond, 2*time.Second)
	root := t.TempDir()
	storeDir := t.TempDir()
	stop := startDaemonOrSkip(t, Config{Workspace: root, StoreDir: storeDir})
	defer stop()

	writeFile(t, filepath.Join(root, "notes.txt"), "v1\n")
	resp := requestNamedCheckpoint(t, SocketPath(storeDir), "save: manual", "before-refactor")
	m, err := store.Load(storeDir, resp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "before-refactor" {
		t.Fatalf("manifest must carry the request's name, got %q", m.Name)
	}
	if m.Source != "save: manual" {
		t.Fatalf("naming must not displace the source label, got %q", m.Source)
	}

	writeFile(t, filepath.Join(root, "notes.txt"), "v2\n")
	resp2, err := RequestCheckpoint(SocketPath(storeDir), "run: plain")
	if err != nil {
		t.Fatal(err)
	}
	m2, err := store.Load(storeDir, resp2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if m2.Name != "" {
		t.Fatalf("an unnamed boundary must stay unnamed, got %q", m2.Name)
	}
}

// TestAutosaveCadenceDuringSession: with an agent session registered and work
// pending, an overdue window cuts a checkpoint with source "autosave" on its
// own. But autosave never fires without a registered session, never cuts an
// empty checkpoint, and resumes only when new work arrives.
func TestAutosaveCadenceDuringSession(t *testing.T) {
	setSettle(t, 100*time.Millisecond, 2*time.Second)
	setAutosaveInterval(t, 300*time.Millisecond)
	root := t.TempDir()
	storeDir := t.TempDir()
	stop := startDaemonOrSkip(t, Config{Workspace: root, StoreDir: storeDir})
	defer stop()
	awaitSetupDone(t, SocketPath(storeDir))

	// A non-empty window with NO registered session never autosaves, however
	// many intervals elapse.
	writeFile(t, filepath.Join(root, "work.txt"), "turn output\n")
	time.Sleep(1 * time.Second) // > 3 intervals
	if ids, _ := store.IDs(storeDir); len(ids) != 1 {
		t.Fatalf("autosave must not fire without a registered session, got ids %v", ids)
	}

	// Register a session: the overdue, non-empty window now cuts by itself,
	// labelled autosave, covering the pending work.
	if err := RegisterAgentRoot(SocketPath(storeDir), os.Getpid(), selfStart(t)); err != nil {
		t.Fatal(err)
	}
	ids := awaitCheckpointCount(t, storeDir, 2, 5*time.Second)
	m, err := store.Load(storeDir, ids[len(ids)-1])
	if err != nil {
		t.Fatal(err)
	}
	if m.Source != "autosave" {
		t.Fatalf("cadence checkpoint must carry source %q, got %q", "autosave", m.Source)
	}
	if _, ok := m.Entries["work.txt"]; !ok {
		t.Fatal("the autosave must cover the pending work")
	}
	if m.Coverage != store.DURABLE {
		t.Fatalf("a clean autosave window must be DURABLE, got %s", m.Coverage)
	}

	// Quiet workspace: the cadence keeps elapsing but nothing changed since the
	// autosave, so there must be no empty repeat.
	//
	// KNOWN INTERMITTENT on small or heavily loaded machines: an extra
	// checkpoint occasionally appears here. Seen on a 2-core CI runner and in a
	// full-suite run on a developer machine, never on an idle 10-core box.
	//
	// What has been RULED OUT by experiment, so nobody repeats the work: it is
	// not foreign filesystem traffic inflating the bounded miss counter. The
	// capture mark is mount-wide, so that was the leading theory, but 10,000
	// file writes on the same mount outside the protected root produced missed=0,
	// no extra checkpoint, and no Limited state. It also did not reproduce in six
	// runs pinned to two cores under saturating IO.
	//
	// Still open: which term of the non-empty test fires. The candidates are a
	// captured version arriving after the cut, a dirty path from the feed, and a
	// queue overflow (the groups do not set FAN_UNLIMITED_QUEUE, so a 16k-event
	// backlog is possible). The failure message below prints each checkpoint's
	// source, coverage, miss count and entry count, which tells those apart on
	// sight: an overflow shows PARTIAL, a late capture shows DURABLE with entries
	// that differ from the previous manifest, and a spurious cut shows DURABLE
	// with identical entries.
	//
	// Do NOT weaken this assertion to silence it. The guarantee that autosave
	// never cuts an empty checkpoint is what stops a quiet machine filling a
	// disk, and if the product really does cut empty checkpoints under load then
	// that is the bug, not the test.
	var reasons []string
	noteAutosaveReason = func(s *server) {
		reasons = append(reasons, fmt.Sprintf("captured=%d dirty=%d missed=%d feedOverflow=%v capOverflow=%v",
			s.captured, s.dirtyPaths(), s.w.Missed(), s.feed != nil && s.feed.Overflowed(), overflowCheck(s.w)))
	}
	t.Cleanup(func() { noteAutosaveReason = func(*server) {} })

	time.Sleep(1 * time.Second)
	if ids, _ := store.IDs(storeDir); len(ids) != 2 {
		var detail strings.Builder
		for _, id := range ids {
			m, err := store.Load(storeDir, id)
			if err != nil {
				fmt.Fprintf(&detail, "\n  id=%d unreadable: %v", id, err)
				continue
			}
			fmt.Fprintf(&detail, "\n  id=%d source=%q coverage=%s missed=%d entries=%d",
				id, m.Source, m.Coverage, m.Missed, len(m.Entries))
		}
		for i, r := range reasons {
			fmt.Fprintf(&detail, "\n  autosave #%d fired because: %s", i+1, r)
		}
		t.Fatalf("autosave must never cut an empty checkpoint, got ids %v%s", ids, detail.String())
	}

	// New work restarts the cadence.
	writeFile(t, filepath.Join(root, "more.txt"), "later output\n")
	ids = awaitCheckpointCount(t, storeDir, 3, 5*time.Second)
	m2, err := store.Load(storeDir, ids[len(ids)-1])
	if err != nil {
		t.Fatal(err)
	}
	if m2.Source != "autosave" {
		t.Fatalf("resumed cadence checkpoint must be an autosave, got %q", m2.Source)
	}
	if _, ok := m2.Entries["more.txt"]; !ok {
		t.Fatal("the resumed autosave must cover the new work")
	}
}

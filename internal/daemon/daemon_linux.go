//go:build linux

package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"

	"github.com/manyapn/checkpoint-public/internal/capture"
	"github.com/manyapn/checkpoint-public/internal/feed"
	"github.com/manyapn/checkpoint-public/internal/provenance"
	"github.com/manyapn/checkpoint-public/internal/store"
)

// Settle policy (internal constants). A boundary request starts
// a timer; each relevant close-write resets it; the checkpoint is cut at the
// first quiet point or at the hard ceiling, and whether the ceiling was hit is
// recorded (honesty signal). Package vars, not consts, so tests can shrink the
// ceiling without a 2 s wait.
var (
	settleQuiet = 250 * time.Millisecond
	hardCeiling = 2 * time.Second
)

// autosaveInterval paces autonomous checkpoints while an agent session is
// registered: a long-running task must produce periodic checkpoints, not only
// one at its end. Package var so tests can shrink it. An autosave never cuts an
// empty window and never fires with no session registered.
var autosaveInterval = 5 * time.Minute

// Config configures the daemon for one watched root.
type Config struct {
	Workspace string   // absolute root to protect
	Extra     []string // additional protected folders (absolute; --protect)
	StoreDir  string   // absolute out-of-tree store
}

type server struct {
	cfg     Config
	w       *capture.Watcher
	nextID  int
	prev    *store.Manifest
	startNS int64

	// captured sums Drain's per-call counts since the last cut (reset alongside
	// ResetWindow): the skip-empty and autosave emptiness checks need "captured
	// versions since last cut", which the Watcher does not track per window.
	// Event-loop-owned. lastCut is when the last checkpoint of ANY kind
	// completed (daemon start before the first); the autosave timer.
	captured int
	lastCut  time.Time

	// Change-feed (nil = this filesystem refused it; scan mode). dirty is the
	// per-root change-set accumulated since the last checkpoint; feedDirty
	// dedupes. Event-loop-owned.
	feed      *feed.Feed
	dirty     map[string]map[string]bool // root -> abs path set
	feedAlive atomic.Bool

	// Read by status connections off the event loop.
	settingUp  atomic.Bool  // first scan of a fresh store still running
	lastCkptNS atomic.Int64 // TimeNS of the last completed checkpoint (0 = none)

	// Baseline-before-Protected: ordinary "Protected" requires one
	// COMPLETE (DURABLE) baseline checkpoint. An overflowed first scan keeps
	// the daemon visibly degraded and schedules automatic clean rescans until
	// one lands. Event-loop-owned except the atomic (read by status conns).
	baselineComplete atomic.Bool
	lastRescan       time.Time

	// setupScanned mirrors store.ScanProgress during the first-time scan so
	// status can show honest progress ("Setting up", with a running
	// files-scanned count).
	setupScanned atomic.Int64
}

// overflowCheck is a seam: the baseline/rescan test injects one overflow. The
// kernel condition itself is exercised for real at capture level by
// TestQueueOverflowDetected; driving a genuine queue overflow all the way
// through the daemon is not reliably reproducible, so the two halves are
// covered by composition rather than in one test.
var overflowCheck = func(w *capture.Watcher) bool { return w.Overflowed() }

// rescanBackoff paces automatic rescan attempts while the baseline is
// incomplete. Package var so tests need not wait.
var rescanBackoff = 2 * time.Second

type reqEnv struct {
	req  Request
	resp chan Response
}

// Serve runs the daemon for cfg until stop is closed. It owns the long-lived
// capture instance (continuously salvaging close-writes between boundaries),
// listens on the store's Unix socket, and serializes checkpoint creation on its
// single event-loop goroutine. On start it recovers cleanly from a prior run:
// checkpoint numbering and the incremental-reuse base come from the store, and
// the version log recovers its own valid prefix (capture.New). If ready is
// non-nil it is closed once the daemon is listening.
func Serve(cfg Config, ready chan<- struct{}, stop <-chan struct{}) error {
	// Protected roots must not nest: a nested extra would double-classify every
	// write under it (and double-snapshot the subtree).
	allRoots := append([]string{cfg.Workspace}, cfg.Extra...)
	for i, a := range allRoots {
		for j, b := range allRoots {
			if i != j && (a == b || strings.HasPrefix(a, b+"/")) {
				return fmt.Errorf("daemon: protected folders must not nest: %s is under %s", a, b)
			}
		}
	}
	// Store validation (the ONE rule): a store that cannot say what it is gets
	// refused with the reason named; a fresh store is stamped with identity.
	meta, err := store.EnsureMeta(cfg.StoreDir, cfg.Workspace)
	if err != nil {
		return err
	}
	// A store stamped for ANOTHER workspace must never be served: its baseline
	// would become this root's incremental base, mixing two projects' history
	// into one store (and one fold). Refuse before listening, with the mismatch
	// named: no READY, no socket.
	if err := store.CheckWorkspaceIdentity(meta, cfg.Workspace); err != nil {
		return err
	}
	w, err := capture.New(capture.Config{Workspace: cfg.Workspace, Extra: cfg.Extra, StoreDir: cfg.StoreDir})
	if err != nil {
		return err
	}
	defer w.Close()

	nextID, err := store.NextID(cfg.StoreDir)
	if err != nil {
		return err
	}
	prev, _, err := store.Latest(cfg.StoreDir)
	if err != nil {
		return err
	}
	s := &server{cfg: cfg, w: w, nextID: nextID, prev: prev, startNS: time.Now().UnixNano(),
		lastCut: time.Now(), dirty: map[string]map[string]bool{}}
	if prev != nil {
		s.lastCkptNS.Store(prev.TimeNS)
		s.baselineComplete.Store(true) // Latest only returns DURABLE manifests
	}
	// The dirent change-feed is an enhancement: delete provenance + O(changes)
	// boundaries. Filesystems that refuse it (overlayfs) stay in scan mode,
	// which is correct, just O(tree) per boundary and without delete attribution.
	if fd, err := feed.New(allRoots); err == nil {
		s.feed = fd
		s.feedAlive.Store(true)
		defer fd.Close()
	}

	sock := SocketPath(cfg.StoreDir)
	os.Remove(sock) // clear a stale socket left by a prior crash
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	defer ln.Close()
	defer os.Remove(sock)

	reqCh := make(chan reqEnv)
	go s.acceptLoop(ln, reqCh)

	if ready != nil {
		close(ready)
	}

	// Streaming setup: capture is already armed (capture.New above), so nothing
	// that happens from here on is lost. A fresh store gets its first full scan
	// NOW, not at the first boundary request. Status reads "Setting up" until
	// this completes; requests arriving meanwhile queue behind it (serialized).
	if s.prev == nil {
		s.settingUp.Store(true)
		// Honest setup progress: status renders "Setting up" with a live
		// files-scanned count from this hook while the first scan runs.
		store.ScanProgress = func(n int) { s.setupScanned.Store(int64(n)) }
		s.drain() // events queued since arming belong to the setup window
		s.cut("setup", "", false)
		store.ScanProgress = nil
		s.settingUp.Store(false)
	}

	for {
		select {
		case <-stop:
			return s.shutdown()
		case env := <-reqCh:
			env.resp <- s.handleBoundary(env.req)
			continue
		default:
		}
		// Between boundaries, keep salvaging close-writes into the version log
		// and folding dirent changes into the dirty set, paced by a poll over
		// both groups so we don't busy-spin.
		if s.pollBoth(100) {
			s.drain()
			s.drainFeed()
		} else if !s.baselineComplete.Load() && time.Since(s.lastRescan) >= rescanBackoff {
			// The first scan's window overflowed, so no complete baseline
			// exists. The backlog has drained (poll timed out with nothing
			// readable): run a clean rescan. It stays degraded, and keeps
			// retrying, until a rescan window is overflow-free.
			s.lastRescan = time.Now()
			s.drain()
			s.drainFeed()
			s.cut("setup-rescan", "", false)
		}
		s.maybeAutosave()
	}
}

// shutdown is the clean-shutdown flush (the stop channel: SIGTERM/SIGINT via
// the CLI, `protect --stop`, or a test's stop func, but NOT a crash, which takes
// none of this path). Writes that closed after the last boundary would
// otherwise survive only as salvage, so shutdown drains everything queued and
// cuts one final checkpoint labelled source "shutdown". Emptiness is decided by
// cut's existing skip-empty rule: an empty window under a live change-feed
// cuts nothing; scan mode never skips because emptiness cannot be proven there.
// The version log is fsynced last, after cut's own drain has appended the final
// batch, so the salvage ledger is durable on exit.
func (s *server) shutdown() error {
	s.drain() // capture anything queued before leaving
	s.drainFeed()
	resp := s.cut("shutdown", "", false)
	if resp.Error != "" {
		// Honest degradation: a shutdown that could not persist the pending
		// window must say so rather than exit 0 as if it had.
		s.w.Sync()
		return fmt.Errorf("shutdown checkpoint failed: %s", resp.Error)
	}
	if err := s.w.Sync(); err != nil {
		return fmt.Errorf("shutdown: flushing the version log: %w", err)
	}
	return nil
}

// drain wraps the Watcher's Drain, summing captured-version counts into the
// per-window total the skip-empty and autosave checks read.
func (s *server) drain() int {
	n := s.w.Drain()
	s.captured += n
	return n
}

// dirtyPaths counts the feed's accumulated change-set across all roots.
func (s *server) dirtyPaths() int {
	n := 0
	for _, set := range s.dirty {
		n += len(set)
	}
	return n
}

// maybeAutosave cuts an "autosave" checkpoint (no settle) when an agent session
// is registered, the last completed cut is older than autosaveInterval, and the
// window is non-empty. Non-empty mirrors the skip-empty rule inverted; in scan
// mode it means >=1 captured version since the last cut (deletes are invisible
// there, but a captured write is proof enough of change). Never fires with no
// session; the timer resets on every cut of any kind.
func (s *server) maybeAutosave() {
	if s.w.AgentSessionCount() == 0 || time.Since(s.lastCut) <= autosaveInterval {
		return
	}
	nonEmpty := s.captured > 0
	if s.feed != nil {
		nonEmpty = nonEmpty || s.dirtyPaths() > 0 || s.w.Missed() > 0 ||
			s.feed.Overflowed() || overflowCheck(s.w)
	}
	if !nonEmpty {
		return
	}
	s.cut("autosave", "", false)
}

// pollBoth waits up to timeoutMs for either the capture group or the change-feed
// to become readable.
func (s *server) pollBoth(timeoutMs int) bool {
	fds := []unix.PollFd{{Fd: int32(s.w.Fd()), Events: unix.POLLIN}}
	if s.feed != nil {
		fds = append(fds, unix.PollFd{Fd: int32(s.feed.Fd()), Events: unix.POLLIN})
	}
	n, err := unix.Poll(fds, timeoutMs)
	if err != nil && err != unix.EINTR {
		return false
	}
	return n > 0
}

// drainFeed folds queued dirent events into the per-root dirty set and records
// delete provenance (classified at event time, like close-writes).
func (s *server) drainFeed() {
	if s.feed == nil {
		return
	}
	for _, e := range s.feed.Drain() {
		root := s.rootOf(e.Path)
		if root == "" {
			continue
		}
		set := s.dirty[root]
		if set == nil {
			set = map[string]bool{}
			s.dirty[root] = set
		}
		set[e.Path] = true
		// Delete provenance: who removed (or renamed away) this file. Dirs are
		// dirty-set only: undo restores files, and dir shape comes from the fold.
		if (e.Op == feed.OpDelete || e.Op == feed.OpRenameFrom) && !e.Dir {
			s.w.RecordDelete(e.Path, e.Pid)
		}
	}
}

func (s *server) rootOf(path string) string {
	for _, r := range append([]string{s.cfg.Workspace}, s.cfg.Extra...) {
		if strings.HasPrefix(path, r+"/") {
			return r
		}
	}
	return ""
}

// acceptLoop accepts connections and decodes one request per connection.
func (s *server) acceptLoop(ln net.Listener, reqCh chan<- reqEnv) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return // listener closed on shutdown
		}
		go s.serveConn(conn, reqCh)
	}
}

func (s *server) serveConn(conn net.Conn, reqCh chan<- reqEnv) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	var req Request
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		json.NewEncoder(conn).Encode(Response{Error: "bad request: " + err.Error()})
		return
	}
	switch req.Op {
	case "register", "unregister":
		// Agent-root (un)registration is fast and thread-safe against capture's
		// own lock, so it is handled here directly and never queued behind a
		// checkpoint settle, which could take up to the hard ceiling.
		id := provenance.Identity{Pid: req.AgentPid, Start: req.AgentStart}
		if req.Op == "register" {
			s.w.RegisterAgentRoot(id)
		} else {
			s.w.UnregisterAgentRoot(id)
		}
		json.NewEncoder(conn).Encode(Response{})
	case "status":
		ids, _ := store.ValidIDs(s.cfg.StoreDir) // torn manifests are not checkpoints
		outside, outsideN := s.w.OutsideWrites()
		json.NewEncoder(conn).Encode(Status{
			Protected:     true,
			Root:          s.cfg.Workspace,
			ProtectedDirs: append([]string{s.cfg.Workspace}, s.cfg.Extra...),
			Outside:       nonNil(outside),
			Checkpoints:   len(ids),
			Limited: s.w.Missed() > 0 || s.w.Overflowed() ||
				!s.baselineComplete.Load() || outsideN > 0,
			OutsideCount:     outsideN,
			Missed:           s.w.Missed(),
			Overflowed:       s.w.Overflowed(),
			AgentSessions:    s.w.AgentSessionCount(),
			SinceUnixNS:      s.startNS,
			SettingUp:        s.settingUp.Load(),
			SetupScanned:     s.setupScanned.Load(),
			LastCkptNS:       s.lastCkptNS.Load(),
			FeedActive:       s.feedAlive.Load(),
			BaselineComplete: s.baselineComplete.Load(),
		})
	case "checkpoint":
		respCh := make(chan Response, 1)
		reqCh <- reqEnv{req: req, resp: respCh} // blocks until the event loop is free (serializes checkpoints)
		json.NewEncoder(conn).Encode(<-respCh)
	default:
		json.NewEncoder(conn).Encode(Response{Error: "unknown op: " + req.Op})
	}
}

// handleBoundary settles, then cuts and persists a boundary-scan manifest. Runs
// on the event-loop goroutine, so it has exclusive use of the capture instance.
func (s *server) handleBoundary(req Request) Response {
	timedOut := s.settle()
	return s.cut(req.Source, req.Name, timedOut)
}

// cut snapshots and persists a checkpoint immediately (no settle). It is used by
// handleBoundary after its settle, by streaming setup at daemon start, and by
// the autosave cadence.
func (s *server) cut(source, name string, timedOut bool) Response {
	s.drain() // the emptiness check and the fold must both see everything queued
	s.drainFeed()
	// Skip-empty: no new manifest when the window is PROVABLY empty, meaning
	// feed active with no overflow (capture overflow included: an overflowed
	// window may hide changes), zero captured versions, zero dirty paths, and
	// zero misses, with the existing latest DURABLE (a PARTIAL prev makes no
	// coverage claim to point back to; setup-rescans must land a real cut).
	// Scan mode never skips: deletes are invisible to capture, so emptiness
	// cannot be proven. A named request never skips either, because the user
	// asked for THIS moment under that name.
	if name == "" && s.feed != nil && s.prev != nil && s.prev.Coverage == store.DURABLE &&
		!s.feed.Overflowed() && !overflowCheck(s.w) &&
		s.captured == 0 && s.dirtyPaths() == 0 && s.w.Missed() == 0 {
		return Response{ID: s.prev.ID, Coverage: string(s.prev.Coverage),
			Entries: len(s.prev.Entries), SkippedEmpty: true}
	}
	// Coverage: an unbounded loss (queue overflow) this window is Incomplete
	// (PARTIAL). Bounded per-file misses stay DURABLE and are carried as the
	// manifest's Missed count, a Recoverable-with-exceptions signal.
	cov := store.DURABLE
	if overflowCheck(s.w) {
		cov = store.PARTIAL
	}
	now := time.Now().UnixNano()
	var m *store.Manifest
	var err error
	// Fold over the change-set when the feed is live with no known hole and we
	// have a base; otherwise full scan (a change-set with a hole must never be
	// folded, because that is how deletions get resurrected).
	if s.feed != nil && !s.feed.Overflowed() && s.prev != nil {
		dirty := map[string][]string{}
		for root, set := range s.dirty {
			for p := range set {
				dirty[root] = append(dirty[root], p)
			}
		}
		m, err = store.SnapshotFold(s.cfg.Workspace, s.w.Objects(), s.prev, s.nextID, now, cov, s.w.Missed(), dirty, s.cfg.Extra...)
	} else {
		m, err = store.Snapshot(s.cfg.Workspace, s.w.Objects(), s.prev, s.nextID, now, cov, s.w.Missed(), s.cfg.Extra...)
	}
	if err != nil {
		return Response{Error: err.Error()}
	}
	m.Source = source
	m.Name = name
	m.SettleTimedOut = timedOut
	// Name the window's capture misses on the manifest (bounded, listed loss).
	for _, p := range s.w.MissedPaths() {
		rel := p
		if r, err := filepath.Rel(s.cfg.Workspace, p); err == nil && !strings.HasPrefix(r, "..") {
			rel = r
		}
		m.Exceptions = append(m.Exceptions, store.Exception{Path: rel, Reason: "write not captured"})
	}
	// Id collision: a CLI pre-op snapshot took this id meanwhile (store.Write
	// refuses to replace rather than destroy the other writer's checkpoint).
	// Re-sync numbering from disk and retry with a fresh id, bounded, because a
	// retry that keeps losing the race must end in a named refusal, never a
	// spin. Never a silent overwrite either way.
	const idRetries = 8
	for attempt := 0; ; attempt++ {
		err := store.Write(s.cfg.StoreDir, m)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrExist) {
			return Response{Error: err.Error()}
		}
		if attempt == idRetries {
			return Response{Error: fmt.Sprintf("checkpoint id kept colliding with another writer after %d retries: %v", idRetries, err)}
		}
		next, nerr := store.NextID(s.cfg.StoreDir)
		if nerr != nil {
			return Response{Error: nerr.Error()}
		}
		s.nextID = next
		m.ID = next
	}
	s.w.ResetWindow() // each checkpoint's status reflects only loss since the previous
	s.captured = 0
	s.dirty = map[string]map[string]bool{}
	if s.feed != nil {
		s.feed.ResetWindow()
	}
	s.prev = m
	s.nextID++
	s.lastCkptNS.Store(m.TimeNS)
	s.lastCut = time.Now() // the autosave timer resets on every cut of any kind
	if cov == store.DURABLE {
		s.baselineComplete.Store(true) // a durable full picture exists from here on
	}
	return Response{ID: m.ID, Coverage: string(cov), Entries: len(m.Entries), SettleTimedOut: timedOut}
}

// settle waits for settleQuiet of write-quiet after the boundary request,
// resetting on each captured close-write, up to hardCeiling. It keeps draining
// throughout so trailing writes are both captured and reflected in the snapshot
// that follows. Returns true iff the ceiling was reached with writes still
// arriving.
func (s *server) settle() (timedOut bool) {
	start := time.Now()
	last := start
	ceiling := start.Add(hardCeiling)
	for {
		if s.drain() > 0 {
			last = time.Now()
		}
		s.drainFeed()
		now := time.Now()
		if now.Sub(last) >= settleQuiet {
			return false
		}
		if !now.Before(ceiling) {
			s.drain() // absorb whatever is queued at the ceiling
			s.drainFeed()
			return true
		}
		s.pollBoth(20) // brief wait so trailing writes are picked up promptly
	}
}

// nonNil guarantees a JSON array rather than null for a list field: clients
// decoding into a slice cannot tell them apart, but a JS client calling
// .length or .map() on null throws. The wire form is part of the contract.
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

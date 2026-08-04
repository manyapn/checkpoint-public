//go:build linux

// Package capture is checkpoint's retained-FD close-write capture: every
// completed write to the protected workspace is captured the moment a writable
// handle closes, read through the kernel-retained fanotify fd, so the content
// survives even if the file is unlinked immediately after. Captured content
// lands in the object store; a version record lands in the salvage log, tagged
// with the writing process's identity.
//
// The package stops there on purpose: it never cuts manifests or decides
// boundaries (that belongs to the store and the daemon's boundary policy). Just:
// close-write -> version. Requires CAP_SYS_ADMIN (fanotify). Scope is
// process-crash consistency, not power-loss.
package capture

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/manyapn/checkpoint-public/internal/objstore"
	"github.com/manyapn/checkpoint-public/internal/provenance"
	"github.com/manyapn/checkpoint-public/internal/store"
	"github.com/manyapn/checkpoint-public/internal/versionlog"
)

const metaSize = 24 // sizeof(struct fanotify_event_metadata)

// Config configures a Watcher.
type Config struct {
	Workspace string   // absolute dir to protect
	Extra     []string // additional protected folders (absolute; --protect)
	StoreDir  string   // absolute out-of-tree store (objects/ + versionlog)
}

// Watcher captures close-writes under a workspace into the object store + version
// log. Not safe for concurrent use across goroutines except that Run may be
// stopped from another goroutine via its stop channel.
type Watcher struct {
	workspace   string
	roots       []string // every protected root: workspace first, then extras
	oc          *objstore.Store
	log         *versionlog.Log
	fan         int
	buf         []byte
	missed      int
	missedPaths []string // the missed writes we could at least NAME (bounded, listed)
	overflowed  bool     // FAN_Q_OVERFLOW seen this window: unbounded, detected loss

	// Provenance: a birth-parent tracker resolves each writer's
	// lineage; agentRoots is the set of registered agent roots (stable
	// identities). Attribution is by lineage terminating at one of these roots,
	// never by which boundary a write fell in.
	tracker    *provenance.Tracker
	scanStop   chan struct{}
	mu         sync.Mutex
	agentRoots map[provenance.Identity]bool

	// Unprotected changes: AGENT-tree writes that landed
	// OUTSIDE every protected root: uncaptured, and surfaced rather than
	// silently ignored. Bounded list + total count, daemon lifetime. Only
	// agent writes: surfacing every process's writes on the mount (browsers,
	// package managers, the OS) would be unusable noise, and the threat model
	// is "the agent wrote somewhere you're not protecting".
	outside      []string
	outsideCount int
	storeDir     string // never warn about our own store writes
}

// maxOutsideListed bounds the retained outside-boundary path list (the count
// keeps counting).
const maxOutsideListed = 20

// bulkExcludes is the default-skip list of rebuildable directories (protect
// meaningful work, not regenerable bulk), plus .git. Matched as path COMPONENTS
// relative to the protected root, never as substrings of the absolute path:
// these names are only meaningful inside the workspace, and a workspace that
// happens to live under a directory of the same name (~/build/proj) is an
// ordinary project. Mirrors store.defaultExcludes, which governs the scan path.
var bulkExcludes = map[string]bool{
	".git": true, "node_modules": true, "build": true, "target": true,
	"dist": true, "__pycache__": true, ".venv": true,
}

// New opens the store + version log and arms a fanotify mount mark for
// FAN_CLOSE_WRITE over the workspace and each extra protected folder. A mount
// mark (not per-dir) is used so a file created in a subdirectory made mid-run is
// still covered; roots on the same mount share one kernel mark (FAN_MARK_ADD is
// idempotent per mount), roots on other mounts get their own. Returns an error
// wrapping unix.EPERM when CAP_SYS_ADMIN is missing (callers/tests can skip).
func New(cfg Config) (*Watcher, error) {
	workspace, err := filepath.Abs(cfg.Workspace)
	if err != nil {
		return nil, err
	}
	roots := []string{workspace}
	for _, ex := range cfg.Extra {
		abs, err := filepath.Abs(ex)
		if err != nil {
			return nil, err
		}
		roots = append(roots, abs)
	}
	oc, err := objstore.Open(cfg.StoreDir)
	if err != nil {
		return nil, err
	}
	log, err := versionlog.Open(filepath.Join(cfg.StoreDir, "versionlog"))
	if err != nil {
		return nil, err
	}
	fan, err := unix.FanotifyInit(unix.FAN_CLASS_NOTIF|unix.FAN_CLOEXEC|unix.FAN_NONBLOCK,
		unix.O_RDONLY|unix.O_CLOEXEC)
	if err != nil {
		log.Close()
		return nil, fmt.Errorf("capture: fanotify_init (needs CAP_SYS_ADMIN): %w", err)
	}
	for _, r := range roots {
		if err := unix.FanotifyMark(fan, unix.FAN_MARK_ADD|unix.FAN_MARK_MOUNT,
			unix.FAN_CLOSE_WRITE, unix.AT_FDCWD, r); err != nil {
			unix.Close(fan)
			log.Close()
			return nil, fmt.Errorf("capture: fanotify_mark %s: %w", r, err)
		}
	}
	w := &Watcher{
		workspace:  workspace,
		roots:      roots,
		oc:         oc,
		log:        log,
		fan:        fan,
		buf:        make([]byte, 256*1024),
		tracker:    provenance.NewTracker(),
		scanStop:   make(chan struct{}),
		agentRoots: map[provenance.Identity]bool{},
	}
	if abs, err := filepath.Abs(cfg.StoreDir); err == nil {
		w.storeDir = abs
	}
	go w.tracker.Run(w.scanStop) // cache birth-parents so reaped one-shot writers stay resolvable
	return w, nil
}

// RegisterAgentRoot marks id (a wrapped command's or agent's stable identity) as
// an agent root: writes whose lineage terminates at it classify as agent. The
// birth-parent scanner spins only while at least one root is registered.
func (w *Watcher) RegisterAgentRoot(id provenance.Identity) {
	w.mu.Lock()
	w.agentRoots[id] = true
	active := len(w.agentRoots) > 0
	w.mu.Unlock()
	w.tracker.SetActive(active)
}

// UnregisterAgentRoot removes an agent root (after its session ends).
func (w *Watcher) UnregisterAgentRoot(id provenance.Identity) {
	w.mu.Lock()
	delete(w.agentRoots, id)
	active := len(w.agentRoots) > 0
	w.mu.Unlock()
	w.tracker.SetActive(active)
}

// AgentSessionCount reports how many agent roots are currently registered.
func (w *Watcher) AgentSessionCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.agentRoots)
}

func (w *Watcher) agentRootSnapshot() map[provenance.Identity]bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	m := make(map[provenance.Identity]bool, len(w.agentRoots))
	for id := range w.agentRoots {
		m[id] = true
	}
	return m
}

// Run processes close-write events until stop is closed, then returns. It polls
// so a closed stop channel is noticed promptly even with no filesystem activity.
func (w *Watcher) Run(stop <-chan struct{}) error {
	pfd := []unix.PollFd{{Fd: int32(w.fan), Events: unix.POLLIN}}
	for {
		select {
		case <-stop:
			w.Drain() // capture anything already queued before leaving
			return nil
		default:
		}
		pfd[0].Revents = 0
		n, err := unix.Poll(pfd, 100)
		if err != nil && err != unix.EINTR {
			return fmt.Errorf("capture: poll: %w", err)
		}
		if n > 0 && pfd[0].Revents&unix.POLLIN != 0 {
			w.Drain()
		}
	}
}

// Drain reads and handles every currently-queued event, returning how many
// close-writes it captured. Exposed for deterministic tests (drive file ops, then
// Drain, then inspect Versions).
func (w *Watcher) Drain() int {
	captured := 0
	for {
		rn, err := unix.Read(w.fan, w.buf)
		if err != nil {
			if err == unix.EAGAIN {
				return captured
			}
			w.missed++
			return captured
		}
		if rn <= 0 {
			return captured
		}
		for off := 0; off+metaSize <= rn; {
			evLen := int(binary.LittleEndian.Uint32(w.buf[off : off+4]))
			if evLen < metaSize || off+evLen > rn {
				break
			}
			mask := binary.LittleEndian.Uint64(w.buf[off+8 : off+16])
			evFD := int32(binary.LittleEndian.Uint32(w.buf[off+16 : off+20]))
			evPID := int32(binary.LittleEndian.Uint32(w.buf[off+20 : off+24])) // writer pid (listener ns)
			if mask&unix.FAN_Q_OVERFLOW != 0 {
				// The kernel dropped events: unbounded, detected loss. The
				// overflow marker carries no fd. Checkpoints in this window
				// must read as Incomplete, never Fully.
				w.overflowed = true
				off += evLen
				continue
			}
			if mask&unix.FAN_CLOSE_WRITE != 0 && evFD >= 0 {
				if w.handle(int(evFD), evPID) {
					captured++
				}
			} else if evFD >= 0 {
				unix.Close(int(evFD))
			}
			off += evLen
		}
	}
}

// handle captures one close-write's content via the retained fd (which survives
// close+unlink) into the object store + version log, classifying the writer by
// process lineage. Returns true on capture. evPID is the writing process, which
// fanotify reports in the LISTENER's pid namespace, so it can be looked up
// directly in the /proc this process sees.
func (w *Watcher) handle(evFD int, evPID int32) bool {
	defer unix.Close(evFD)
	link, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", evFD))
	if err != nil {
		w.missed++
		return false
	}
	path := strings.TrimSuffix(link, " (deleted)")
	if !w.protected(path) {
		w.noteOutside(path, evPID)
		return false // outside every protected root: uncaptured, surfaced above
	}
	if w.excluded(path) {
		return false // bulk-excluded (default-skip list)
	}
	// Credential material is never read or stored (security default). Checked
	// BEFORE the content read: the point is that the bytes never enter the
	// object store, not that they are stored and later filtered.
	if store.IsSecretPath(path) {
		return false
	}
	var st unix.Stat_t
	if err := unix.Fstat(evFD, &st); err != nil {
		w.miss(path)
		return false
	}
	if _, err := unix.Seek(evFD, 0, 0); err != nil {
		w.miss(path)
		return false
	}
	content, err := readAll(evFD, st.Size)
	if err != nil {
		w.miss(path)
		return false
	}
	ref, _, err := w.oc.Put(content) // persist blob first
	if err != nil {
		w.miss(path)
		return false
	}
	// Classify the writer at capture time, while /proc is warm (the birth-parent
	// cache is the only source for an already-reaped one-shot writer).
	writer, wpid, wstart := w.classify(evPID)
	// Op is a coarse "a version exists" label: capture-on-close cannot cheaply
	// tell create from modify, and salvage only needs the latest ref per path.
	v := versionlog.Version{
		Op: versionlog.OpModify, Path: path, Ref: ref, Mode: uint32(st.Mode & 0o7777),
		Writer: writer.String(), WriterPid: wpid, WriterStart: wstart, TimeNS: time.Now().UnixNano(),
	}
	if err := w.log.Append(v); err != nil {
		w.miss(path)
		return false
	}
	return true
}

// readFD is a seam so tests can drive a read failure through the real capture
// path (a mid-file read error cannot be provoked from userspace on an ordinary
// file).
var readFD = unix.Read

// readAll reads the whole file behind fd. Short reads are normal and are looped
// over; EINTR is retried. Any other read error is FATAL to this capture: the
// bytes read so far are a truncated prefix, and storing them would put a
// silently truncated version in the store and report it as fully recoverable.
// The caller turns the error into a named miss, so the loss is surfaced.
func readAll(fd int, size int64) ([]byte, error) {
	content := make([]byte, 0, size)
	tmp := make([]byte, 64*1024)
	for {
		n, err := readFD(fd, tmp)
		if err != nil {
			if err == unix.EINTR {
				continue // no bytes consumed: a signal, not a loss
			}
			return nil, err
		}
		if n == 0 {
			return content, nil // EOF: the whole file
		}
		content = append(content, tmp[:n]...)
	}
}

// RecordDelete journals delete provenance for path: who removed it, classified
// by lineage at event time exactly like a close-write. Fed by the dirent
// change-feed (the fd-class group cannot see deletions). An OpDelete version
// makes an agent-deleted file a restorable undo candidate and clears the path
// from salvage.
func (w *Watcher) RecordDelete(path string, evPID int32) error {
	if !w.protected(path) || w.excluded(path) {
		return nil
	}
	writer, wpid, wstart := w.classify(evPID)
	v := versionlog.Version{
		Op: versionlog.OpDelete, Path: path,
		Writer: writer.String(), WriterPid: wpid, WriterStart: wstart, TimeNS: time.Now().UnixNano(),
	}
	if err := w.log.Append(v); err != nil {
		w.miss(path)
		return err
	}
	return nil
}

// Fd exposes the fanotify fd so the daemon can poll capture and the change-feed
// together.
func (w *Watcher) Fd() int { return w.fan }

// noteOutside records an agent-tree write that landed outside every protected
// root. Cheap pre-checks first: no agent session ⇒ skip (no lineage walks for
// ambient system noise); own-store writes are ours, never warned about.
func (w *Watcher) noteOutside(path string, evPID int32) {
	w.mu.Lock()
	haveAgent := len(w.agentRoots) > 0
	w.mu.Unlock()
	if !haveAgent || w.storeDir != "" && (path == w.storeDir || strings.HasPrefix(path, w.storeDir+"/")) {
		return
	}
	if writer, _, _ := w.classify(evPID); writer != provenance.Agent {
		return
	}
	// Under w.mu: status connections read this state from their own goroutines
	// while the event loop appends, so neither side may touch it unlocked.
	w.mu.Lock()
	defer w.mu.Unlock()
	w.outsideCount++
	for _, p := range w.outside {
		if p == path {
			return // count every write, list each path once
		}
	}
	if len(w.outside) < maxOutsideListed {
		w.outside = append(w.outside, path)
	}
}

// OutsideWrites returns the agent-tree writes seen outside every protected
// root since daemon start: up to maxOutsideListed distinct paths, plus the
// total write count (which keeps counting past the list bound).
func (w *Watcher) OutsideWrites() ([]string, int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.outside...), w.outsideCount
}

// miss records a bounded, NAMED capture loss (we know which path we failed on).
func (w *Watcher) miss(path string) {
	w.missed++
	w.missedPaths = append(w.missedPaths, path)
}

// classify resolves the writer's lineage and attributes the write. Returns the
// verdict plus the writer's stable identity (pid + start-time) for the ledger.
func (w *Watcher) classify(evPID int32) (provenance.Class, int, uint64) {
	if evPID <= 0 {
		return provenance.Unknown, 0, 0
	}
	pid := int(evPID)
	wstart, _ := w.tracker.StartTime(pid)
	// Checkpoint's own restore/undo writes are the tool's, not the agent's and
	// not the human's. Checked BEFORE lineage: the undo process is a child of
	// the user's shell, so a lineage walk would (correctly, but uselessly)
	// call it human and then conflict the very file it just reverted.
	if provenance.IsSelfWrite(pid) {
		return provenance.Self, pid, wstart
	}
	roots := w.agentRootSnapshot()
	chain, resolved := w.tracker.Lineage(pid, roots)
	return provenance.Classify(chain, resolved, roots), pid, wstart
}

// rootFor returns the protected root path sits under, and whether it sits under
// one at all. The LONGEST match wins, so an extra protected folder nested inside
// the workspace scopes exclusions to itself: --protect on ~/proj/build means the
// user wants that tree, and the root they named is not "bulk" to them.
func (w *Watcher) rootFor(path string) (string, bool) {
	best := ""
	for _, r := range w.roots {
		if strings.HasPrefix(path, r+"/") && len(r) > len(best) {
			best = r
		}
	}
	return best, best != ""
}

// protected reports whether path sits under any protected root.
func (w *Watcher) protected(path string) bool {
	_, ok := w.rootFor(path)
	return ok
}

// excluded reports whether path lives under a bulk-excluded directory. The
// decision is made on the path RELATIVE to its protected root: comparing
// against the absolute path would exclude every file in a workspace whose own
// parent directory is named build/dist/target/.git, leaving the user entirely
// uncaptured while the tool reported the workspace protected. Only the
// directory components are considered; a FILE named build is real work.
func (w *Watcher) excluded(path string) bool {
	root, ok := w.rootFor(path)
	if !ok {
		return false
	}
	segs := strings.Split(path[len(root)+1:], "/")
	for _, seg := range segs[:len(segs)-1] {
		if bulkExcludes[seg] {
			return true
		}
	}
	return false
}

// Poll blocks up to timeoutMs for close-write events to become available,
// returning true if the fanotify fd is readable. The daemon uses it to pace its
// drain loop without busy-spinning (Drain itself is non-blocking).
func (w *Watcher) Poll(timeoutMs int) bool {
	pfd := []unix.PollFd{{Fd: int32(w.fan), Events: unix.POLLIN}}
	n, err := unix.Poll(pfd, timeoutMs)
	if err != nil && err != unix.EINTR {
		return false
	}
	return n > 0 && pfd[0].Revents&unix.POLLIN != 0
}

// Objects returns the underlying object store, so a caller (the daemon) can build
// a boundary-scan manifest against the same content the capture loop is writing.
func (w *Watcher) Objects() *objstore.Store { return w.oc }

// Versions returns the captured versions so far (oldest first).
func (w *Watcher) Versions() []versionlog.Version { return w.log.Versions() }

// Missed reports how many individual events could not be captured this window
// (unreadable link/stat, store error). This is a detected, BOUNDED loss signal
// (named, counted). A checkpoint with missed>0 is Recoverable-with-exceptions.
func (w *Watcher) Missed() int { return w.missed }

// MissedPaths returns the absolute paths of this window's missed writes where
// the path was at least resolvable (Missed may exceed len(MissedPaths): a miss
// before readlink has no name). The daemon stamps these on the checkpoint as
// named exceptions.
func (w *Watcher) MissedPaths() []string { return append([]string(nil), w.missedPaths...) }

// Overflowed reports whether the fanotify queue overflowed this window, an
// UNBOUNDED, detected loss (we know something was dropped but not what). A
// checkpoint whose window overflowed is Incomplete (PARTIAL), never Fully.
func (w *Watcher) Overflowed() bool { return w.overflowed }

// ResetWindow clears the per-window loss counters. The daemon calls it after
// stamping each checkpoint, so each checkpoint's status reflects only the loss
// detected since the previous one.
func (w *Watcher) ResetWindow() {
	w.missed = 0
	w.missedPaths = nil
	w.overflowed = false
}

// Sync flushes the version log (call before relying on durability of a batch).
func (w *Watcher) Sync() error { return w.log.Sync() }

// Close stops the birth-parent scanner, releases the fanotify fd and the
// version-log lock.
func (w *Watcher) Close() error {
	close(w.scanStop)
	unix.Close(w.fan)
	return w.log.Close()
}

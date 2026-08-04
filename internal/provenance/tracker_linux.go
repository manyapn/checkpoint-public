//go:build linux

package provenance

import (
	"encoding/binary"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

// procInfo is what we cache per pid at first sighting: its start-time, its
// birth-parent, and the last moment the scanner confirmed that this process
// still held the pid.
type procInfo struct {
	start uint64
	ppid  int
	seen  time.Time
}

// staleAfter bounds how long a cache entry may speak for a pid whose process is
// GONE. A dead pid's ancestry is trustworthy only while the number cannot yet
// have been handed to somebody else, and reuse requires the allocator to wrap
// all the way around pid_max (tens of thousands of forks at minimum). Confirmed
// within this window, no wrap is physically possible, so the corpse is provably
// still the writer; past it, the entry is only a guess and a guess must never
// read as Agent. The window is orders of magnitude larger than the gap between a
// one-shot writer's exit and the classification of its write, so the design
// point (a reaped `rm` still resolves) is untouched.
//
// It doubles as the cache's eviction age: an entry the scanner no longer sees in
// /proc is unusable once it is this old, so an always-on daemon does not
// accumulate one entry per process ever run.
const staleAfter = 2 * time.Second

// Tracker caches every process's birth-parent + start-time and resolves lineage.
// A one-shot writer (rm, a shell subprocess) can be dead and reaped before its
// close-write event is handled, so its parent must be captured at birth while the
// process still exists in /proc. The recorded birth-parent is
// also the attribution-correct parent for a process later orphaned/reparented.
type Tracker struct {
	mu     sync.Mutex
	cache  map[int]procInfo
	active atomic.Bool   // true while an agent session is live (spin); false idles
	wake   chan struct{} // kicks the scanner out of its idle sleep on activation
}

func NewTracker() *Tracker {
	return &Tracker{cache: map[int]procInfo{}, wake: make(chan struct{}, 1)}
}

// SetActive controls the scan cadence. While active (an agent root is
// registered), the scanner spins to catch one-shot writers born and reaped
// between events, the only window where the birth cache is load-bearing. While
// inactive it paces itself, so the always-on daemon does not burn a core when no
// session is running (long-lived pre-session processes still resolve via fresh
// /proc reads at classification time).
//
// Activation WAKES the scanner immediately: a session's first one-shot child can
// be born within a millisecond of registration, and a scanner still finishing
// its up-to-20ms idle sleep would miss it entirely: without the wake, the rm in
// `run -- bash -c 'rm ...'` classifies as unknown.
func (t *Tracker) SetActive(active bool) {
	t.active.Store(active)
	if active {
		select {
		case t.wake <- struct{}{}:
		default:
		}
	}
}

// Run continuously scans /proc, caching each pid's start-time + parent at first
// sighting, until stop is closed. Call as a goroutine. The getdents loop is
// deliberately allocation-light: it runs hot while a session is active.
//
// A pid is tracked only while it stays present: when it vanishes it is dropped
// from the scanner's set, so the NEXT sighting of that number re-reads /proc
// instead of leaving the old occupant's ancestry standing forever. Suppressing
// every future sighting of a numeric pid (the obvious optimization) is what
// would let a recycled pid inherit a dead agent child's lineage.
func (t *Tracker) Run(stop <-chan struct{}) {
	f, err := os.Open("/proc")
	if err != nil {
		return
	}
	defer f.Close()
	buf := make([]byte, 1<<16)
	tracked := make(map[int]uint64) // scanner-local: pid -> pass last seen present
	var pass uint64
	prev, lastEvict := time.Now(), time.Now()
	for {
		select {
		case <-stop:
			return
		default:
		}
		if _, err := unix.Seek(int(f.Fd()), 0, 0); err != nil {
			return
		}
		now := time.Now()
		// Presence across consecutive passes is only proof of continuity because
		// the gap is short: a pid cannot be freed and reissued without a wrap.
		// A scanner that lost the CPU for longer than an entry may be trusted
		// cannot make that claim, so it re-verifies every pid from /proc.
		if now.Sub(prev) > staleAfter {
			clear(tracked)
		}
		prev = now
		pass++
		for {
			n, err := unix.Getdents(int(f.Fd()), buf)
			if err != nil || n <= 0 {
				break
			}
			for off := 0; off < n; {
				reclen := int(binary.LittleEndian.Uint16(buf[off+16 : off+18]))
				if reclen <= 0 || off+reclen > n {
					break
				}
				pid := 0
				for i := off + 19; i < off+reclen && buf[i] != 0; i++ { // linux_dirent64 name at +19
					c := buf[i]
					if c < '0' || c > '9' {
						pid = 0
						break
					}
					pid = pid*10 + int(c-'0')
				}
				off += reclen
				if pid == 0 {
					continue
				}
				if tracked[pid] == 0 { // unseen, or seen last as a different process
					t.observe(pid, now)
				}
				tracked[pid] = pass
			}
		}
		evict := now.Sub(lastEvict) > staleAfter
		if evict {
			lastEvict = now
		}
		t.sweep(tracked, pass, now, evict)
		if t.active.Load() {
			runtime.Gosched() // active session: spin to catch one-shot writers
		} else {
			select { // idle: seed the cache cheaply, but wake instantly on activation
			case <-stop:
				return
			case <-t.wake:
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
}

// observe records pid's start-time and birth-parent on a sighting. When the
// cache already holds this exact process (same start-time) the recorded parent
// is KEPT: /proc reports the current parent, which changes when a process is
// orphaned and reparented, and the birth-parent is the attribution-correct one.
// A different start-time means the number was recycled, so the dead
// predecessor's entry is replaced outright rather than inherited.
func (t *Tracker) observe(pid int, now time.Time) {
	start, ppid, ok := readStat(pid)
	if !ok {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if pi, cached := t.cache[pid]; cached && pi.start == start {
		pi.seen = now
		t.cache[pid] = pi
		return
	}
	t.cache[pid] = procInfo{start: start, ppid: ppid, seen: now}
}

// sweep closes a scan pass: it refreshes the confirmation time of every pid
// still present, and forgets the ones that vanished so their number is re-read
// (not inherited) at its next sighting. When evict is set it also drops cache
// entries no longer young enough to speak for a dead pid, which is what keeps
// the map bounded in an always-on daemon.
func (t *Tracker) sweep(tracked map[int]uint64, pass uint64, now time.Time, evict bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for pid, p := range tracked {
		if p != pass {
			delete(tracked, pid)
			continue
		}
		if pi, ok := t.cache[pid]; ok {
			pi.seen = now
			t.cache[pid] = pi
		}
	}
	if !evict {
		return
	}
	for pid, pi := range t.cache {
		if now.Sub(pi.seen) > staleAfter {
			delete(t.cache, pid)
		}
	}
}

// StartTime returns pid's start-time (for building an Identity). A live process
// is its own authority, so it is read from /proc first; the birth cache answers
// only for a process that is already gone, and only while that entry is still
// young enough to be about this pid's writer rather than a possible successor
// (see staleAfter). An unresolvable start-time is reported as such: a phantom
// identity in the ledger is worse than an honest gap.
func (t *Tracker) StartTime(pid int) (uint64, bool) {
	if start, _, ok := readStat(pid); ok {
		return start, true
	}
	t.mu.Lock()
	pi, cached := t.cache[pid]
	t.mu.Unlock()
	if cached && time.Since(pi.seen) <= staleAfter {
		return pi.start, true
	}
	return 0, false
}

// Lineage walks from pid up through birth-parents, returning the chain of stable
// identities (writer-first) and whether it reached a clean terminal: an agent
// root, init (pid 1), or a session root (a pid whose parent is 0, a container
// session leader that never chains to the namespace's init). On any mid-chain
// resolution failure it returns (nil, false): never a partial chain dressed as
// complete (the recording contract). The birth cache is consulted first, since
// it is the only source for already-reaped one-shot writers.
func (t *Tracker) Lineage(pid int, agentRoots map[Identity]bool) ([]Identity, bool) {
	if pid <= 0 {
		return nil, false
	}
	chain := make([]Identity, 0, 8)
	p := pid
	for steps := 0; steps < 128; steps++ {
		start, ppid, ok := t.info(p)
		if !ok {
			return nil, false
		}
		id := Identity{Pid: p, Start: start}
		chain = append(chain, id)
		if agentRoots[id] { // agent terminal
			return chain, true
		}
		if p == 1 { // init terminal
			return chain, true
		}
		if ppid == 0 { // session-root terminal (provably outside any agent tree)
			return chain, true
		}
		p = ppid
	}
	return nil, false
}

// info returns pid's (start, ppid), preferring the birth cache. A cached entry
// only speaks for the writer if the pid still belongs to the process it
// describes, which is checked two ways:
//
//   - A LIVE process whose start-time contradicts the cache means the pid was
//     recycled and the cache describes a dead predecessor. Trusting it could walk
//     a stranger's write into the agent's ancestry, so the resolution fails
//     instead (-> Unknown, never Agent) and the entry is repaired for future
//     events.
//   - A DEAD pid has no live process to contradict anything, and reaped one-shot
//     writers are the cache's whole purpose, so the entry still answers, but only
//     while the scanner confirmed the pid that recently (staleAfter). Beyond that
//     the number could have been reissued to a short-lived stranger who exited
//     before its write was classified, leaving the corpse's agent ancestry as the
//     only record: precisely the mistake that reverts a human's edit. Ancestry
//     that old is a guess, and a guess resolves to nothing.
//
// The live ppid is NOT compared: reparenting after orphaning is expected, and the
// cached birth-parent is the attribution-correct one.
func (t *Tracker) info(pid int) (start uint64, ppid int, ok bool) {
	t.mu.Lock()
	pi, cached := t.cache[pid]
	t.mu.Unlock()
	if !cached {
		start, ppid, ok = readStat(pid)
		if ok {
			t.mu.Lock()
			t.cache[pid] = procInfo{start: start, ppid: ppid, seen: time.Now()}
			t.mu.Unlock()
		}
		return start, ppid, ok
	}
	liveStart, livePpid, liveOk := readStat(pid)
	switch {
	case liveOk && liveStart == pi.start:
		return pi.start, pi.ppid, true // same process: the birth-parent stands
	case liveOk:
		t.mu.Lock()
		t.cache[pid] = procInfo{start: liveStart, ppid: livePpid, seen: time.Now()}
		t.mu.Unlock()
		return 0, 0, false // recycled pid: ambiguous writer, fail conservative
	case time.Since(pi.seen) <= staleAfter:
		return pi.start, pi.ppid, true // dead, but this pid was its own moments ago
	default:
		return 0, 0, false // dead and stale: the pid may since have been reissued
	}
}

// StartTime resolves a pid's start-time without a Tracker (used by `run` to build
// its child's identity). Prefers nothing; always a fresh read.
func StartTime(pid int) (uint64, bool) {
	start, _, ok := readStat(pid)
	return start, ok
}

// readStat parses /proc/<pid>/stat for the start-time (field 22) and parent pid
// (field 4). Both are parsed AFTER the last ')' so a comm containing spaces or
// parens cannot shift the field offsets. ok=false means the entry was unreadable
// (process gone). ppid==0 with ok=true means pid is a session root.
func readStat(pid int) (start uint64, ppid int, ok bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, 0, false
	}
	return parseStat(b)
}

// parseStat extracts (starttime, ppid) from raw stat content. Split from
// readStat so hostile content (truncated, garbage, paren-laden comms) is
// testable directly; ok=false on anything malformed.
func parseStat(b []byte) (start uint64, ppid int, ok bool) {
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, 0, false
	}
	fields := strings.Fields(s[i+1:]) // fields[0]=state (whole-stat field 3)
	if len(fields) < 20 {
		return 0, 0, false
	}
	pp, err := strconv.Atoi(fields[1]) // field 4: ppid
	if err != nil || pp < 0 {
		return 0, 0, false
	}
	st, err := strconv.ParseUint(fields[19], 10, 64) // field 22: starttime
	if err != nil {
		return 0, 0, false
	}
	return st, pp, true
}

// selfExeID identifies the checkpoint binary by device+inode (not by path: a
// path can be relative, symlinked, or renamed under us, and the store may be
// read by a copy of the binary at a different location).
type exeID struct {
	dev uint64
	ino uint64
}

var (
	selfExeOnce sync.Once
	selfExe     exeID
	selfExeOK   bool
)

func ownExeID() (exeID, bool) {
	selfExeOnce.Do(func() {
		var st unix.Stat_t
		if err := unix.Stat("/proc/self/exe", &st); err == nil {
			selfExe, selfExeOK = exeID{dev: uint64(st.Dev), ino: st.Ino}, true
		}
	})
	return selfExe, selfExeOK
}

// IsSelfWrite reports whether pid is running the SAME executable as this
// process, i.e. the write came from checkpoint itself (a restore or an undo
// materializing files), not from the agent or the human. Compared by
// device+inode through /proc/<pid>/exe, so a renamed or symlinked invocation
// still matches and an unrelated binary never does. A dead or unreadable pid
// is not self: uncertainty must never claim a write as checkpoint's own.
func IsSelfWrite(pid int) bool {
	me, ok := ownExeID()
	if !ok || pid <= 0 {
		return false
	}
	var st unix.Stat_t
	if err := unix.Stat("/proc/"+strconv.Itoa(pid)+"/exe", &st); err != nil {
		return false
	}
	return uint64(st.Dev) == me.dev && st.Ino == me.ino
}

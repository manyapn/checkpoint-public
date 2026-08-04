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

// procInfo is what we cache per pid at first sighting: its start-time and parent.
type procInfo struct {
	start uint64
	ppid  int
}

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
func (t *Tracker) Run(stop <-chan struct{}) {
	f, err := os.Open("/proc")
	if err != nil {
		return
	}
	defer f.Close()
	buf := make([]byte, 1<<16)
	seen := make(map[int]bool) // scanner-local: single writer, no lock
	for {
		select {
		case <-stop:
			return
		default:
		}
		if _, err := unix.Seek(int(f.Fd()), 0, 0); err != nil {
			return
		}
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
				if pid == 0 || seen[pid] {
					continue
				}
				seen[pid] = true
				if start, ppid, ok := readStat(pid); ok {
					t.mu.Lock()
					t.cache[pid] = procInfo{start: start, ppid: ppid}
					t.mu.Unlock()
				}
			}
		}
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

// StartTime returns pid's start-time (for building an Identity), preferring the
// birth cache, falling back to a fresh /proc read.
func (t *Tracker) StartTime(pid int) (uint64, bool) {
	t.mu.Lock()
	pi, ok := t.cache[pid]
	t.mu.Unlock()
	if ok {
		return pi.start, true
	}
	start, _, ok := readStat(pid)
	return start, ok
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
// is verified against the live process when one exists: the cache is
// authoritative for the DEAD (reaped one-shot writers are its whole purpose), but
// a live process whose start-time contradicts the cache means the pid was
// recycled and the cache describes a dead predecessor. Trusting it could walk a
// stranger's write into the agent's ancestry, so the resolution fails instead
// (-> Unknown, never Agent) and the entry is repaired for future events. The
// live ppid is NOT compared: reparenting after orphaning is expected, and the
// cached birth-parent is the attribution-correct one.
func (t *Tracker) info(pid int) (start uint64, ppid int, ok bool) {
	t.mu.Lock()
	pi, cached := t.cache[pid]
	t.mu.Unlock()
	if cached {
		if liveStart, livePpid, liveOk := readStat(pid); liveOk && liveStart != pi.start {
			t.mu.Lock()
			t.cache[pid] = procInfo{start: liveStart, ppid: livePpid}
			t.mu.Unlock()
			return 0, 0, false // recycled pid: ambiguous writer, fail conservative
		}
		return pi.start, pi.ppid, true
	}
	start, ppid, ok = readStat(pid)
	if ok {
		t.mu.Lock()
		t.cache[pid] = procInfo{start: start, ppid: ppid}
		t.mu.Unlock()
	}
	return start, ppid, ok
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

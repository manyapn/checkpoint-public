package lineage

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

// A one-shot writer such as rm can exit before its event is handled, and a
// dead process has no /proc entry to ask for its parent. The tracker keeps a
// recent copy of /proc's parent table, refreshed continuously while an agent
// session is active.
type Tracker struct {
	mu       sync.Mutex
	procs    map[int]proc
	active   atomic.Bool
	wake     chan struct{}
	ownExe   exeID
	ownExeOK bool
}

type proc struct {
	start uint64
	ppid  int
	seen  time.Time
}

// A dead pid's cached parent is trusted only this long; pid reuse needs the
// allocator to wrap, which cannot happen in two seconds.
const trust = 2 * time.Second

func NewTracker() *Tracker {
	t := &Tracker{procs: map[int]proc{}, wake: make(chan struct{}, 1)}
	var st unix.Stat_t
	if err := unix.Stat("/proc/self/exe", &st); err == nil {
		t.ownExe, t.ownExeOK = exeID{st.Dev, st.Ino}, true
	}
	return t
}

type exeID struct{ dev, ino uint64 }

// SetActive makes the scanner spin while an agent session runs and idle
// otherwise. Activation wakes it at once: the session's first child can be
// born within a millisecond.
func (t *Tracker) SetActive(active bool) {
	t.active.Store(active)
	if active {
		select {
		case t.wake <- struct{}{}:
		default:
		}
	}
}

func (t *Tracker) Run(stop <-chan struct{}) {
	dir, err := os.Open("/proc")
	if err != nil {
		return
	}
	defer dir.Close()
	buf := make([]byte, 64*1024)
	present := map[int]bool{}
	lastPass := time.Now()
	for {
		select {
		case <-stop:
			return
		default:
		}
		now := time.Now()
		if now.Sub(lastPass) > trust {
			clear(present) // scanner stalled: re-read everything
		}
		lastPass = now
		nowPresent := make(map[int]bool, len(present))
		for _, pid := range listPids(dir, buf) {
			nowPresent[pid] = true
			if !present[pid] {
				t.observe(pid, now)
			}
		}
		t.sweep(nowPresent, now)
		present = nowPresent
		if t.active.Load() {
			runtime.Gosched()
			continue
		}
		select {
		case <-stop:
			return
		case <-t.wake:
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// listPids reads /proc with getdents into a reused buffer: this loop runs
// continuously while an agent works, and a short-lived child is only caught
// if a pass overlaps its lifetime, so each pass is kept as cheap as possible.
func listPids(dir *os.File, buf []byte) []int {
	var pids []int
	unix.Seek(int(dir.Fd()), 0, 0)
	for {
		n, err := unix.Getdents(int(dir.Fd()), buf)
		if err != nil || n <= 0 {
			return pids
		}
		for off := 0; off < n; {
			reclen := int(binary.LittleEndian.Uint16(buf[off+16:]))
			if reclen <= 0 || off+reclen > n {
				return pids
			}
			if pid := pidFromName(buf[off+19 : off+reclen]); pid > 0 {
				pids = append(pids, pid)
			}
			off += reclen
		}
	}
}

// pidFromName parses an all-digit NUL-terminated dirent name; 0 otherwise.
func pidFromName(name []byte) int {
	pid := 0
	for _, c := range name {
		if c == 0 {
			break
		}
		if c < '0' || c > '9' {
			return 0
		}
		pid = pid*10 + int(c-'0')
	}
	return pid
}

// observe records a pid's birth parent. A known pid with the same start
// keeps its recorded parent (reparenting after orphaning is not a new birth).
func (t *Tracker) observe(pid int, now time.Time) {
	start, ppid, ok := readStat(pid)
	if !ok {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if p, known := t.procs[pid]; known && p.start == start {
		p.seen = now
		t.procs[pid] = p
		return
	}
	t.procs[pid] = proc{start, ppid, now}
}

func (t *Tracker) sweep(present map[int]bool, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for pid, p := range t.procs {
		if present[pid] {
			p.seen = now
			t.procs[pid] = p
		} else if now.Sub(p.seen) > trust {
			delete(t.procs, pid)
		}
	}
}

// info answers (start, ppid) for pid: from /proc if alive, from the cache if
// it died within the trust window. A live process whose start time differs
// from the cache is a recycled pid and resolves to nothing.
func (t *Tracker) info(pid int) (uint64, int, bool) {
	t.mu.Lock()
	cached, known := t.procs[pid]
	t.mu.Unlock()
	start, ppid, alive := readStat(pid)
	switch {
	case !known && alive:
		t.mu.Lock()
		t.procs[pid] = proc{start, ppid, time.Now()}
		t.mu.Unlock()
		return start, ppid, true
	case known && alive && start == cached.start:
		return cached.start, cached.ppid, true
	case known && !alive && time.Since(cached.seen) <= trust:
		return cached.start, cached.ppid, true
	}
	return 0, 0, false
}

// Lineage walks from pid up through parents until it reaches an agent root,
// init, or a session root. Any gap in the walk returns resolved=false.
func (t *Tracker) Lineage(pid int, roots map[Identity]bool) ([]Identity, bool) {
	var chain []Identity
	for steps := 0; pid > 0 && steps < 128; steps++ {
		start, ppid, ok := t.info(pid)
		if !ok {
			return nil, false
		}
		id := Identity{pid, start}
		chain = append(chain, id)
		if roots[id] || pid == 1 || ppid == 0 {
			return chain, true
		}
		pid = ppid
	}
	return nil, false
}

// Who classifies the process behind a write. Checkpoint's own restore and
// undo writes are Self so they never count as either author.
func (t *Tracker) Who(pid int, roots map[Identity]bool) string {
	if pid <= 0 {
		return Unknown
	}
	if t.isSelf(pid) {
		return Self
	}
	chain, resolved := t.Lineage(pid, roots)
	return Classify(chain, resolved, roots)
}

func (t *Tracker) isSelf(pid int) bool {
	var st unix.Stat_t
	if !t.ownExeOK || unix.Stat("/proc/"+strconv.Itoa(pid)+"/exe", &st) != nil {
		return false
	}
	return exeID{st.Dev, st.Ino} == t.ownExe
}

// StartTime reads a live process's start time, for building its Identity.
func StartTime(pid int) (uint64, bool) {
	start, _, ok := readStat(pid)
	return start, ok
}

func readStat(pid int) (start uint64, ppid int, ok bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, 0, false
	}
	return parseStat(string(b))
}

// Fields are taken after the last ')' so a command name with spaces or
// parentheses cannot shift them.
func parseStat(s string) (start uint64, ppid int, ok bool) {
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, 0, false
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 20 {
		return 0, 0, false
	}
	ppid, err := strconv.Atoi(f[1])
	if err != nil || ppid < 0 {
		return 0, 0, false
	}
	start, err = strconv.ParseUint(f[19], 10, 64)
	return start, ppid, err == nil
}

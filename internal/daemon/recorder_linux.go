package daemon

import (
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/manyapn/checkpoint-public/internal/lineage"
	"github.com/manyapn/checkpoint-public/internal/objects"
	"github.com/manyapn/checkpoint-public/internal/snapshot"
	"github.com/manyapn/checkpoint-public/internal/writelog"
)

// recorder turns kernel events into write-log entries with an author.
type recorder struct {
	root, storeDir string
	objs           *objects.Store
	log            *writelog.Log
	tracker        *lineage.Tracker

	mu     sync.Mutex
	agents map[lineage.Identity]bool

	missed       []string // this window's writes we could not capture
	outside      []string // agent writes outside the protected folder (bounded list)
	outsideCount int
}

func newRecorder(root, storeDir string) (*recorder, error) {
	objs, err := objects.Open(storeDir)
	if err != nil {
		return nil, err
	}
	log, err := writelog.Open(filepath.Join(storeDir, snapshot.LogFile))
	if err != nil {
		return nil, err
	}
	return &recorder{root: root, storeDir: storeDir, objs: objs, log: log,
		tracker: lineage.NewTracker(), agents: map[lineage.Identity]bool{}}, nil
}

func (r *recorder) setAgent(id lineage.Identity, on bool) {
	r.mu.Lock()
	if on {
		r.agents[id] = true
	} else {
		delete(r.agents, id)
	}
	n := len(r.agents)
	r.mu.Unlock()
	r.tracker.SetActive(n > 0)
}

func (r *recorder) agentRoots() map[lineage.Identity]bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	copy := make(map[lineage.Identity]bool, len(r.agents))
	for id := range r.agents {
		copy[id] = true
	}
	return copy
}

func (r *recorder) who(pid int) string { return r.tracker.Who(pid, r.agentRoots()) }

func (r *recorder) under(path string) bool { return strings.HasPrefix(path, r.root+"/") }

func (r *recorder) skip(path string) bool {
	rel := strings.TrimPrefix(path, r.root+"/")
	for _, seg := range strings.Split(filepath.Dir(rel), "/") {
		if snapshot.ExcludedDir(seg) {
			return true
		}
	}
	return snapshot.IsSecret(path)
}

// write saves the closed file's content through the kernel-held descriptor,
// which still works if the file has since been deleted. Reports whether the
// write was one of ours to record.
func (r *recorder) write(path string, fd int, pid int) bool {
	if !r.under(path) {
		r.noteOutside(path, pid)
		return false
	}
	if r.skip(path) {
		return false
	}
	writer := r.who(pid) // first: the writer may be about to exit
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil {
		r.missed = append(r.missed, path)
		return true
	}
	content, err := readAll(fd, st.Size)
	if err != nil {
		r.missed = append(r.missed, path)
		return true
	}
	ref, err := r.objs.Put(content)
	if err != nil {
		r.missed = append(r.missed, path)
		return true
	}
	r.log.Append(writelog.Entry{Op: writelog.Write, Path: path, Ref: ref,
		Mode: uint32(st.Mode & 0o7777), Writer: writer, TimeNS: time.Now().UnixNano()})
	return true
}

func (r *recorder) delete(path string, pid int) {
	if !r.under(path) || r.skip(path) {
		return
	}
	r.log.Append(writelog.Entry{Op: writelog.Delete, Path: path, Writer: r.who(pid), TimeNS: time.Now().UnixNano()})
}

// A short read is normal; any other error means a truncated file would be
// stored as complete, so the write is reported as missed instead.
func readAll(fd int, size int64) ([]byte, error) {
	content := make([]byte, 0, size)
	buf := make([]byte, 64*1024)
	if _, err := unix.Seek(fd, 0, 0); err != nil {
		return nil, err
	}
	for {
		n, err := unix.Read(fd, buf)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return nil, err
		}
		if n == 0 {
			return content, nil
		}
		content = append(content, buf[:n]...)
	}
}

func (r *recorder) noteOutside(path string, pid int) {
	if len(r.agentRoots()) == 0 || strings.HasPrefix(path, r.storeDir+"/") || r.who(pid) != lineage.Agent {
		return
	}
	r.outsideCount++
	for _, p := range r.outside {
		if p == path {
			return
		}
	}
	if len(r.outside) < 20 {
		r.outside = append(r.outside, path)
	}
}

func (r *recorder) close() {
	r.log.Sync()
	r.log.Close()
}

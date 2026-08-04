// Package versionlog is checkpoint's salvage log: an append-only, crash-safe
// record of every captured completed file version (a close-write's post-image
// content ref). It is what makes files recoverable that no checkpoint holds: a
// file created→written→deleted between two boundaries never reaches a manifest,
// but its close-write was captured here, so its bytes stay recoverable from the
// object store.
//
// Crash safety: newline-delimited JSON records, single-writer advisory lock, and
// torn-tail recovery on open. A process killed mid-append truncates to the last
// complete record; a valid record is never lost. Scope is process-crash
// (SIGKILL/exit) consistency, not host power-loss (no per-append fsync; Sync
// exists for boundaries).
package versionlog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// Op is the kind of completed write a version records.
type Op string

const (
	OpCreate Op = "create"
	OpModify Op = "modify"
	OpDelete Op = "delete"
)

// Version is one captured completed file version. Ref is the object-store ref of
// the post-image content for create/modify; empty for delete.
type Version struct {
	Op   Op     `json:"op"`
	Path string `json:"path"`
	Ref  string `json:"ref,omitempty"`
	Mode uint32 `json:"mode,omitempty"`

	// Provenance: the write's verdict plus the writer's stable identity,
	// classified at capture time. Writer is "agent" | "human" | "unknown";
	// empty on older records (read back as unknown). This is the durable
	// ledger selective undo consumes.
	Writer      string `json:"writer,omitempty"`
	WriterPid   int    `json:"writer_pid,omitempty"`
	WriterStart uint64 `json:"writer_start,omitempty"`

	// TimeNS is when the write was captured (unix nanos), so undo can window the
	// ledger to the writes since a given checkpoint's TimeNS.
	TimeNS int64 `json:"time_ns,omitempty"`
}

// Log is the append-only salvage version log. Single-writer: a second Open of the
// same file fails while the first holds it.
type Log struct {
	mu   sync.Mutex
	f    *os.File
	recs []Version
	size int64 // byte offset of the valid prefix / append point
}

// lockWait bounds how long Open waits for the exclusive lock. A daemon that is
// shutting down still holds this lock while it cuts its final checkpoint and
// flushes, so a restart can legitimately arrive a moment too early. Waiting
// briefly turns that overlap into a slightly slower start instead of a failure,
// while a genuinely concurrent writer still fails, just after this bound.
const lockWait = 5 * time.Second

// lockWithin takes the exclusive lock, retrying until deadline. The error names
// the process holding it when that can be determined, because "already locked"
// with no holder is the least actionable message this program can print.
func lockWithin(f *os.File, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("versionlog: %s already locked by another writer%s after waiting %s: %w",
				f.Name(), holderSuffix(f.Name()), wait, err)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// holderSuffix names the likely lock holder from the store's daemon.pid, and
// says whether that process is still alive, so the message points at something
// the user can act on.
func holderSuffix(logPath string) string {
	b, err := os.ReadFile(filepath.Join(filepath.Dir(logPath), "daemon.pid"))
	if err != nil {
		return ""
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return ""
	}
	if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); err != nil {
		return fmt.Sprintf(" (daemon.pid names pid %d, which is gone; the lock is held by some other process)", pid)
	}
	return fmt.Sprintf(" (held by the daemon at pid %d, which is still running)", pid)
}

// Open opens (creating if needed) the log at path, takes an exclusive advisory
// lock, and recovers the longest valid prefix (repairing a torn tail on disk).
func Open(path string) (*Log, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockWithin(f, lockWait); err != nil {
		f.Close()
		return nil, err
	}
	recs, valid, err := recoverPrefix(f)
	if err != nil {
		unix.Flock(int(f.Fd()), unix.LOCK_UN)
		f.Close()
		return nil, err
	}
	// Repair: drop any torn/corrupt bytes past the valid prefix so appends stay
	// clean, then position at the append point.
	if err := f.Truncate(valid); err != nil {
		unix.Flock(int(f.Fd()), unix.LOCK_UN)
		f.Close()
		return nil, err
	}
	if _, err := f.Seek(valid, 0); err != nil {
		unix.Flock(int(f.Fd()), unix.LOCK_UN)
		f.Close()
		return nil, err
	}
	return &Log{f: f, recs: recs, size: valid}, nil
}

// recoverPrefix reads f and returns the records of the longest valid prefix plus
// that prefix's byte length. Recovery stops at the first line that is
// unterminated (no trailing newline, which means a torn final append) or does
// not parse as a Version.
func recoverPrefix(f *os.File) ([]Version, int64, error) {
	data, err := os.ReadFile(f.Name())
	if err != nil {
		return nil, 0, err
	}
	var recs []Version
	var valid int64
	for {
		nl := bytes.IndexByte(data[valid:], '\n')
		if nl < 0 {
			break // no more complete lines; a trailing fragment is torn, so drop it
		}
		line := data[valid : valid+int64(nl)]
		var v Version
		if err := json.Unmarshal(line, &v); err != nil {
			break // corrupt complete line: stop; it and everything after is dropped
		}
		recs = append(recs, v)
		valid += int64(nl) + 1 // include the newline
	}
	return recs, valid, nil
}

// Read returns the versions in the log at path WITHOUT taking the writer lock,
// a read-only view for inspect/recover. It returns the current valid prefix; a
// torn tail is ignored (never repaired here). A missing file yields nil, nil.
func Read(path string) ([]Version, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	recs, _, err := recoverPrefix(f)
	return recs, err
}

// Append writes one version record durably-ordered (no fsync; see Sync). It is
// atomic at the record level: a single Write of the marshaled line + newline.
func (l *Log) Append(v Version) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	n, err := l.f.WriteAt(b, l.size)
	if err != nil {
		return err
	}
	if n != len(b) {
		return fmt.Errorf("versionlog: short write %d/%d", n, len(b))
	}
	l.size += int64(n)
	l.recs = append(l.recs, v)
	return nil
}

// Versions returns all appended versions, oldest first (a copy).
func (l *Log) Versions() []Version {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Version, len(l.recs))
	copy(out, l.recs)
	return out
}

// Sync flushes the log to disk (call at checkpoint boundaries).
func (l *Log) Sync() error { return l.f.Sync() }

// Close releases the lock and closes the file.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	unix.Flock(int(l.f.Fd()), unix.LOCK_UN)
	return l.f.Close()
}

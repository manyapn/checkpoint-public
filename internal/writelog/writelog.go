// Package writelog is the append-only record of every captured write: which
// path, what content, and who wrote it. It is what makes undo per-author and
// what keeps a file that was created and deleted between checkpoints.
package writelog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const (
	Write  = "write"
	Delete = "delete"
)

type Entry struct {
	Op     string `json:"op"`
	Path   string `json:"path"`
	Ref    string `json:"ref,omitempty"`
	Mode   uint32 `json:"mode,omitempty"`
	Writer string `json:"writer"` // agent | human | self | unknown
	TimeNS int64  `json:"time_ns"`
}

type Log struct {
	mu   sync.Mutex
	f    *os.File
	size int64
}

// Open locks the log for writing and drops a torn final line left by a crash.
func Open(path string) (*Log, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lock(f); err != nil {
		f.Close()
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		f.Close()
		return nil, err
	}
	_, valid := parse(data)
	if err := f.Truncate(valid); err != nil {
		f.Close()
		return nil, err
	}
	return &Log{f: f, size: valid}, nil
}

// A daemon shutting down still holds the lock while it cuts its last
// checkpoint, so a restart waits briefly instead of failing.
var lockWait = 5 * time.Second

func lock(f *os.File) error {
	deadline := time.Now().Add(lockWait)
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("writelog: %s is locked by another process: %w", f.Name(), err)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// parse returns the entries of the longest valid prefix and its byte length.
func parse(data []byte) ([]Entry, int64) {
	var entries []Entry
	var valid int64
	for {
		nl := bytes.IndexByte(data[valid:], '\n')
		if nl < 0 {
			return entries, valid
		}
		var e Entry
		if err := json.Unmarshal(data[valid:valid+int64(nl)], &e); err != nil {
			return entries, valid
		}
		entries = append(entries, e)
		valid += int64(nl) + 1
	}
}

func (l *Log) Append(e Entry) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	n, err := l.f.WriteAt(line, l.size)
	if err != nil {
		return err
	}
	l.size += int64(n)
	return nil
}

func (l *Log) Sync() error { return l.f.Sync() }

func (l *Log) Close() error {
	unix.Flock(int(l.f.Fd()), unix.LOCK_UN)
	return l.f.Close()
}

// Read returns the valid entries without taking the lock. A missing log is
// empty, not an error.
func Read(path string) ([]Entry, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	entries, _ := parse(data)
	return entries, nil
}

// Rewrite replaces the log with only the given entries. Only safe with no
// writer open.
func Rewrite(path string, entries []Entry) error {
	var buf bytes.Buffer
	for _, e := range entries {
		line, err := json.Marshal(e)
		if err != nil {
			return err
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

package daemon

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/manyapn/checkpoint-public/internal/snapshot"
	"github.com/manyapn/checkpoint-public/internal/watch"
)

// Timing knobs, vars so tests can shrink them.
var (
	settleQuiet      = 250 * time.Millisecond
	settleCeiling    = 2 * time.Second
	autosaveInterval = 5 * time.Minute
)

type Config struct {
	Root     string
	StoreDir string
}

type server struct {
	cfg     Config
	rec     *recorder
	writes  *watch.Writes
	changes *watch.Changes // nil when the filesystem refuses it
	prev    *snapshot.Checkpoint
	startNS int64

	captured int             // writes recorded since the last checkpoint
	dirty    map[string]bool // paths the change feed reported since the last checkpoint
	lastCut  time.Time
}

type pending struct {
	req  Request
	resp chan any
}

// Serve runs until stop is closed. ready is closed once the socket answers.
// Every request is handled on the one event loop, so checkpoints never
// overlap and status is always consistent.
func Serve(cfg Config, ready chan<- struct{}, stop <-chan struct{}) error {
	if err := snapshot.CheckStoreLocation(cfg.StoreDir, cfg.Root); err != nil {
		return err
	}
	if err := snapshot.Stamp(cfg.StoreDir, cfg.Root); err != nil {
		return err
	}
	rec, err := newRecorder(cfg.Root, cfg.StoreDir)
	if err != nil {
		return err
	}
	defer rec.close()
	trackerStop := make(chan struct{})
	go rec.tracker.Run(trackerStop)
	defer close(trackerStop)

	writes, err := watch.OpenWrites(cfg.Root)
	if err != nil {
		return err
	}
	defer writes.Close()
	s := &server{cfg: cfg, rec: rec, writes: writes, startNS: time.Now().UnixNano(),
		dirty: map[string]bool{}, lastCut: time.Now()}
	if changes, err := watch.OpenChanges(cfg.Root); err == nil {
		s.changes = changes
		defer changes.Close()
	}
	if s.prev, err = snapshot.Latest(cfg.StoreDir); err != nil {
		return err
	}

	sock := SocketPath(cfg.StoreDir)
	os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	defer os.Remove(sock)
	defer ln.Close()
	calls := make(chan pending)
	go accept(ln, calls)
	if ready != nil {
		close(ready)
	}

	if s.prev == nil {
		s.cut("setup", "", false)
	}
	for {
		select {
		case <-stop:
			s.drain()
			if res := s.cut("shutdown", "", false); res.Error != "" {
				return fmt.Errorf("shutdown checkpoint: %s", res.Error)
			}
			return nil
		case p := <-calls:
			p.resp <- s.handle(p.req)
		default:
		}
		if s.poll(100) {
			s.drain()
		}
		if len(s.rec.agentRoots()) > 0 && time.Since(s.lastCut) > autosaveInterval && s.activity() {
			s.cut("autosave", "", false)
		}
	}
}

func accept(ln net.Listener, calls chan<- pending) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(30 * time.Second))
			var req Request
			if err := json.NewDecoder(conn).Decode(&req); err != nil {
				json.NewEncoder(conn).Encode(Result{Error: "bad request: " + err.Error()})
				return
			}
			c := pending{req, make(chan any, 1)}
			calls <- c
			json.NewEncoder(conn).Encode(<-c.resp)
		}()
	}
}

func (s *server) handle(req Request) any {
	switch req.Op {
	case "status":
		return s.status()
	case "register":
		s.rec.setAgent(req.Agent, true)
		return Result{}
	case "unregister":
		s.rec.setAgent(req.Agent, false)
		return Result{}
	case "checkpoint":
		return s.cut(req.Source, req.Name, s.settle())
	}
	return Result{Error: "unknown op " + req.Op}
}

func (s *server) status() Status {
	ids, _ := snapshot.IDs(s.cfg.StoreDir)
	st := Status{Protected: true, Root: s.cfg.Root, Checkpoints: len(ids), SinceNS: s.startNS,
		AgentSessions: len(s.rec.agentRoots()), FeedActive: s.changes != nil,
		Missed: append([]string{}, s.rec.missed...), Overflowed: s.overflowed(),
		Outside: append([]string{}, s.rec.outside...), OutsideCount: s.rec.outsideCount}
	if s.prev != nil {
		st.LastCheckpointNS = s.prev.TimeNS
	}
	return st
}

func (s *server) poll(timeoutMs int) bool {
	if s.changes != nil {
		return watch.Poll(timeoutMs, s.writes.Fd(), s.changes.Fd())
	}
	return watch.Poll(timeoutMs, s.writes.Fd())
}

// drain moves queued kernel events into the write log and the dirty set.
// Deletes go first: a deleting process (rm) is short-lived, and classifying
// it before the slower content reads gives the best chance that it is
// still alive to be asked about.
func (s *server) drain() int {
	if s.changes != nil {
		for _, ch := range s.changes.Drain() {
			s.dirty[ch.Path] = true
			if (ch.Op == watch.Deleted || ch.Op == watch.RenamedFrom) && !ch.Dir {
				s.rec.delete(ch.Path, ch.Pid)
			}
		}
	}
	n := 0
	s.writes.Drain(func(path string, fd int, pid int) {
		if s.rec.write(path, fd, pid) {
			n++
		}
	})
	s.captured += n
	return n
}

func (s *server) overflowed() bool {
	return s.writes.Overflowed || (s.changes != nil && s.changes.Overflowed)
}

func (s *server) activity() bool {
	return s.captured > 0 || len(s.dirty) > 0 || len(s.rec.missed) > 0 || s.overflowed()
}

// settle waits for writes to go quiet after a boundary request, so a command
// that is still flushing is not caught mid-write. Reports whether the
// ceiling was hit with writes still arriving.
func (s *server) settle() (timedOut bool) {
	last := time.Now()
	ceiling := last.Add(settleCeiling)
	for {
		if s.drain() > 0 {
			last = time.Now()
		}
		if time.Since(last) >= settleQuiet {
			return false
		}
		if time.Now().After(ceiling) {
			return true
		}
		s.poll(20)
	}
}

// cut records the tree now. With a working change feed and nothing changed,
// an unnamed request is skipped rather than producing a duplicate.
func (s *server) cut(source, name string, settleTimedOut bool) Result {
	s.drain()
	if name == "" && s.changes != nil && s.prev != nil && !s.activity() {
		return Result{ID: s.prev.ID, Badge: s.prev.Badge(), Entries: len(s.prev.Entries), Skipped: true}
	}
	c, err := s.record()
	if err != nil {
		return Result{Error: err.Error()}
	}
	c.Source, c.Name, c.SettleTimedOut = source, name, settleTimedOut
	if err := snapshot.Commit(s.cfg.StoreDir, c); err != nil {
		return Result{Error: err.Error()}
	}
	s.rec.log.Sync()
	s.startWindow(c)
	return Result{ID: c.ID, Badge: c.Badge(), Entries: len(c.Entries), SettleTimedOut: settleTimedOut}
}

// record builds the checkpoint: a fold over the change set when the feed
// is live and complete, a full scan otherwise. What this window could not
// capture is stamped on it.
func (s *server) record() (*snapshot.Checkpoint, error) {
	var c *snapshot.Checkpoint
	var err error
	if s.changes != nil && !s.changes.Overflowed && s.prev != nil {
		changed := make([]string, 0, len(s.dirty))
		for p := range s.dirty {
			changed = append(changed, p)
		}
		c, err = snapshot.Fold(s.cfg.Root, s.rec.objs, s.prev, changed)
	} else {
		c, err = snapshot.Scan(s.cfg.Root, s.rec.objs, s.prev)
	}
	if err != nil {
		return nil, err
	}
	c.EventsDropped = s.writes.Overflowed
	for _, p := range s.rec.missed {
		rel, _ := filepath.Rel(s.cfg.Root, p)
		c.Exceptions = append(c.Exceptions, snapshot.Exception{Path: rel, Reason: "write not captured"})
	}
	return c, nil
}

// startWindow makes c the base for the next checkpoint and clears every
// per-window counter.
func (s *server) startWindow(c *snapshot.Checkpoint) {
	s.prev = c
	s.captured, s.dirty, s.rec.missed = 0, map[string]bool{}, nil
	s.writes.Overflowed = false
	if s.changes != nil {
		s.changes.Overflowed = false
	}
	s.lastCut = time.Now()
}

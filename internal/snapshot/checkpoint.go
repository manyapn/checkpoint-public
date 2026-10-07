// Package snapshot records the whole protected tree at one instant and puts
// it back. Checkpoints live outside the project so `rm -rf project` cannot
// take its history with it.
package snapshot

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	File    = "file"
	Dir     = "dir"
	Symlink = "symlink"
)

type Entry struct {
	Kind    string `json:"kind"`
	Ref     string `json:"ref,omitempty"`
	Mode    uint32 `json:"mode,omitempty"`
	Link    string `json:"link,omitempty"`
	Size    int64  `json:"size,omitempty"`
	MtimeNS int64  `json:"mtime_ns,omitempty"`
	CtimeNS int64  `json:"ctime_ns,omitempty"`
}

// Exception names one path a checkpoint could not cover and why.
type Exception struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

type Checkpoint struct {
	ID             int              `json:"id"`
	TimeNS         int64            `json:"time_ns"`
	Root           string           `json:"root"`
	Source         string           `json:"source"`
	Name           string           `json:"name,omitempty"`
	EventsDropped  bool             `json:"events_dropped,omitempty"`
	SettleTimedOut bool             `json:"settle_timed_out,omitempty"`
	ScanNS         int64            `json:"scan_ns"`
	Entries        map[string]Entry `json:"entries"`
	Exceptions     []Exception      `json:"exceptions,omitempty"`
}

// Badge grades how much of the window this checkpoint vouches for. The tree
// itself is always exact; EventsDropped means writes between checkpoints may
// have gone unrecorded (authorship, transient files).
func (c *Checkpoint) Badge() string {
	switch {
	case c.EventsDropped:
		return "Incomplete"
	case len(c.Exceptions) > 0:
		return "Recoverable with exceptions"
	default:
		return "Fully recoverable"
	}
}

func checkpointsDir(storeDir string) string { return filepath.Join(storeDir, "checkpoints") }

func checkpointPath(storeDir string, id int) string {
	return filepath.Join(checkpointsDir(storeDir), strconv.Itoa(id)+".json")
}

// Commit assigns the next free id and writes the checkpoint. Two writers
// (the daemon and a CLI command) can race for an id; link(2) refuses to
// replace an existing file, so the loser just takes the next id.
func Commit(storeDir string, c *Checkpoint) error {
	dir := checkpointsDir(storeDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for attempt := 0; attempt < 8; attempt++ {
		ids, err := IDs(storeDir)
		if err != nil {
			return err
		}
		c.ID = 0
		if len(ids) > 0 {
			c.ID = ids[len(ids)-1] + 1
		}
		data, err := json.Marshal(c)
		if err != nil {
			return err
		}
		tmp, err := os.CreateTemp(dir, "tmp-")
		if err != nil {
			return err
		}
		_, werr := tmp.Write(data)
		cerr := tmp.Close()
		if werr != nil || cerr != nil {
			os.Remove(tmp.Name())
			return errors.Join(werr, cerr)
		}
		err = os.Link(tmp.Name(), checkpointPath(storeDir, c.ID))
		os.Remove(tmp.Name())
		if err == nil {
			return nil
		}
		if !errors.Is(err, os.ErrExist) {
			return err
		}
	}
	return fmt.Errorf("snapshot: could not claim a checkpoint id after 8 tries")
}

func Load(storeDir string, id int) (*Checkpoint, error) {
	data, err := os.ReadFile(checkpointPath(storeDir, id))
	if err != nil {
		return nil, err
	}
	var c Checkpoint
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("snapshot: checkpoint %d is corrupt: %w", id, err)
	}
	return &c, nil
}

// IDs lists every checkpoint id on disk, ascending, torn files included so
// Commit never reuses a number.
func IDs(storeDir string) ([]int, error) {
	ents, err := os.ReadDir(checkpointsDir(storeDir))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []int
	for _, e := range ents {
		id, err := strconv.Atoi(strings.TrimSuffix(e.Name(), ".json"))
		if err == nil && strings.HasSuffix(e.Name(), ".json") {
			ids = append(ids, id)
		}
	}
	sort.Ints(ids)
	return ids, nil
}

// All loads every readable checkpoint, oldest first.
func All(storeDir string) ([]*Checkpoint, error) {
	ids, err := IDs(storeDir)
	if err != nil {
		return nil, err
	}
	var all []*Checkpoint
	for _, id := range ids {
		if c, err := Load(storeDir, id); err == nil {
			all = append(all, c)
		}
	}
	return all, nil
}

// Latest returns the newest readable checkpoint, or nil.
func Latest(storeDir string) (*Checkpoint, error) {
	ids, err := IDs(storeDir)
	if err != nil {
		return nil, err
	}
	for i := len(ids) - 1; i >= 0; i-- {
		if c, err := Load(storeDir, ids[i]); err == nil {
			return c, nil
		}
	}
	return nil, nil
}

func Remove(storeDir string, id int) error {
	return os.Remove(checkpointPath(storeDir, id))
}

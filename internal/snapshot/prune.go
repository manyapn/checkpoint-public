package snapshot

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/manyapn/checkpoint-public/internal/objects"
	"github.com/manyapn/checkpoint-public/internal/writelog"
)

const LogFile = "writes.jsonl"

type PruneReport struct {
	RemovedCheckpoints []int
	RemovedObjects     int
	RemovedBytes       int64
	ExpiredWrites      int
}

// Prune drops unnamed checkpoints older than keepDays (never the latest),
// trims the write log to the oldest kept checkpoint, then deletes objects
// nothing references. Run only with the daemon stopped.
func Prune(storeDir string, objs *objects.Store, keepDays int, now time.Time, dryRun bool) (PruneReport, error) {
	var rep PruneReport
	all, err := All(storeDir)
	if err != nil {
		return rep, err
	}
	cutoff := now.Add(-time.Duration(keepDays) * 24 * time.Hour).UnixNano()
	var kept []*Checkpoint
	for i, c := range all {
		old := c.TimeNS < cutoff
		if old && c.Name == "" && i != len(all)-1 {
			rep.RemovedCheckpoints = append(rep.RemovedCheckpoints, c.ID)
			continue
		}
		kept = append(kept, c)
	}
	keepAfter := int64(0)
	if len(kept) > 0 {
		keepAfter = kept[0].TimeNS
	}
	writes, err := writelog.Read(filepath.Join(storeDir, LogFile))
	if err != nil {
		return rep, err
	}
	var keptWrites []writelog.Entry
	for _, w := range writes {
		if w.TimeNS >= keepAfter {
			keptWrites = append(keptWrites, w)
		}
	}
	rep.ExpiredWrites = len(writes) - len(keptWrites)

	referenced := map[string]bool{}
	for _, c := range kept {
		for _, e := range c.Entries {
			referenced[e.Ref] = true
		}
	}
	for _, w := range keptWrites {
		referenced[w.Ref] = true
	}
	refs, err := objs.List()
	if err != nil {
		return rep, err
	}
	var garbage []string
	for _, ref := range refs {
		if !referenced[ref] {
			garbage = append(garbage, ref)
			rep.RemovedObjects++
			rep.RemovedBytes += objs.Size(ref)
		}
	}
	if dryRun {
		return rep, nil
	}
	for _, id := range rep.RemovedCheckpoints {
		if err := Remove(storeDir, id); err != nil && !os.IsNotExist(err) {
			return rep, err
		}
	}
	if rep.ExpiredWrites > 0 {
		if err := writelog.Rewrite(filepath.Join(storeDir, LogFile), keptWrites); err != nil {
			return rep, err
		}
	}
	for _, ref := range garbage {
		if err := objs.Delete(ref); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

type Usage struct {
	Bytes       int64
	Objects     int
	Checkpoints int
}

func MeasureUsage(storeDir string) Usage {
	var u Usage
	filepath.WalkDir(storeDir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if fi, err := d.Info(); err == nil && fi.Mode().IsRegular() {
			u.Bytes += fi.Size()
		}
		rel, _ := filepath.Rel(storeDir, p)
		parts := strings.Split(rel, "/")
		switch {
		case len(parts) == 3 && parts[0] == "objects" && objects.ValidRef(parts[1]+parts[2]):
			u.Objects++
		case len(parts) == 2 && parts[0] == "checkpoints" && filepath.Ext(rel) == ".json":
			u.Checkpoints++
		}
		return nil
	})
	return u
}

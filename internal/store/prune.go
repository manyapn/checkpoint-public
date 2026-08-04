package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/manyapn/checkpoint-public/internal/objstore"
	"github.com/manyapn/checkpoint-public/internal/versionlog"
)

// PruneOpts configures one prune pass.
type PruneOpts struct {
	KeepDays int   // unnamed manifests older than this many days are removal candidates
	NowNS    int64 // "now" for the age computation (explicit so callers/tests are deterministic)
	DryRun   bool  // compute the full report; delete nothing
}

// PruneReport is what one pass removed (or, under DryRun, would remove).
type PruneReport struct {
	RemovedManifests []int // unnamed old manifests removed
	KeptNamed        []int // named manifests old enough to be candidates, kept by name
	RemovedObjects   int   // unreferenced objects GC'd
	RemovedBytes     int64 // their on-disk (compressed) bytes
	ExpiredVersions  int   // versionlog records dropped by compaction
}

// Prune is checkpoint's retention engine. Removal candidates are manifests
// that are UNNAMED and older than KeepDays relative to NowNS, but NEVER the
// latest DURABLE manifest (the incremental/undo baseline) and NEVER any named
// manifest. PARTIAL manifests are prunable like unnamed ones. After manifest
// removal the versionlog is compacted to the oldest retained manifest's TimeNS,
// then GC deletes every object referenced by NO remaining manifest (Entries +
// Extra trees) and NO remaining versionlog record.
//
// Offline-only: the caller guarantees exclusive store access (the CLI refuses
// while a daemon answers on the store's socket). Rerunning after an
// interruption completes the pass: already-removed manifests are simply no
// longer candidates, compaction converges, and GC is idempotent.
func Prune(storeDir string, oc *objstore.Store, opts PruneOpts) (PruneReport, error) {
	var rep PruneReport
	ids, err := IDs(storeDir)
	if err != nil {
		return rep, err
	}
	cutoff := opts.NowNS - int64(opts.KeepDays)*int64(24*time.Hour)

	loaded := map[int]*Manifest{}
	latestDurable := -1
	for _, id := range ids {
		m, err := Load(storeDir, id)
		if err != nil {
			continue // torn/unparsable: not restorable, but never deleted blind either
		}
		loaded[id] = m
		if m.Coverage == DURABLE && id > latestDurable {
			latestDurable = id
		}
	}

	retained := map[int]*Manifest{}
	for id, m := range loaded {
		switch {
		case m.TimeNS >= cutoff: // young enough: not a candidate
			retained[id] = m
		case m.Name != "": // named checkpoints survive pruning
			retained[id] = m
			rep.KeptNamed = append(rep.KeptNamed, id)
		case id == latestDurable: // the incremental/undo baseline: never pruned
			retained[id] = m
		default:
			rep.RemovedManifests = append(rep.RemovedManifests, id)
		}
	}
	sort.Ints(rep.RemovedManifests)
	sort.Ints(rep.KeptNamed)

	if !opts.DryRun {
		for _, id := range rep.RemovedManifests {
			if err := RemoveManifest(storeDir, id); err != nil && !os.IsNotExist(err) {
				return rep, err // IsNotExist: an interrupted earlier pass already removed it
			}
		}
	}

	// Versionlog compaction: records older than the oldest retained manifest
	// back no undo window and no restorable state, so they are dropped (their
	// salvage offers expire with them).
	keepAfterNS := int64(0)
	for _, m := range retained {
		if keepAfterNS == 0 || m.TimeNS < keepAfterNS {
			keepAfterNS = m.TimeNS
		}
	}
	vlogPath := filepath.Join(storeDir, "versionlog")
	recs, err := versionlog.Read(vlogPath)
	if err != nil {
		return rep, err
	}
	if opts.DryRun {
		for _, v := range recs {
			if keepAfterNS > 0 && v.TimeNS < keepAfterNS {
				rep.ExpiredVersions++
			}
		}
	} else {
		n, err := CompactVersions(storeDir, keepAfterNS)
		if err != nil {
			return rep, err
		}
		rep.ExpiredVersions = n
	}

	// GC: an object referenced by NO remaining manifest and NO remaining
	// versionlog record is unreachable. Only refs List returned are touched.
	referenced := map[string]bool{}
	for _, m := range retained {
		for _, e := range m.Entries {
			if e.Ref != "" {
				referenced[e.Ref] = true
			}
		}
		for _, entries := range m.Extra {
			for _, e := range entries {
				if e.Ref != "" {
					referenced[e.Ref] = true
				}
			}
		}
	}
	for _, v := range recs {
		if keepAfterNS > 0 && v.TimeNS < keepAfterNS {
			continue // compacted away (or would be, under DryRun)
		}
		if v.Ref != "" {
			referenced[v.Ref] = true
		}
	}
	refs, err := oc.List()
	if err != nil {
		return rep, err
	}
	for _, ref := range refs {
		if referenced[ref] {
			continue
		}
		if sz, err := oc.Size(ref); err == nil {
			rep.RemovedBytes += sz
		}
		rep.RemovedObjects++
		if !opts.DryRun {
			if err := oc.Delete(ref); err != nil {
				return rep, err
			}
		}
	}
	return rep, nil
}

// CompactVersions drops versionlog records with TimeNS < keepAfterNS and
// rewrites the log atomically (tmp + rename). keepAfterNS <= 0 is a no-op, as
// is a missing or already-compact log. Offline-only: the caller guarantees no
// daemon holds the log, since renaming under a live writer's fd would fork it.
func CompactVersions(storeDir string, keepAfterNS int64) (removed int, err error) {
	if keepAfterNS <= 0 {
		return 0, nil
	}
	logPath := filepath.Join(storeDir, "versionlog")
	recs, err := versionlog.Read(logPath)
	if err != nil {
		return 0, err
	}
	kept := recs[:0:0]
	for _, v := range recs {
		if v.TimeNS >= keepAfterNS {
			kept = append(kept, v)
		}
	}
	removed = len(recs) - len(kept)
	if removed == 0 {
		return 0, nil
	}
	tmp, err := os.CreateTemp(storeDir, "vlog-")
	if err != nil {
		return 0, err
	}
	tmpName := tmp.Name()
	for _, v := range kept {
		b, err := json.Marshal(v)
		if err != nil {
			tmp.Close()
			os.Remove(tmpName)
			return 0, err
		}
		if _, err := tmp.Write(append(b, '\n')); err != nil {
			tmp.Close()
			os.Remove(tmpName)
			return 0, err
		}
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return 0, err
	}
	if err := os.Rename(tmpName, logPath); err != nil {
		os.Remove(tmpName)
		return 0, err
	}
	return removed, nil
}

// RemoveManifest deletes manifest <id>'s file. Prune-only: every other path
// treats manifests as immutable once written (store.Write refuses replacement).
func RemoveManifest(storeDir string, id int) error {
	return os.Remove(filepath.Join(storeDir, "manifests", fmt.Sprintf("%d.json", id)))
}

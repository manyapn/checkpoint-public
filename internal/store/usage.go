package store

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/manyapn/checkpoint-public/internal/objstore"
	"github.com/manyapn/checkpoint-public/internal/versionlog"
)

// Usage is what a store costs on disk, right now. Every byte figure is the
// COMPRESSED, on-disk size (the number the user's `df` moves by), never the
// logical size of the content the store can reproduce (which is larger, often
// by a lot: objects are zlib blobs and identical content is stored once).
//
// The same shape describes both "what is stored" (MeasureUsage) and "what a
// prune would free" (EstimateReclaim), so a caller can print them side by side.
type Usage struct {
	Objects         int   `json:"objects"`          // content blobs in objects/ (valid refs only)
	ObjectBytes     int64 `json:"object_bytes"`     // their on-disk, compressed bytes
	Manifests       int   `json:"manifests"`        // checkpoints whose manifest file PARSES
	NamedManifests  int   `json:"named_manifests"`  // of those, the ones with a --name (prune never removes them)
	VersionLogBytes int64 `json:"versionlog_bytes"` // the per-write ledger's bytes
	TotalBytes      int64 `json:"total_bytes"`      // every regular file under the store dir, incl. manifests and store.json
	OldestNS        int64 `json:"oldest_ns"`        // oldest counted checkpoint's TimeNS (0 when there are none)
	NewestNS        int64 `json:"newest_ns"`        // newest counted checkpoint's TimeNS (0 when there are none)
}

// manifestMeta is the slice of a manifest MeasureUsage needs. Decoding into it
// (rather than into Manifest) makes encoding/json SKIP the entries/extra maps
// with its scanner instead of allocating a map per checkpoint. That is the
// difference between "cheap enough for status" and "parse the whole history".
type manifestMeta struct {
	TimeNS int64  `json:"time_ns"`
	Name   string `json:"name"`
}

// MeasureUsage reports what the store at storeDir occupies on disk.
//
// Cost: ONE directory walk of the store (objects/ is inherently O(objects);
// there is no cheaper way to know how many bytes are down there), with exactly
// one stat per file, taken from the walk's own directory entry. No file is
// stat'ed twice and no object is opened. On top of the walk it streams each
// manifest file through a JSON decoder to read its timestamp and name; that is
// O(manifest bytes), which is why the decode target is a two-field struct
// (encoding/json then SKIPS the entries/extra maps with its scanner instead of
// allocating them).
//
// The manifest decode dominates on any store with real history, so the total
// cost is linear in store size: fine for a user-typed `status`, and not
// something to call in a loop.
//
// Honesty rules:
//   - A missing, empty, or manifests-less store is the zero Usage and a nil
//     error: "nothing stored yet" is a fact, not a failure.
//   - TotalBytes counts EVERY regular file under the store, including files
//     these fields do not break out (store.json, an operation journal, a
//     daemon log, a tmp- file left by a crash). It is therefore >= the sum of
//     the parts, never less: the disk sees all of it.
//   - A torn or unparsable manifest is NOT counted as a checkpoint (nothing can
//     be restored from it) but its bytes ARE counted in TotalBytes (they are
//     still occupying the disk). Same for a tmp- object left mid-Put.
//   - Non-regular files (the daemon socket) contribute nothing.
//   - Entries that vanish mid-walk (a concurrent prune, a daemon compaction)
//     are skipped rather than turned into an error: a status command must not
//     fail because the store was busy. The result is then a snapshot of a
//     moving target, which is what any usage number is.
func MeasureUsage(storeDir string) (Usage, error) {
	var u Usage
	var manifestFiles []string

	err := filepath.WalkDir(storeDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil // vanished under us (concurrent prune); not an error
			}
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		size := fi.Size()
		u.TotalBytes += size

		rel, relErr := filepath.Rel(storeDir, p)
		if relErr != nil {
			return nil
		}
		parts := strings.Split(rel, string(filepath.Separator))
		switch {
		case len(parts) == 3 && parts[0] == "objects" && isObjectRef(parts[1]+parts[2]):
			u.Objects++
			u.ObjectBytes += size
		case len(parts) == 2 && parts[0] == "manifests" && isManifestFile(parts[1]):
			manifestFiles = append(manifestFiles, p)
		case rel == "versionlog":
			u.VersionLogBytes += size
		}
		return nil
	})
	if err != nil {
		if os.IsNotExist(err) {
			return Usage{}, nil // no store yet: zeroes, not an error
		}
		return Usage{}, fmt.Errorf("measuring store usage in %s: %w", storeDir, err)
	}

	for _, p := range manifestFiles {
		meta, ok := readManifestMeta(p)
		if !ok {
			continue // torn/unparsable: bytes counted above, never a checkpoint
		}
		u.Manifests++
		if meta.Name != "" {
			u.NamedManifests++
		}
		u.noteTime(meta.TimeNS)
	}
	return u, nil
}

// noteTime folds one checkpoint timestamp into the oldest/newest window.
// Non-positive timestamps are ignored: a manifest with no clock stamp must not
// make the store look infinitely old.
func (u *Usage) noteTime(ns int64) {
	if ns <= 0 {
		return
	}
	if u.OldestNS == 0 || ns < u.OldestNS {
		u.OldestNS = ns
	}
	if ns > u.NewestNS {
		u.NewestNS = ns
	}
}

// readManifestMeta streams one manifest's metadata. ok is false for anything
// unreadable or unparsable; the caller decides what that means.
func readManifestMeta(path string) (manifestMeta, bool) {
	f, err := os.Open(path)
	if err != nil {
		return manifestMeta{}, false
	}
	defer f.Close()
	var m manifestMeta
	if err := json.NewDecoder(f).Decode(&m); err != nil {
		return manifestMeta{}, false
	}
	return m, true
}

// isObjectRef reports whether name is a 40-char lowercase-hex object ref, the
// same shape objstore accepts, applied here to a directory entry so that a
// tmp- file left by a crashed Put is never counted as a stored object.
func isObjectRef(ref string) bool {
	if len(ref) != 40 {
		return false
	}
	for _, c := range ref {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// isManifestFile reports whether name is a manifest file (<id>.json), excluding
// the tmp- files Write leaves behind on a crash.
func isManifestFile(name string) bool {
	if !strings.HasSuffix(name, ".json") {
		return false
	}
	_, err := strconv.Atoi(strings.TrimSuffix(name, ".json"))
	return err == nil
}

// EstimateReclaim answers "how much would `prune --keep-days N` give me back?"
// WITHOUT touching the store. It is the dry-run Prune path (the same retention
// decisions the real prune makes, from the same code), with the resulting plan
// priced in bytes.
//
// The returned Usage describes what would be FREED:
//
//	Objects/ObjectBytes   objects GC would delete, and their on-disk bytes
//	Manifests             manifest files prune would remove
//	NamedManifests        always 0, since prune never removes a named checkpoint
//	VersionLogBytes       bytes versionlog compaction would drop
//	TotalBytes            all of the above, including the manifest files' own bytes
//	OldestNS/NewestNS     the age span of the checkpoints that would go
//
// Guarantee: every field is bounded by the corresponding MeasureUsage field,
// because a prune can only ever free a subset of what is stored. Cost: heavier
// than MeasureUsage (it parses every manifest and reads the version log), so
// call it for a prune preview, not on a hot path.
//
// A missing store is the zero Usage and a nil error.
func EstimateReclaim(storeDir string, keepDays int, nowNS int64) (Usage, error) {
	var u Usage
	if _, err := os.Stat(storeDir); err != nil {
		if os.IsNotExist(err) {
			return Usage{}, nil
		}
		return Usage{}, err
	}
	oc, err := objstore.Open(storeDir)
	if err != nil {
		return Usage{}, err
	}
	rep, err := Prune(storeDir, oc, PruneOpts{KeepDays: keepDays, NowNS: nowNS, DryRun: true})
	if err != nil {
		return Usage{}, err
	}

	u.Objects = rep.RemovedObjects
	u.ObjectBytes = rep.RemovedBytes
	u.NamedManifests = 0 // by construction: named checkpoints are never candidates

	doomed := make(map[int]bool, len(rep.RemovedManifests))
	for _, id := range rep.RemovedManifests {
		doomed[id] = true
	}

	// Price the doomed manifests and find the retention floor. keepAfterNS is
	// the oldest RETAINED checkpoint's timestamp, exactly the cut Prune hands to
	// CompactVersions. It is derived here from Prune's own plan, not re-decided.
	var manifestBytes, keepAfterNS int64
	ids, err := IDs(storeDir)
	if err != nil {
		return Usage{}, err
	}
	for _, id := range ids {
		path := filepath.Join(storeDir, "manifests", fmt.Sprintf("%d.json", id))
		meta, ok := readManifestMeta(path)
		if !ok {
			continue // unparsable: Prune never removes it, so it frees nothing
		}
		if !doomed[id] {
			if meta.TimeNS > 0 && (keepAfterNS == 0 || meta.TimeNS < keepAfterNS) {
				keepAfterNS = meta.TimeNS
			}
			continue
		}
		u.Manifests++
		u.noteTime(meta.TimeNS)
		if fi, err := os.Stat(path); err == nil {
			manifestBytes += fi.Size()
		}
	}

	if rep.ExpiredVersions > 0 && keepAfterNS > 0 {
		freed, err := estimateCompactionBytes(storeDir, keepAfterNS)
		if err != nil {
			return Usage{}, err
		}
		u.VersionLogBytes = freed
	}

	u.TotalBytes = u.ObjectBytes + manifestBytes + u.VersionLogBytes
	return u, nil
}

// estimateCompactionBytes prices versionlog compaction: the rewritten log holds
// exactly the records at or after keepAfterNS, one marshaled line each, so the
// freed bytes are the current file size minus that. Anything the log's recovery
// pass rejects (a torn tail) is freed too, because compaction rewrites the file
// from the valid prefix. Never negative: a log that would not shrink frees
// nothing.
func estimateCompactionBytes(storeDir string, keepAfterNS int64) (int64, error) {
	path := filepath.Join(storeDir, "versionlog")
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	recs, err := versionlog.Read(path)
	if err != nil {
		return 0, err
	}
	var keptBytes int64
	for _, v := range recs {
		if v.TimeNS < keepAfterNS {
			continue
		}
		b, err := json.Marshal(v)
		if err != nil {
			return 0, err
		}
		keptBytes += int64(len(b)) + 1 // + the newline Append writes
	}
	if freed := fi.Size() - keptBytes; freed > 0 {
		return freed, nil
	}
	return 0, nil
}

// Human renders a Usage as one line a stranger can act on, e.g.
//
//	412 MB across 1,203 objects and 37 checkpoints (3 named, oldest 6 days ago)
//
// Ages are relative to time.Now(). An empty store says so in words rather than
// printing a row of zeroes. Byte sizes are decimal (kB = 1000 B), matching what
// `df -h --si` and most cloud dashboards show.
func (u Usage) Human() string {
	if u.TotalBytes == 0 && u.Objects == 0 && u.Manifests == 0 {
		return "empty (nothing stored yet)"
	}
	s := fmt.Sprintf("%s across %s and %s",
		humanBytes(u.TotalBytes),
		plural(u.Objects, "object", "objects"),
		plural(u.Manifests, "checkpoint", "checkpoints"))

	var notes []string
	if u.NamedManifests > 0 {
		notes = append(notes, fmt.Sprintf("%d named", u.NamedManifests))
	}
	if u.OldestNS > 0 {
		notes = append(notes, "oldest "+humanAge(time.Now().UnixNano()-u.OldestNS))
	}
	if len(notes) > 0 {
		s += " (" + strings.Join(notes, ", ") + ")"
	}
	return s
}

// plural renders "1 object" / "2 objects" with thousands separators.
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return commas(int64(n)) + " " + many
}

// commas groups an integer in threes: 1203 -> "1,203".
func commas(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}

// humanBytes renders decimal (SI) byte sizes: 999 B, 1.2 kB, 412 MB, 3.4 GB.
// Values below 10 in a unit keep one decimal so "1.2 GB" is not rounded to
// "1 GB"; above that the decimal is noise.
func humanBytes(n int64) string {
	if n < 1000 {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"kB", "MB", "GB", "TB", "PB"}
	v := float64(n)
	unit := units[0]
	for _, un := range units {
		v /= 1000
		unit = un
		if v < 1000 {
			break
		}
	}
	if v < 10 {
		return fmt.Sprintf("%.1f %s", v, unit)
	}
	return fmt.Sprintf("%.0f %s", v, unit)
}

// humanAge renders a duration as a coarse "N units ago" phrase. Anything under
// a minute is "just now"; a negative age (clock skew between machines) is
// reported as "just now" rather than as a time traveller.
func humanAge(ns int64) string {
	d := time.Duration(ns)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return countAgo(int(d/time.Minute), "minute")
	case d < 24*time.Hour:
		return countAgo(int(d/time.Hour), "hour")
	default:
		return countAgo(int(d/(24*time.Hour)), "day")
	}
}

func countAgo(n int, unit string) string {
	if n == 1 {
		return "1 " + unit + " ago"
	}
	return fmt.Sprintf("%d %ss ago", n, unit)
}

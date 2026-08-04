package store

import (
	"sort"

	"github.com/manyapn/checkpoint-public/internal/objstore"
	"github.com/manyapn/checkpoint-public/internal/versionlog"
)

// MaxSalvageEntries caps how many salvageable paths are retained: beyond it the
// OLDEST (by last captured write) are evicted from the offer. A bound to keep
// the offer list finite, not a retention policy: expiry is the version log's
// job (see CompactVersions). Package var only so the eviction test can exercise
// the cap without building thousands of versions.
var MaxSalvageEntries = 1000

// Salvage returns the latest captured content ref for every path that has a
// recoverable version but is ABSENT from every given manifest: a file
// checkpoint can bring back even though it was never part of a saved
// checkpoint, e.g. a transient created→written→deleted between boundaries.
// These back the user-facing "Recovered file" escape hatch: their bytes survive
// in the object store even though no workspace snapshot lists them.
//
// A deletion does NOT clear a path's salvageability: a deleted transient is the
// canonical loss this escape hatch exists for, and since the change feed
// journals every unlink as OpDelete, a delete-clears rule would erase salvage
// entirely. What a delete record means here is only that the latest CONTENT
// version is what's recoverable. A path any manifest lists is never offered; it
// is restorable by checkpoint id instead. A ref not actually present in oc is
// never offered either, because Salvage promises only what it can genuinely
// recover. ms may be empty (no checkpoint yet): every captured version is
// salvageable.
//
// The second return is the number of offers EVICTED by the cap. Callers must
// surface it: a truncated list presented as complete reads as "that's
// everything recoverable" when it isn't.
func Salvage(versions []versionlog.Version, ms []*Manifest, oc *objstore.Store) (map[string]string, int) {
	inAnyManifest := func(abs string) bool {
		for _, m := range ms {
			if m == nil {
				continue
			}
			for rel := range m.Entries {
				if m.Root+"/"+rel == abs {
					return true
				}
			}
			for root, entries := range m.Extra { // extra protected folders count too
				for rel := range entries {
					if root+"/"+rel == abs {
						return true
					}
				}
			}
		}
		return false
	}
	type cand struct {
		ref string
		idx int // index of the path's latest content version (recency)
	}
	latest := map[string]cand{} // path -> latest recoverable content ref
	for i, v := range versions {
		if v.Op == versionlog.OpDelete {
			continue // provenance, not content; the last content stays offerable
		}
		if v.Ref != "" && oc.Has(v.Ref) {
			latest[v.Path] = cand{ref: v.Ref, idx: i}
		}
	}
	type offer struct {
		path string
		cand
	}
	var offers []offer
	for path, c := range latest {
		if !inAnyManifest(path) {
			offers = append(offers, offer{path: path, cand: c})
		}
	}
	// Cap: newest first survive; the oldest beyond MaxSalvageEntries are evicted.
	sort.Slice(offers, func(i, j int) bool { return offers[i].idx > offers[j].idx })
	evicted := 0
	if len(offers) > MaxSalvageEntries {
		evicted = len(offers) - MaxSalvageEntries
		offers = offers[:MaxSalvageEntries]
	}
	out := map[string]string{}
	for _, o := range offers {
		out[o.path] = o.ref
	}
	return out, evicted
}

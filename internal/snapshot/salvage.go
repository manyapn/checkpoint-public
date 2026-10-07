package snapshot

import (
	"github.com/manyapn/checkpoint-public/internal/objects"
	"github.com/manyapn/checkpoint-public/internal/writelog"
)

// Salvage returns the latest captured content for every path that no
// checkpoint holds: files created and deleted between checkpoints.
func Salvage(writes []writelog.Entry, checkpoints []*Checkpoint, objs *objects.Store) map[string]string {
	held := map[string]bool{}
	for _, c := range checkpoints {
		for rel := range c.Entries {
			held[c.Root+"/"+rel] = true
		}
	}
	latest := map[string]string{}
	for _, w := range writes {
		if w.Op == writelog.Write && !held[w.Path] && objs.Has(w.Ref) {
			latest[w.Path] = w.Ref
		}
	}
	return latest
}

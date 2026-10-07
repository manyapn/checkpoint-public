// Package undo reverts the agent's changes from the latest turn and leaves
// the human's alone. It never merges: a file both touched is reported and
// skipped. Under-reverting is the safe direction.
package undo

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/manyapn/checkpoint-public/internal/lineage"
	"github.com/manyapn/checkpoint-public/internal/objects"
	"github.com/manyapn/checkpoint-public/internal/snapshot"
	"github.com/manyapn/checkpoint-public/internal/writelog"
)

const (
	Revert   = "revert"   // agent-only change to an existing file: put the baseline back
	Remove   = "remove"   // agent-only file that did not exist at baseline
	Conflict = "conflict" // someone else also wrote it: leave it, report it
)

// Sources of checkpoints that record what checkpoint itself was about to do.
// They are never "the turn".
const (
	PreUndo    = "pre-undo"
	PreRestore = "pre-restore"
)

type Step struct {
	Rel      string
	Action   string
	Baseline *snapshot.Entry // what to put back for Revert; what to save for Conflict
	Other    string          // for Conflict: human, unknown, or both
}

// Turn finds the latest real checkpoint and the one before it. The window
// to undo is everything written between them.
func Turn(storeDir string) (turn, baseline *snapshot.Checkpoint, err error) {
	all, err := snapshot.All(storeDir)
	if err != nil {
		return nil, nil, err
	}
	for i := len(all) - 1; i >= 0; i-- {
		if all[i].Source == PreUndo || all[i].Source == PreRestore {
			continue
		}
		if turn == nil {
			turn = all[i]
		} else {
			return turn, all[i], nil
		}
	}
	return turn, nil, nil
}

// Window returns every recorded write since the baseline checkpoint: the
// writes the latest turn is made of.
func Window(storeDir string, baseline *snapshot.Checkpoint) ([]writelog.Entry, error) {
	since := int64(0)
	if baseline != nil {
		since = baseline.TimeNS
	}
	writes, err := writelog.Read(filepath.Join(storeDir, snapshot.LogFile))
	if err != nil {
		return nil, err
	}
	var window []writelog.Entry
	for _, w := range writes {
		if w.TimeNS > since {
			window = append(window, w)
		}
	}
	return window, nil
}

// Plan decides, per path written since the baseline, what undo will do.
// only limits the plan to those relative paths.
func Plan(baseline *snapshot.Checkpoint, window []writelog.Entry, root string, only []string) []Step {
	type authors struct{ agent, human, unknown bool }
	byRel := map[string]*authors{}
	for _, w := range window {
		rel, ok := strings.CutPrefix(w.Path, root+"/")
		if !ok {
			continue
		}
		a := byRel[rel]
		if a == nil {
			a = &authors{}
			byRel[rel] = a
		}
		switch w.Writer {
		case lineage.Agent:
			a.agent = true
		case lineage.Human:
			a.human = true
		case lineage.Self:
		default:
			a.unknown = true
		}
	}
	wanted := map[string]bool{}
	for _, rel := range only {
		wanted[rel] = true
	}
	var steps []Step
	for rel, a := range byRel {
		if !a.agent || (len(wanted) > 0 && !wanted[rel]) {
			continue
		}
		step := Step{Rel: rel}
		var base *snapshot.Entry
		if baseline != nil {
			if e, ok := baseline.Entries[rel]; ok && e.Kind != snapshot.Dir {
				base = &e
			}
		}
		switch {
		case a.human && a.unknown:
			step.Action, step.Other, step.Baseline = Conflict, "both", base
		case a.human:
			step.Action, step.Other, step.Baseline = Conflict, "human", base
		case a.unknown:
			step.Action, step.Other, step.Baseline = Conflict, "unknown", base
		case base != nil:
			step.Action, step.Baseline = Revert, base
		default:
			step.Action = Remove
		}
		steps = append(steps, step)
	}
	sort.Slice(steps, func(i, j int) bool { return steps[i].Rel < steps[j].Rel })
	return steps
}

type Result struct {
	Reverted, Removed, Conflicts, Errors []string
}

// Apply runs the plan. Removes go deepest-first so agent-created trees
// unwind child before parent.
func Apply(steps []Step, objs *objects.Store, root string) Result {
	var res Result
	var removes []string
	for _, s := range steps {
		switch s.Action {
		case Conflict:
			res.Conflicts = append(res.Conflicts, s.Rel)
		case Revert:
			if err := snapshot.WriteEntry(objs, *s.Baseline, root, s.Rel); err != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", s.Rel, err))
			} else {
				res.Reverted = append(res.Reverted, s.Rel)
			}
		case Remove:
			removes = append(removes, s.Rel)
		}
	}
	sort.Slice(removes, func(i, j int) bool { return strings.Count(removes[i], "/") > strings.Count(removes[j], "/") })
	for _, rel := range removes {
		if err := os.Remove(filepath.Join(root, rel)); err != nil && !os.IsNotExist(err) {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", rel, err))
		} else {
			res.Removed = append(res.Removed, rel)
		}
	}
	return res
}

// SaveBoth writes each conflict's baseline version next to the live file as
// <file><suffix>, never touching the live file or an existing sibling.
func SaveBoth(steps []Step, objs *objects.Store, root, suffix string) (saved []string, errs []string) {
	for _, s := range steps {
		if s.Action != Conflict || s.Baseline == nil {
			continue
		}
		rel := s.Rel + suffix
		if _, err := os.Lstat(filepath.Join(root, rel)); err == nil {
			continue
		}
		if err := snapshot.WriteEntry(objs, *s.Baseline, root, rel); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", rel, err))
		} else {
			saved = append(saved, rel)
		}
	}
	return saved, errs
}

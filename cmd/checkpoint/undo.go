package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/manyapn/checkpoint-public/internal/daemon"
	"github.com/manyapn/checkpoint-public/internal/objstore"
	"github.com/manyapn/checkpoint-public/internal/oplog"
	"github.com/manyapn/checkpoint-public/internal/store"
	"github.com/manyapn/checkpoint-public/internal/undo"
	"github.com/manyapn/checkpoint-public/internal/versionlog"
)

func cmdUndo(args []string) error {
	fs := flag.NewFlagSet("undo", flag.ExitOnError)
	rootFlag := fs.String("root", "", "protected root (default: current directory)")
	storeFlag := fs.String("store", "", "store directory (default: derived from root path)")
	onlyFlag := fs.String("only", "", "comma-separated relative paths to limit the undo to")
	saveBoth := fs.Bool("save-both", false, "for each conflict, write the checkpoint version alongside the live file (<file>.checkpoint-<id>); the live file is never modified")
	fs.Parse(args)
	if fs.NArg() != 0 {
		return fmt.Errorf("undo: unexpected argument %q (this command takes only flags)", fs.Arg(0))
	}
	root := *rootFlag
	if root == "" {
		if wd, err := os.Getwd(); err == nil {
			root = wd
		}
	}
	root, err := resolveDir(root)
	if err != nil {
		return err
	}
	storeDir, err := resolveStore(*storeFlag, root)
	if err != nil {
		return err
	}
	if err := requireStoreFor(storeDir, root); err != nil {
		return err
	}
	oc, err := objstore.Open(storeDir)
	if err != nil {
		return err
	}
	// An interrupted undo is finished by REPLAYING its journal, never by
	// recomputing a plan: the interrupted run's own writes already shifted the
	// provenance window, so a recomputed plan would be empty and the "safe
	// rerun" would silently abandon the remaining actions.
	if op, interrupted := oplog.CheckInterrupted(storeDir); interrupted {
		if op != nil && op.Kind == "undo" {
			fmt.Printf("finishing interrupted undo: %s\n", oplog.Describe(op))
			res := undo.ReplayJournal(op.Actions, oc)
			fmt.Printf("replay: restored %d, removed %d\n", len(res.Restored), len(res.Deleted))
			for _, e := range res.Errors {
				fmt.Fprintf(os.Stderr, "  error: %s\n", e)
			}
			if len(res.Errors) > 0 {
				return fmt.Errorf("replay completed with %d error(s); the journal is kept, so rerun undo to retry", len(res.Errors))
			}
			if err := oplog.Done(storeDir); err != nil {
				return err
			}
			fmt.Println("interrupted undo completed; rerun undo if you want to undo further")
			return nil
		}
		fmt.Printf("note: %s\n", oplog.Describe(op))
	}
	// Undo targets the latest AGENT TURN, not merely the latest manifest: its
	// own bookkeeping cuts (`pre-undo`, and the `pre-restore` a restore makes)
	// are checkpoint's records of what it was about to do, never turns the user
	// performed. Counting them made selective undo ONE-SHOT: after
	// `undo --only a`, the pre-undo cut became `latest`, so `undo --only b`
	// found an empty window and silently did nothing.
	latest, ok, err := latestTurn(storeDir)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Println("nothing to undo (no checkpoints yet)")
		return nil
	}
	// Baseline = the checkpoint before the latest (the pre-turn state). nil if the
	// latest is the first checkpoint.
	var baseline *store.Manifest
	baseTime := int64(0)
	if latest.ID > 0 {
		// The pre-turn state is the newest DURABLE checkpoint BELOW the target
		// that is not bookkeeping: id-1 may be a pre-undo/pre-restore cut from
		// an earlier partial undo, whose tree already has some of the turn's
		// changes reverted.
		if b, ok := manifestBelow(storeDir, latest.ID); ok {
			baseline, baseTime = b, b.TimeNS
		}
	}
	// The provenance window: every captured write since the baseline.
	all, err := versionlog.Read(filepath.Join(storeDir, "versionlog"))
	if err != nil {
		return err
	}
	var window []versionlog.Version
	for _, v := range all {
		if v.TimeNS > baseTime {
			window = append(window, v)
		}
	}

	// Extra protected folders are recorded on the checkpoints themselves; the
	// undo covers every root the latest checkpoint covers.
	var extraRoots []string
	for r := range latest.Extra {
		extraRoots = append(extraRoots, r)
	}
	sort.Strings(extraRoots)

	only, _, err := parseOnly(*onlyFlag, flagWasSet(fs, "only"), root)
	if err != nil {
		return fmt.Errorf("undo: %w", err)
	}
	// Build every root's plan BEFORE mutating anything, so the whole operation
	// can be journaled first: an interrupted undo must be diagnosable and safely
	// retryable. --only is workspace-scoped, and that exclusion is stated in the
	// OUTPUT whenever it actually bites, not just in the help text.
	type rootPlan struct {
		root string
		plan *undo.Plan
	}
	plans := []rootPlan{{root, undo.BuildPlan(baseline, window, root, only)}}
	if len(only) > 0 && len(extraRoots) > 0 {
		fmt.Printf("note: --only is workspace-scoped; %d extra protected folder(s) excluded from this undo\n", len(extraRoots))
	}
	if len(only) == 0 {
		for _, r := range extraRoots {
			var exBase *store.Manifest
			if baseline != nil {
				if entries, ok := baseline.Extra[r]; ok {
					exBase = &store.Manifest{Entries: entries}
				}
			}
			plans = append(plans, rootPlan{r, undo.BuildPlan(exBase, window, r, nil)})
		}
	}

	// Conflict floor, FAIL-FAST form: unresolved conflicts abort BEFORE any
	// mutation. Failing after reverting (the earlier shape) advanced "latest"
	// via the pre-undo checkpoint, so the advertised "rerun with --save-both"
	// silently no-opped against the wrong turn. Nothing has been changed at
	// this point, so the rerun genuinely targets the same turn.
	var conflicts []string
	conflictWith := map[string]undo.Other{}
	for i, rp := range plans {
		for _, e := range rp.plan.Entries {
			if e.Action == undo.Conflict {
				name := e.Path
				if i == 0 {
					name = e.Rel
				}
				conflicts = append(conflicts, name)
				conflictWith[name] = e.Other
			}
		}
	}
	if len(conflicts) > 0 && !*saveBoth {
		for _, c := range conflicts {
			fmt.Printf("  needs review (%s): %s\n", reviewNote(conflictWith[c]), c)
		}
		return fmt.Errorf("%d file(s) need review; NOTHING was changed. Rerun with --save-both to revert the rest and keep each conflict's checkpoint version alongside", len(conflicts))
	}

	// The saved side of a conflict is what undo would have restored: the
	// PRE-TURN state, i.e. the baseline checkpoint's version. The sibling is
	// named after that id, so its content is exactly what
	// `restore --only <file> <id>` produces.
	suffix := ".checkpoint-version"
	if baseline != nil {
		suffix = fmt.Sprintf(".checkpoint-%d", baseline.ID)
	}
	// The journal carries every mutation (reverts, deletes, and save-both
	// siblings) WITH its payload, so an interrupted run replays from the
	// journal alone (see the replay block above).
	var acts []oplog.Action
	for _, rp := range plans {
		for _, e := range rp.plan.Entries {
			switch e.Action {
			case undo.Restore:
				acts = append(acts, oplog.Action{Do: "restore-file", Path: e.Path,
					Kind: e.Target.Kind, Ref: e.Target.Ref, Mode: e.Target.Mode, Link: e.Target.Link})
			case undo.Delete:
				acts = append(acts, oplog.Action{Do: "delete", Path: e.Path})
			case undo.Conflict:
				if *saveBoth && e.Target != nil {
					acts = append(acts, oplog.Action{Do: "save-both", Path: e.Path + suffix,
						Kind: e.Target.Kind, Ref: e.Target.Ref, Mode: e.Target.Mode, Link: e.Target.Link})
				}
			}
		}
	}

	// Auto-checkpoint the present before mutating, so this undo is itself
	// undoable, but ONLY when something will actually be mutated (an empty
	// undo must not advance "latest"). The daemon's autonomous cuts (setup /
	// baseline rescans) can race this id, so a taken id is retried, never
	// overwritten (store.Write refuses to replace).
	var present *store.Manifest
	if len(acts) > 0 {
		for attempt := 0; attempt < 3 && present == nil; attempt++ {
			preID, err := store.NextID(storeDir)
			if err != nil {
				return err
			}
			m, err := store.Snapshot(root, oc, latest, preID, time.Now().UnixNano(), store.DURABLE, 0, extraRoots...)
			if err != nil {
				return err
			}
			m.Source = sourcePreUndo
			switch err := store.Write(storeDir, m); {
			case err == nil:
				present = m
			case errors.Is(err, os.ErrExist):
				continue // the daemon cut this id meanwhile; take the next one
			default:
				return err
			}
		}
		if present == nil {
			return fmt.Errorf("could not cut the pre-undo checkpoint (id contention with the daemon); retry")
		}
		if err := oplog.Begin(storeDir, oplog.Op{Kind: "undo", Root: root, CheckpointID: latest.ID, Only: only, Actions: acts}); err != nil {
			return err
		}
	}

	res := undo.Apply(plans[0].plan, oc, plans[0].root)
	for _, rp := range plans[1:] {
		exRes := undo.Apply(rp.plan, oc, rp.root)
		res.Restored = append(res.Restored, prefixAll(rp.root, exRes.Restored)...)
		res.Deleted = append(res.Deleted, prefixAll(rp.root, exRes.Deleted)...)
		res.Conflicts = append(res.Conflicts, prefixAll(rp.root, exRes.Conflicts)...)
		// Extra roots report absolute paths, so the reason map must be rekeyed
		// to match what is printed; otherwise every extra-root conflict would
		// fall through to the unknown-writer wording.
		for rel, other := range exRes.ConflictWith {
			if res.ConflictWith == nil {
				res.ConflictWith = map[string]undo.Other{}
			}
			res.ConflictWith[filepath.Join(rp.root, rel)] = other
		}
		res.Errors = append(res.Errors, exRes.Errors...)
	}

	fmt.Printf("undo of checkpoint %d: reverted %d, removed %d, skipped %d for review\n",
		latest.ID, len(res.Restored), len(res.Deleted), len(res.Conflicts))
	for _, c := range res.Conflicts {
		fmt.Printf("  needs review (%s, left untouched): %s\n", reviewNote(res.ConflictWith[c]), c)
	}
	if *saveBoth && len(res.Conflicts) > 0 {
		for _, rp := range plans {
			saved, skippedNoBase, kept, errs := undo.MaterializeConflicts(rp.plan, oc, suffix)
			for _, s := range saved {
				fmt.Printf("  saved checkpoint version alongside: %s\n", s)
			}
			for _, s := range kept {
				fmt.Printf("  existing sibling left untouched (it may hold your merge): %s\n", s)
			}
			for _, s := range skippedNoBase {
				fmt.Printf("  no checkpoint version to save for %s (created this turn); live file kept\n", s)
			}
			res.Errors = append(res.Errors, errs...)
		}
	}
	// The journal clears only after EVERY mutation has succeeded (reverts,
	// deletes, and save-both siblings); the kept-journal message is then true.
	if len(res.Errors) == 0 && len(acts) > 0 {
		if err := oplog.Done(storeDir); err != nil {
			return err
		}
	}
	if present != nil {
		fmt.Printf("pre-undo checkpoint %d saved (restore it to undo this undo)\n", present.ID)
	} else if len(res.Conflicts) == 0 {
		fmt.Println("nothing to revert (no agent-only changes in the latest turn)")
		// On a filesystem without the dirent change feed (overlayfs, the
		// Docker default), deletions carry no provenance, so a file the agent
		// deleted is invisible to undo. Saying only "nothing to revert" there
		// reads as "nothing was deleted", which is the opposite of the truth.
		// Name the missing files and the command that actually gets them back.
	}
	// Deletions the filesystem cannot attribute are invisible to undo whether
	// or not it reverted anything else, so report them either way (comparing
	// against the PRE-TURN baseline: a file deleted during the turn is already
	// absent from the turn's own manifest).
	delRef := baseline
	if delRef == nil {
		delRef = latest
	}
	reportUnattributableDeletions(storeDir, root, delRef)
	for _, e := range res.Errors {
		fmt.Fprintf(os.Stderr, "  error: %s\n", e)
	}
	if len(res.Errors) > 0 {
		fmt.Fprintf(os.Stderr, "  the operation journal is kept (%s); rerunning undo replays the remaining actions\n",
			filepath.Join(storeDir, "operation.json"))
		return fmt.Errorf("undo completed with %d error(s)", len(res.Errors))
	}
	return nil
}

// plural picks a singular/plural suffix for counted nouns in user output.
// reviewNote phrases why a path needs review. An unknown writer must never be
// reported as the user: checkpoint refuses to touch the path either way, but
// it only claims the user changed something when it actually attributed the
// write to them.
func reviewNote(other undo.Other) string {
	switch other {
	case undo.OtherHuman:
		return "you also changed it"
	case undo.OtherBoth:
		return "you and an unidentified process also changed it"
	default:
		return "an unidentified process also changed it"
	}
}

// latestTurn returns the newest DURABLE checkpoint that represents real work,
// skipping checkpoint's own bookkeeping cuts. Baseline selection then walks
// down from it the same way, so a partially-undone turn stays addressable
// until every selected path has been handled.
func latestTurn(storeDir string) (*store.Manifest, bool, error) {
	ids, err := store.ValidIDs(storeDir)
	if err != nil {
		return nil, false, err
	}
	for i := len(ids) - 1; i >= 0; i-- {
		m, err := store.Load(storeDir, ids[i])
		if err != nil || m.Coverage != store.DURABLE {
			continue
		}
		if isBookkeeping(m.Source) {
			continue
		}
		return m, true, nil
	}
	return nil, false, nil
}

// manifestBelow returns the newest non-bookkeeping DURABLE checkpoint with an
// id below id, which is the pre-turn state for the turn at id.
func manifestBelow(storeDir string, id int) (*store.Manifest, bool) {
	ids, err := store.ValidIDs(storeDir)
	if err != nil {
		return nil, false
	}
	for i := len(ids) - 1; i >= 0; i-- {
		if ids[i] >= id {
			continue
		}
		m, err := store.Load(storeDir, ids[i])
		if err != nil || m.Coverage != store.DURABLE || isBookkeeping(m.Source) {
			continue
		}
		return m, true
	}
	return nil, false
}

// Bookkeeping checkpoint sources: manifests checkpoint cuts to record what IT
// was about to do, never turns someone performed. The values are persisted
// semantics (old manifests carry these literals), so they must not change; the
// constants exist so the writers and isBookkeeping share one definition — a
// typo in a writer would otherwise silently turn a bookkeeping cut into a
// "turn" that undo then targets.
const (
	sourcePreUndo    = "pre-undo"
	sourcePreRestore = "pre-restore"
)

// isBookkeeping reports whether a checkpoint records what checkpoint itself was
// about to do, rather than work someone did.
func isBookkeeping(source string) bool {
	return source == sourcePreUndo || source == sourcePreRestore
}

// reportUnattributableDeletions surfaces files that exist in the target
// checkpoint but are gone from disk, when this filesystem cannot attribute
// deletions. Undo legitimately cannot restore them (it never reverts what it
// cannot prove the agent did), but the user must not be left thinking nothing
// is missing: restore-by-path can bring them back.
func reportUnattributableDeletions(storeDir, root string, m *store.Manifest) {
	if st, err := daemon.RequestStatus(daemon.SocketPath(storeDir)); err == nil && st.FeedActive {
		return // deletions ARE attributed here; undo already handled them
	}
	var missing []string
	for rel, e := range m.Entries {
		if e.Kind == store.KindDir {
			continue
		}
		if _, err := os.Lstat(filepath.Join(root, rel)); os.IsNotExist(err) {
			missing = append(missing, rel)
		}
	}
	if len(missing) == 0 {
		return
	}
	sort.Strings(missing)
	shown := missing
	if len(shown) > 10 {
		shown = shown[:10]
	}
	fmt.Printf("\nnote: %d file(s) in checkpoint %d are missing from the workspace. This\n"+
		"filesystem cannot attribute deletions (no change feed), so undo cannot tell whether\n"+
		"the agent or you deleted them, and it never reverts what it cannot prove:\n", len(missing), m.ID)
	for _, rel := range shown {
		fmt.Printf("  - %q\n", rel)
	}
	if len(missing) > len(shown) {
		fmt.Printf("  … and %d more\n", len(missing)-len(shown))
	}
	fmt.Printf("  bring one back:  %s restore --only %q %d %s\n", prog(), shown[0], m.ID, root)
	fmt.Printf("  bring all back:  %s restore %d %s   (restores the whole checkpoint)\n", prog(), m.ID, root)
}

// prefixAll joins each rel path onto root, for reporting extra-root undo results
// unambiguously alongside workspace-relative ones.
func prefixAll(root string, rels []string) []string {
	out := make([]string, len(rels))
	for i, r := range rels {
		out[i] = filepath.Join(root, r)
	}
	return out
}

package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/manyapn/checkpoint-public/internal/daemon"
	"github.com/manyapn/checkpoint-public/internal/objects"
	"github.com/manyapn/checkpoint-public/internal/snapshot"
	"github.com/manyapn/checkpoint-public/internal/undo"
	"github.com/manyapn/checkpoint-public/internal/writelog"
)

func cmdUndo(args []string) error {
	fs, rootFlag, storeFlag := flags("undo")
	onlyFlag := fs.String("only", "", "comma-separated relative paths to limit the undo to")
	saveBoth := fs.Bool("save-both", false, "for each conflict, write the checkpoint version alongside the live file")
	fs.Parse(args)
	if err := noArgs(fs); err != nil {
		return err
	}
	t, err := resolve(*rootFlag, *storeFlag)
	if err != nil {
		return err
	}
	if err := snapshot.Stamp(t.storeDir, t.root); err != nil {
		return err
	}
	objs, err := objects.Open(t.storeDir)
	if err != nil {
		return err
	}
	turn, baseline, err := undo.Turn(t.storeDir)
	if err != nil {
		return err
	}
	if turn == nil {
		fmt.Println("nothing to undo (no checkpoints yet)")
		return nil
	}
	since := int64(0)
	if baseline != nil {
		since = baseline.TimeNS
	}
	writes, err := writelog.Read(filepath.Join(t.storeDir, snapshot.LogFile))
	if err != nil {
		return err
	}
	var window []writelog.Entry
	for _, w := range writes {
		if w.TimeNS > since {
			window = append(window, w)
		}
	}
	var only []string
	if *onlyFlag != "" {
		if only, err = parseOnly(*onlyFlag, t.root); err != nil {
			return err
		}
	}
	steps := undo.Plan(baseline, window, t.root, only)

	var conflicts []undo.Step
	for _, s := range steps {
		if s.Action == undo.Conflict {
			conflicts = append(conflicts, s)
		}
	}
	if len(conflicts) > 0 && !*saveBoth {
		for _, s := range conflicts {
			fmt.Printf("  needs review (%s): %s\n", reviewNote(s.Other), s.Rel)
		}
		return fmt.Errorf("%s need review; nothing was changed. Rerun with --save-both to revert the rest and keep each conflict's checkpoint version alongside", plural(len(conflicts), "file", "files"))
	}
	if len(steps) == 0 {
		fmt.Println("nothing to revert (no agent-only changes in the latest turn)")
		reportUnattributedDeletions(t, baseline, turn)
		return nil
	}

	present, err := snapshot.Scan(t.root, objs, turn)
	if err != nil {
		return err
	}
	present.Source = undo.PreUndo
	if err := snapshot.Commit(t.storeDir, present); err != nil {
		return err
	}
	res := undo.Apply(steps, objs, t.root)
	fmt.Printf("undo of checkpoint %d: reverted %d, removed %d, skipped %d for review\n",
		turn.ID, len(res.Reverted), len(res.Removed), len(res.Conflicts))
	for _, s := range conflicts {
		fmt.Printf("  needs review (%s, left untouched): %s\n", reviewNote(s.Other), s.Rel)
	}
	if *saveBoth {
		suffix := ".checkpoint-version"
		if baseline != nil {
			suffix = fmt.Sprintf(".checkpoint-%d", baseline.ID)
		}
		saved, errs := undo.SaveBoth(steps, objs, t.root, suffix)
		for _, rel := range saved {
			fmt.Printf("  saved checkpoint version alongside: %s\n", rel)
		}
		res.Errors = append(res.Errors, errs...)
	}
	fmt.Printf("pre-undo checkpoint %d saved (restore it to undo this undo)\n", present.ID)
	reportUnattributedDeletions(t, baseline, turn)
	for _, e := range res.Errors {
		fmt.Fprintf(os.Stderr, "  error: %s\n", e)
	}
	if len(res.Errors) > 0 {
		return fmt.Errorf("undo finished with %s", plural(len(res.Errors), "error", "errors"))
	}
	return nil
}

func reviewNote(other string) string {
	switch other {
	case "human":
		return "you also changed it"
	case "both":
		return "you and an unidentified process also changed it"
	}
	return "an unidentified process also changed it"
}

// Without the change feed a deletion has no recorded author, so undo cannot
// know whether to bring the file back. Say which files are missing and how
// to restore them, rather than implying nothing was deleted.
func reportUnattributedDeletions(t target, baseline, turn *snapshot.Checkpoint) {
	if st, err := daemon.GetStatus(t.sock); err == nil && st.FeedActive {
		return
	}
	ref := baseline
	if ref == nil {
		ref = turn
	}
	var missing []string
	for rel, e := range ref.Entries {
		if _, err := os.Lstat(filepath.Join(t.root, rel)); e.Kind != snapshot.Dir && os.IsNotExist(err) {
			missing = append(missing, rel)
		}
	}
	if len(missing) == 0 {
		return
	}
	fmt.Printf("\nnote: %s from checkpoint %d are missing. This filesystem does not report who deletes files, so undo cannot tell whether the agent or you did:\n", plural(len(missing), "file", "files"), ref.ID)
	for i, rel := range missing {
		if i == 10 {
			fmt.Printf("  ... and %d more\n", len(missing)-10)
			break
		}
		fmt.Printf("  %s\n", rel)
	}
	fmt.Printf("  bring them back with: checkpoint restore --only %s %d\n", missing[0], ref.ID)
}

package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/manyapn/checkpoint-public/internal/objstore"
	"github.com/manyapn/checkpoint-public/internal/oplog"
	"github.com/manyapn/checkpoint-public/internal/store"
)

func cmdRestore(args []string) error {
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	storeFlag := fs.String("store", "", "store directory (default: derived from target path)")
	includeExtra := fs.Bool("include-extra", false, "also restore extra protected folders IN PLACE (to their original absolute locations)")
	yesFlag := fs.Bool("yes", false, "skip the --include-extra confirmation (noninteractive)")
	onlyFlag := fs.String("only", "", "comma-separated workspace-relative paths: restore only these entries from the checkpoint")
	fs.Parse(args)
	if fs.NArg() != 2 {
		return fmt.Errorf("restore: expected <id> <target-dir>")
	}
	id, err := parseCheckpointID(fs.Arg(0))
	if err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	target, err := resolveDir(fs.Arg(1))
	if err != nil {
		return err
	}
	storeDir, err := resolveStore(*storeFlag, target)
	if err != nil {
		return err
	}
	m, err := store.Load(storeDir, id)
	if err != nil {
		if os.IsNotExist(err) && *storeFlag == "" {
			// The store was DERIVED from the target path: restoring into a new
			// directory therefore looked in a store that does not exist. This
			// is the single most confusing failure in the CLI, so name it.
			return fmt.Errorf("no checkpoint %d in %s.\n"+
				"  the store was derived from the target directory, and restoring into a NEW directory\n"+
				"  looks in a different (empty) store. Name the project's store explicitly:\n"+
				"    %s restore --store <the project's store> %d %s\n"+
				"  (%s status --root <project> shows its store path)", id, storeDir, prog(), id, target, prog())
		}
		return fmt.Errorf("restore: load checkpoint %d: %w", id, err)
	}
	// Single-file restore: --only keeps just the named workspace entries.
	// Workspace-scoped like undo's --only, so it cannot be combined with
	// --include-extra.
	only, onlyGiven, err := parseOnly(*onlyFlag, flagWasSet(fs, "only"), m.Root)
	if err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	if onlyGiven {
		if *includeExtra {
			return fmt.Errorf("restore: --only is workspace-scoped and cannot be combined with --include-extra")
		}
		filtered := map[string]store.Entry{}
		for _, rel := range only {
			e, ok := m.Entries[rel]
			if !ok {
				return fmt.Errorf("restore: %s is not in checkpoint %d", rel, id)
			}
			filtered[rel] = e
		}
		m = &store.Manifest{ID: m.ID, TimeNS: m.TimeNS, Root: m.Root, Coverage: m.Coverage, Entries: filtered}
	}
	oc, err := objstore.Open(storeDir)
	if err != nil {
		return err
	}
	if op, interrupted := oplog.CheckInterrupted(storeDir); interrupted {
		fmt.Printf("note: %s\n", oplog.Describe(op))
	}
	// The include-extra confirmation happens BEFORE any mutation (incl. the
	// pre-restore checkpoint): the user must see the actual outside-workspace
	// paths this will rewrite, not a count.
	if *includeExtra && len(m.Extra) > 0 {
		// LOAD the identity; never stamp it here. EnsureMeta on this path would
		// mint current-machine identity for exactly the carried/copied store this
		// check exists to refuse; only the daemon, running on the store's home
		// machine, stamps.
		meta, err := store.LoadMeta(storeDir)
		if os.IsNotExist(err) {
			return fmt.Errorf("extra-root restore refused: this store has no identity record (it predates identity stamping or was copied without it); run the daemon on the store's original machine to stamp it, or restore the extra folders manually")
		}
		if err != nil {
			return err
		}
		if err := store.CheckExtraRestoreIdentity(meta, m.Root); err != nil {
			return err
		}
		if err := confirmExtraPaths(m, *yesFlag); err != nil {
			return err
		}
	}
	// Restore auto-checkpoints the present FIRST, so overwriting an existing
	// tree is itself undoable. A missing/empty target has nothing to save. A
	// taken id (the daemon's autonomous cuts race this) is retried, never
	// overwritten.
	if ents, err := os.ReadDir(target); err == nil && len(ents) > 0 {
		var pre *store.Manifest
		for attempt := 0; attempt < 3 && pre == nil; attempt++ {
			preID, err := store.NextID(storeDir)
			if err != nil {
				return err
			}
			p, err := store.Snapshot(target, oc, m, preID, time.Now().UnixNano(), store.DURABLE, 0)
			if err != nil {
				return err
			}
			p.Source = "pre-restore"
			switch err := store.Write(storeDir, p); {
			case err == nil:
				pre = p
			case errors.Is(err, os.ErrExist):
				continue
			default:
				return err
			}
		}
		if pre == nil {
			return fmt.Errorf("could not cut the pre-restore checkpoint (id contention with the daemon); retry")
		}
		fmt.Printf("pre-restore checkpoint %d saved (restore it to undo this restore)\n", pre.ID)
	}
	// Journal the whole plan before the first mutation, so an interrupted
	// restore is diagnosable and safely retryable.
	var acts []oplog.Action
	for rel, e := range m.Entries {
		acts = append(acts, oplog.Action{Do: "restore-" + e.Kind, Path: filepath.Join(target, rel)})
	}
	if *includeExtra {
		for r, entries := range m.Extra {
			for rel, e := range entries {
				acts = append(acts, oplog.Action{Do: "restore-" + e.Kind, Path: filepath.Join(r, rel)})
			}
		}
	}
	sort.Slice(acts, func(i, j int) bool { return acts[i].Path < acts[j].Path })
	// The journal exists to make an INTERRUPTED mutation diagnosable. A failure
	// before the first mutation (bad target, unreadable object) must not leave
	// one behind, because the next unrelated restore would then report a phantom
	// incomplete operation.
	if err := oplog.Begin(storeDir, oplog.Op{Kind: "restore", Root: m.Root, CheckpointID: m.ID,
		Target: target, IncludeExtra: *includeExtra, Actions: acts}); err != nil {
		return err
	}
	mutated := false
	defer func() {
		if !mutated {
			oplog.Done(storeDir) // nothing was touched: no interruption to report
		}
	}()
	if fi, err := os.Lstat(target); err == nil && !fi.IsDir() {
		return fmt.Errorf("restore: target %s exists and is not a directory", target)
	}
	mutated = true

	// A WHOLE-checkpoint restore reproduces the tree exactly: paths the
	// checkpoint does not contain are removed (recoverable via the pre-restore
	// checkpoint cut above). A --only restore materializes just the named
	// entries and removes nothing: there, "absent from the manifest" says
	// nothing about what the user wants kept.
	var removed, keptExceptions []string
	if !onlyGiven {
		removed, keptExceptions, err = store.RestoreExact(m, oc, target)
	} else {
		err = store.Restore(m, oc, target)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "the operation journal is kept (%s); rerunning restore is safe\n",
			filepath.Join(storeDir, "operation.json"))
		return err
	}
	fmt.Printf("restored checkpoint %d (%d entries) to %s [coverage: %s]\n", m.ID, len(m.Entries), target, m.Coverage)
	if len(removed) > 0 {
		shown := removed
		if len(shown) > 10 {
			shown = shown[:10]
		}
		fmt.Printf("removed %d path(s) the checkpoint does not contain:\n", len(removed))
		for _, rel := range shown {
			fmt.Printf("  - %q\n", rel) // %q: a newline in a filename must not fake a log line
		}
		if len(removed) > len(shown) {
			fmt.Printf("  … and %d more\n", len(removed)-len(shown))
		}
		fmt.Println("  (default-skipped folders such as node_modules/.git were left untouched;")
		fmt.Println("   restore the pre-restore checkpoint above to get these back)")
	}
	// Named exceptions are things this checkpoint COULD NOT capture (a fifo, an
	// unreadable file). They are absent from the manifest for that reason, not
	// because the user does not want them, and no checkpoint can put them back,
	// so removing them would be irreversible. They are left alone, said plainly,
	// and never counted as "restored".
	if len(keptExceptions) > 0 {
		fmt.Printf("left %d path(s) this checkpoint could not capture (unrestorable, so never removed):\n", len(keptExceptions))
		for _, rel := range keptExceptions {
			fmt.Printf("  ~ %q\n", rel)
		}
	}
	if len(m.Extra) > 0 {
		if *includeExtra {
			if err := store.RestoreExtras(m, oc); err != nil {
				fmt.Fprintf(os.Stderr, "the operation journal is kept (%s); rerunning restore is safe\n",
					filepath.Join(storeDir, "operation.json"))
				return err
			}
			for r, entries := range m.Extra {
				fmt.Printf("restored extra protected folder %s (%d entries) in place\n", r, len(entries))
			}
		} else {
			fmt.Printf("note: checkpoint also covers %d extra protected folder(s); rerun with --include-extra to restore them in place\n", len(m.Extra))
		}
	}
	return oplog.Done(storeDir)
}

// confirmExtraPaths shows the ACTUAL outside-workspace paths an
// --include-extra restore will rewrite (first 10 + count) and requires an
// explicit yes. Noninteractive runs must pass --yes; a non-terminal stdin
// without it is a refusal, never a silent proceed.
func confirmExtraPaths(m *store.Manifest, yes bool) error {
	var paths []string
	for r, entries := range m.Extra {
		for rel := range entries {
			paths = append(paths, filepath.Join(r, rel))
		}
	}
	sort.Strings(paths)
	fmt.Printf("--include-extra will rewrite these paths OUTSIDE the workspace, in place:\n")
	n := len(paths)
	show := n
	if show > 10 {
		show = 10
	}
	for _, p := range paths[:show] {
		fmt.Printf("  %s\n", p)
	}
	if n > show {
		fmt.Printf("  … and %d more (%d total)\n", n-show, n)
	}
	if yes {
		return nil
	}
	if fi, err := os.Stdin.Stat(); err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return fmt.Errorf("refusing to rewrite %d outside-workspace path(s) without confirmation: rerun with --yes", n)
	}
	fmt.Printf("rewrite these in place? [y/N] ")
	var answer string
	if _, err := fmt.Fscanln(os.Stdin, &answer); err != nil {
		return fmt.Errorf("no confirmation received: rerun with --yes for noninteractive use")
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return nil
	}
	return fmt.Errorf("aborted: extra folders not restored")
}

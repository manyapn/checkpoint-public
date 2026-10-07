package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/manyapn/checkpoint-public/internal/objects"
	"github.com/manyapn/checkpoint-public/internal/snapshot"
	"github.com/manyapn/checkpoint-public/internal/undo"
	"github.com/manyapn/checkpoint-public/internal/writelog"
)

// restore rebuilds a checkpoint into the protected folder (or another
// directory). A whole restore makes the target exactly match; --only
// writes just the named entries and removes nothing.
func cmdRestore(args []string) error {
	fs, rootFlag, storeFlag := flags("restore")
	onlyFlag := fs.String("only", "", "comma-separated relative paths: restore just these")
	fs.Parse(args)
	if fs.NArg() < 1 || fs.NArg() > 2 {
		return fmt.Errorf("expected: restore [--only a,b] ID [DIR]")
	}
	id, err := parseID(fs.Arg(0))
	if err != nil {
		return err
	}
	t, err := resolve(*rootFlag, *storeFlag)
	if err != nil {
		return err
	}
	target := t.root
	if fs.NArg() == 2 {
		if target, err = realDir(fs.Arg(1)); err != nil {
			return err
		}
	}
	c, err := snapshot.Load(t.storeDir, id)
	if err != nil {
		return fmt.Errorf("checkpoint %d: %w (store %s)", id, err, t.storeDir)
	}
	objs, err := objects.Open(t.storeDir)
	if err != nil {
		return err
	}
	exact := *onlyFlag == ""
	if !exact {
		only, err := parseOnly(*onlyFlag, t.root)
		if err != nil {
			return err
		}
		kept := map[string]snapshot.Entry{}
		for _, rel := range only {
			e, ok := c.Entries[rel]
			if !ok {
				return fmt.Errorf("%s is not in checkpoint %d", rel, id)
			}
			kept[rel] = e
		}
		c.Entries = kept
	}
	if ents, _ := os.ReadDir(target); len(ents) > 0 {
		present, err := snapshot.Scan(target, objs, c)
		if err != nil {
			return err
		}
		present.Source = undo.PreRestore
		if err := snapshot.Commit(t.storeDir, present); err != nil {
			return err
		}
		fmt.Printf("pre-restore checkpoint %d saved (restore it to undo this restore)\n", present.ID)
	}
	removed, err := snapshot.Restore(c, objs, target, exact)
	if err != nil {
		return err
	}
	fmt.Printf("restored checkpoint %d (%s) to %s\n", id, plural(len(c.Entries), "entry", "entries"), target)
	if len(removed) > 0 {
		fmt.Printf("removed %s the checkpoint does not contain (node_modules, .git and similar are left alone)\n", plural(len(removed), "path", "paths"))
	}
	return nil
}

// recover lists or extracts files that were captured but never made it into
// a checkpoint: created and deleted inside one turn.
func cmdRecover(args []string) error {
	fs, rootFlag, storeFlag := flags("recover")
	to := fs.String("to", "", "extract into this directory (default: list only)")
	fs.Parse(args)
	if err := noArgs(fs); err != nil {
		return err
	}
	t, err := resolve(*rootFlag, *storeFlag)
	if err != nil {
		return err
	}
	writes, err := writelog.Read(filepath.Join(t.storeDir, snapshot.LogFile))
	if err != nil {
		return err
	}
	objs, err := objects.Open(t.storeDir)
	if err != nil {
		return err
	}
	all, err := snapshot.All(t.storeDir)
	if err != nil {
		return err
	}
	found := snapshot.Salvage(writes, all, objs)
	var paths []string
	for p := range found {
		if _, err := os.Lstat(p); err != nil { // still on disk means nothing to recover
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		fmt.Println("no recoverable files (everything captured is in a checkpoint or still on disk)")
		return nil
	}
	if *to == "" {
		for _, p := range paths {
			fmt.Println(p)
		}
		return nil
	}
	dir, err := realDir(*to)
	if err != nil {
		return err
	}
	for _, p := range paths {
		rel, _ := filepath.Rel(t.root, p)
		dst := filepath.Join(dir, rel)
		if _, err := os.Lstat(dst); err == nil {
			fmt.Fprintf(os.Stderr, "  skipped %s: %s already exists\n", p, dst)
			continue
		}
		content, err := objs.Get(found[p])
		if err != nil {
			return err
		}
		os.MkdirAll(filepath.Dir(dst), 0o755)
		if err := os.WriteFile(dst, content, 0o644); err != nil {
			return err
		}
		fmt.Printf("recovered %s -> %s\n", p, dst)
	}
	return nil
}

package main

import (
	"fmt"
	"os"

	"github.com/manyapn/checkpoint-public/internal/objects"
	"github.com/manyapn/checkpoint-public/internal/snapshot"
	"github.com/manyapn/checkpoint-public/internal/undo"
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

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/manyapn/checkpoint-public/internal/objects"
	"github.com/manyapn/checkpoint-public/internal/snapshot"
	"github.com/manyapn/checkpoint-public/internal/writelog"
)

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

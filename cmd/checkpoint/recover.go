package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/manyapn/checkpoint-public/internal/objstore"
	"github.com/manyapn/checkpoint-public/internal/store"
	"github.com/manyapn/checkpoint-public/internal/versionlog"
)

func cmdRecover(args []string) error {
	fs := flag.NewFlagSet("recover", flag.ExitOnError)
	storeFlag := fs.String("store", "", "store directory (default: derived from workspace path)")
	toFlag := fs.String("to", "", "extract recoverable files under this directory (default: list only)")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("recover: expected <workspace>")
	}
	ws, err := resolveDir(fs.Arg(0))
	if err != nil {
		return err
	}
	storeDir, err := resolveStore(*storeFlag, ws)
	if err != nil {
		return err
	}
	if err := requireStoreFor(storeDir, ws); err != nil {
		return err
	}
	versions, err := versionlog.Read(filepath.Join(storeDir, "versionlog"))
	if err != nil {
		return err
	}
	oc, err := objstore.Open(storeDir)
	if err != nil {
		return err
	}
	// A path ANY checkpoint holds is restorable by id, not a "recovered file".
	ids, err := store.IDs(storeDir)
	if err != nil {
		return err
	}
	var ms []*store.Manifest
	for _, id := range ids {
		if m, err := store.Load(storeDir, id); err == nil {
			ms = append(ms, m)
		}
	}
	salv, evicted := store.Salvage(versions, ms, oc)
	// A path currently on disk is not lost, so there is nothing to recover.
	for p := range salv {
		if _, err := os.Lstat(p); err == nil {
			delete(salv, p)
		}
	}
	if evicted > 0 {
		fmt.Printf("note: %d older recoverable file(s) beyond the %d-entry cap are NOT listed here\n", evicted, store.MaxSalvageEntries)
	}
	if len(salv) == 0 {
		fmt.Println("no recoverable files (nothing captured that a checkpoint doesn't already hold)")
		return nil
	}
	paths := make([]string, 0, len(salv))
	for p := range salv {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	toDir := ""
	if *toFlag != "" {
		if toDir, err = resolveDir(*toFlag); err != nil {
			return err
		}
	}
	skipped := 0
	for _, p := range paths {
		if toDir == "" {
			fmt.Println(p)
			continue
		}
		rel, err := filepath.Rel(ws, p)
		if err != nil || rel == "" || rel[0] == '.' {
			rel = filepath.Base(p) // outside ws or odd: fall back to base name
		}
		// Recovery writes into a directory the user named; it must stay inside
		// it and must never write THROUGH something already sitting at the
		// destination. A symlink there would send salvaged bytes outside the
		// target, and an ordinary file there is live data that recovery must
		// never overwrite. Both are skipped and reported: recovery is an escape
		// hatch, never a silent overwrite.
		dst, err := store.SafeJoinUnder(toDir, rel)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  skipped %s: %v\n", p, err)
			skipped++
			continue
		}
		content, err := oc.Get(salv[p])
		if err != nil {
			return fmt.Errorf("recover %s: %w", p, err)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := store.WriteFileNoFollow(dst, content, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "  skipped %s: %v\n", p, err)
			skipped++
			continue
		}
		fmt.Printf("recovered %s -> %s\n", p, dst)
	}
	if skipped > 0 {
		fmt.Fprintf(os.Stderr, "%d file(s) not recovered because something already occupies the "+
			"destination (recovery never overwrites or follows an existing entry); "+
			"remove or rename it, or use a fresh --to directory\n", skipped)
		return fmt.Errorf("recover: %d file(s) skipped", skipped)
	}
	return nil
}

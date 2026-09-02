package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/manyapn/checkpoint-public/internal/objstore"
	"github.com/manyapn/checkpoint-public/internal/store"
)

func cmdCreate(args []string) error {
	fs := flag.NewFlagSet("create", flag.ExitOnError)
	storeFlag := fs.String("store", "", "store directory (default: derived from project path)")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("create: expected <project-dir>")
	}
	project, err := resolveDir(fs.Arg(0))
	if err != nil {
		return err
	}
	if fi, err := os.Stat(project); err != nil || !fi.IsDir() {
		return fmt.Errorf("create: %s is not a directory", project)
	}
	storeDir, err := resolveStore(*storeFlag, project)
	if err != nil {
		return err
	}
	if err := requireStoreFor(storeDir, project); err != nil {
		return err
	}
	oc, err := objstore.Open(storeDir)
	if err != nil {
		return err
	}
	prev, _, err := store.Latest(storeDir)
	if err != nil {
		return err
	}
	id, err := store.NextID(storeDir)
	if err != nil {
		return err
	}
	// Concurrent creates race for the next id; store.Write refuses to replace an
	// existing manifest (that refusal is what stops one writer destroying
	// another's checkpoint), so take the next free id and retry rather than
	// failing the user's create.
	var m *store.Manifest
	for attempt := 0; attempt < 8; attempt++ {
		cand, err := store.Snapshot(project, oc, prev, id, time.Now().UnixNano(), store.DURABLE, 0)
		if err != nil {
			return err
		}
		werr := store.Write(storeDir, cand)
		if werr == nil {
			m = cand
			break
		}
		if !errors.Is(werr, os.ErrExist) {
			return werr
		}
		if id, err = store.NextID(storeDir); err != nil {
			return err
		}
	}
	if m == nil {
		return fmt.Errorf("create: could not claim a checkpoint id after 8 attempts (heavy concurrent writes); retry")
	}
	fmt.Printf("created checkpoint %d (%d entries) [store: %s]\n", m.ID, len(m.Entries), storeDir)
	return nil
}

package main

import (
	"fmt"
	"time"

	"github.com/manyapn/checkpoint-public/internal/daemon"
	"github.com/manyapn/checkpoint-public/internal/objects"
	"github.com/manyapn/checkpoint-public/internal/snapshot"
)

func cmdPrune(args []string) error {
	fs, rootFlag, storeFlag := flags("prune")
	keepDays := fs.Int("keep-days", 7, "keep unnamed checkpoints newer than this")
	dryRun := fs.Bool("dry-run", false, "report what would go; delete nothing")
	yes := fs.Bool("yes", false, "skip the confirmation")
	fs.Parse(args)
	if err := noArgs(fs); err != nil {
		return err
	}
	if *keepDays < 0 {
		return fmt.Errorf("--keep-days must be >= 0")
	}
	t, err := resolve(*rootFlag, *storeFlag)
	if err != nil {
		return err
	}
	if daemon.Running(t.sock) {
		return fmt.Errorf("stop protection first (checkpoint protect --stop); prune needs the store to itself")
	}
	objs, err := objects.Open(t.storeDir)
	if err != nil {
		return err
	}
	now := time.Now()
	plan, err := snapshot.Prune(t.storeDir, objs, *keepDays, now, true)
	if err != nil {
		return err
	}
	if len(plan.RemovedCheckpoints) == 0 && plan.RemovedObjects == 0 {
		fmt.Println("nothing to prune")
		return nil
	}
	fmt.Printf("would remove %s older than %d days, %s (%s), and %d expired write records\n",
		plural(len(plan.RemovedCheckpoints), "checkpoint", "checkpoints"), *keepDays,
		plural(plan.RemovedObjects, "unreferenced object", "unreferenced objects"), humanBytes(plan.RemovedBytes), plan.ExpiredWrites)
	if *dryRun {
		return nil
	}
	if err := confirm("delete these?", *yes); err != nil {
		return err
	}
	rep, err := snapshot.Prune(t.storeDir, objs, *keepDays, now, false)
	if err != nil {
		return err
	}
	fmt.Printf("removed %s and %s (%s reclaimed)\n", plural(len(rep.RemovedCheckpoints), "checkpoint", "checkpoints"),
		plural(rep.RemovedObjects, "object", "objects"), humanBytes(rep.RemovedBytes))
	return nil
}

package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/manyapn/checkpoint-public/internal/daemon"
)

func cmdSave(args []string) error {
	fs := flag.NewFlagSet("save", flag.ExitOnError)
	rootFlag := fs.String("root", "", "protected root the daemon watches (default: current directory)")
	storeFlag := fs.String("store", "", "store directory (default: derived from root path)")
	sourceFlag := fs.String("source", "manual", "label recorded on the checkpoint (e.g. the agent turn)")
	nameFlag := fs.String("name", "", "name this checkpoint; named checkpoints survive pruning and always cut, even with no changes")
	fs.Parse(args)
	if fs.NArg() != 0 {
		return fmt.Errorf("save: unexpected argument %q (this command takes only flags)", fs.Arg(0))
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
	sock := daemon.SocketPath(storeDir)
	resp, err := daemon.RequestNamedCheckpoint(sock, *sourceFlag, *nameFlag)
	if err != nil {
		// Turn hooks call save, so a raw "connect: no such file or directory"
		// is the message a stranger hits most often. Carry the remedy, the way
		// run's refusal does.
		if _, serr := daemon.RequestStatus(sock); serr != nil {
			return fmt.Errorf("no daemon is protecting %s (store %s), so there is nothing to checkpoint into.\n"+
				"  start protection:  %s protect --store %s %s", root, storeDir, prog(), storeDir, root)
		}
		return err
	}
	if resp.SkippedEmpty {
		fmt.Printf("no changes since checkpoint %d, so no new checkpoint was created\n", resp.ID)
		return nil
	}
	tag := ""
	if resp.SettleTimedOut {
		tag = " (settle timed out; trailing writes may be mid-operation)"
	}
	name := ""
	if *nameFlag != "" {
		name = fmt.Sprintf(" %q", *nameFlag)
	}
	fmt.Printf("checkpoint %d%s %s (%d entries)%s\n", resp.ID, name, resp.Coverage, resp.Entries, tag)
	return nil
}

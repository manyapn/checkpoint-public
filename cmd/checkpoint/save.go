package main

import (
	"fmt"

	"github.com/manyapn/checkpoint-public/internal/daemon"
)

func cmdSave(args []string) error {
	fs, rootFlag, storeFlag := flags("save")
	source := fs.String("source", "manual", "label for where this checkpoint came from")
	name := fs.String("name", "", "name it; named checkpoints never expire and always cut")
	fs.Parse(args)
	if err := noArgs(fs); err != nil {
		return err
	}
	t, err := resolve(*rootFlag, *storeFlag)
	if err != nil {
		return err
	}
	res, err := daemon.Checkpoint(t.sock, *source, *name)
	if err != nil && !daemon.Running(t.sock) {
		return fmt.Errorf("%s is not protected; start with: checkpoint protect %s", t.root, t.root)
	}
	report(res, err)
	return err
}

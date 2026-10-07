package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// ui hands over to checkpoint-ui, a separate binary so the terminal library
// it links never slows down the plain commands.
func cmdUI(args []string) error {
	fs, rootFlag, storeFlag := flags("ui")
	fs.Parse(args)
	t, err := resolve(*rootFlag, *storeFlag)
	if err != nil {
		return err
	}
	self, _ := os.Executable()
	ui := filepath.Join(filepath.Dir(self), "checkpoint-ui")
	if _, err := os.Stat(ui); err != nil {
		if ui, err = exec.LookPath("checkpoint-ui"); err != nil {
			return fmt.Errorf("checkpoint-ui is not installed next to %s; `make build` builds both", self)
		}
	}
	cmd := exec.Command(ui, "--cli", self, "--root", t.root, "--store", t.storeDir)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

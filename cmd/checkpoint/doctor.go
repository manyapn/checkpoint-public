package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"

	"github.com/manyapn/checkpoint-public/internal/doctor"
)

func cmdDoctor(args []string) error {
	fs, rootFlag, storeFlag := flags("doctor")
	fs.Parse(args)
	if err := noArgs(fs); err != nil {
		return err
	}
	// resolve() refuses a bad store location; doctor reports it instead
	root := *rootFlag
	if root == "" {
		root, _ = os.Getwd()
	}
	root, _ = realDir(root)
	storeDir := *storeFlag
	if storeDir == "" {
		storeDir = defaultStore(root)
	}
	storeDir, _ = filepath.Abs(storeDir)
	rep := doctor.Run(root, storeDir)
	fmt.Print(rep.Text())
	if !rep.Healthy() {
		return fmt.Errorf("this machine cannot run checkpoint as configured")
	}
	return nil
}

func printVersion() {
	rev, dirty := "unknown", ""
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" {
				rev = s.Value
			}
			if s.Key == "vcs.modified" && s.Value == "true" {
				dirty = " (modified working tree)"
			}
		}
	}
	fmt.Printf("checkpoint %s%s, %s\n", rev, dirty, runtime.Version())
}

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

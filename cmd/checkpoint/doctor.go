package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/manyapn/checkpoint-public/internal/doctor"
)

// cmdDoctor answers the question a stranger machine actually poses: will this
// work here, and if not, what do I do about it? Every failing check carries a
// remedy, and the exit status is usable from a setup script.
func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	rootFlag := fs.String("root", "", "project to check (default: current directory)")
	storeFlag := fs.String("store", "", "store directory (default: derived from root path)")
	fs.Parse(args)
	if fs.NArg() != 0 {
		return fmt.Errorf("doctor: unexpected argument %q (this command takes only flags)", fs.Arg(0))
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
	// Resolve the store WITHOUT letting the in-tree guard abort: a bad store
	// location is one of the things doctor exists to REPORT, not to die on.
	storeDir := *storeFlag
	if storeDir == "" {
		if d, derr := resolveStore("", root); derr == nil {
			storeDir = d
		} else {
			storeDir = filepath.Join(root, ".checkpoint-store")
		}
	} else if abs, aerr := filepath.Abs(storeDir); aerr == nil {
		storeDir = abs
	}
	rep := doctor.Run(root, storeDir)
	fmt.Print(rep.Text())
	if !rep.Healthy() {
		return fmt.Errorf("this machine cannot run checkpoint as configured (see the remedies above)")
	}
	return nil
}

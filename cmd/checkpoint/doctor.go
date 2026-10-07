package main

import (
	"fmt"
	"os"
	"path/filepath"

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

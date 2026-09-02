package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/manyapn/checkpoint-public/internal/daemon"
	"github.com/manyapn/checkpoint-public/internal/provenance"
)

func cmdRegisterAgent(args []string, register bool) error {
	name := "register-agent"
	if !register {
		name = "unregister-agent"
	}
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	rootFlag := fs.String("root", "", "protected root (default: current directory)")
	storeFlag := fs.String("store", "", "store directory (default: derived from root path)")
	pidFlag := fs.Int("pid", 0, "the agent process pid to (un)register as an agent root")
	fs.Parse(args)
	if fs.NArg() != 0 {
		return fmt.Errorf("%s: unexpected argument %q (this command takes only flags)", name, fs.Arg(0))
	}
	if *pidFlag <= 0 {
		return fmt.Errorf("%s: --pid is required", name)
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
	// A registration whose stable identity cannot be read is worthless and
	// actively harmful: it inflates agent_sessions, keeps the provenance
	// scanner spinning, and (with a start-time of 0) would credit whatever
	// process later inherits that pid. Refuse it.
	start, ok := provenance.StartTime(*pidFlag)
	if !ok {
		return fmt.Errorf("%s: no live process with pid %d (its identity cannot be read, so "+
			"writes could never be attributed to it)", name, *pidFlag)
	}
	sock := daemon.SocketPath(storeDir)
	if register {
		if err := daemon.RegisterAgentRoot(sock, *pidFlag, start); err != nil {
			return err
		}
		fmt.Printf("registered agent root pid %d\n", *pidFlag)
	} else {
		if err := daemon.UnregisterAgentRoot(sock, *pidFlag, start); err != nil {
			return err
		}
		fmt.Printf("unregistered agent root pid %d\n", *pidFlag)
	}
	return nil
}

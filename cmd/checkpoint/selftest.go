package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/manyapn/checkpoint-public/internal/selftest"
)

// selftest proves the guarantees on this machine by driving this very
// binary against a scratch project. The scratch dir decides which
// filesystem gets tested, so it defaults to the current directory.
func cmdSelftest(args []string) error {
	fs := flag.NewFlagSet("selftest", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "machine-readable report")
	verbose := fs.Bool("verbose", false, "narrate every step and command")
	keep := fs.Bool("keep", false, "keep the scratch directory")
	work := fs.String("work", "", "scratch directory (default: under the current directory)")
	fs.Parse(args)
	self, _ := os.Executable()
	if *work == "" {
		*work, _ = os.Getwd()
	}
	scratch, err := os.MkdirTemp(*work, ".checkpoint-selftest-")
	if err != nil {
		return err
	}
	if !*keep {
		defer os.RemoveAll(scratch)
	}
	if len(filepath.Join(scratch, "store", "daemon.sock"))+1 > 108 {
		return fmt.Errorf("%s is too deep for a Unix socket path; pass --work with a shorter path", scratch)
	}
	rep := selftest.Run(self, scratch, *verbose)
	if *asJSON {
		json.NewEncoder(os.Stdout).Encode(rep)
	} else {
		fmt.Print("\n" + rep.Text())
	}
	if *keep {
		fmt.Println("scratch kept at", scratch)
	}
	if !rep.OK() {
		return fmt.Errorf("selftest: a guarantee failed on this machine")
	}
	return nil
}

package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// cmdUI runs the TUI, a pure client of this binary's own --json output. The
// TUI can render nothing the JSON contract doesn't carry, which keeps the
// contract honest: anything the UI needs, a script can get too.
func cmdUI(args []string) error {
	fs := flag.NewFlagSet("ui", flag.ExitOnError)
	rootFlag := fs.String("root", "", "protected root (default: current directory)")
	storeFlag := fs.String("store", "", "store directory (default: derived from root path)")
	fs.Parse(args)
	if fs.NArg() != 0 {
		return fmt.Errorf("ui: unexpected argument %q (this command takes only flags)", fs.Arg(0))
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
	self, err := os.Executable()
	if err != nil {
		return err
	}

	// The interactive screen lives in its own binary, and this command hands
	// over to it. Bubble Tea's package init asks the terminal for its
	// background colour and waits out a five second timeout when nothing
	// answers, so linking it here would put that delay in front of every
	// command, doctor included. Terminals that do not answer are ordinary:
	// CI logs, script(1), some editor terminals, tmux without passthrough.
	uiBin, err := uiBinary(self)
	if err != nil {
		return err
	}
	uiArgs := []string{"--root", root, "--cli", self}
	if *storeFlag != "" {
		uiArgs = append(uiArgs, "--store", *storeFlag)
	}
	cmd := exec.Command(uiBin, uiArgs...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// uiBinary locates checkpoint-ui: beside this binary first, so a pair built or
// installed together stay a pair, then $PATH. The error says how to get it
// rather than only that it is missing.
func uiBinary(self string) (string, error) {
	sibling := filepath.Join(filepath.Dir(self), "checkpoint-ui")
	if st, err := os.Stat(sibling); err == nil && !st.IsDir() {
		return sibling, nil
	}
	if found, err := exec.LookPath("checkpoint-ui"); err == nil {
		return found, nil
	}
	return "", fmt.Errorf("checkpoint-ui is not installed (looked beside %s and on $PATH).\n"+
		"It ships alongside this binary: `make build && sudo make install`, or `go build -o checkpoint-ui ./cmd/checkpoint-ui`.\n"+
		"Everything the screen shows is available without it: `checkpoint status`, `checkpoint history`", self)
}

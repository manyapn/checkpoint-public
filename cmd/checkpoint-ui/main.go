// Command checkpoint-ui is the interactive screen over `checkpoint`.
//
// It is a separate binary for one reason, and the reason is measurable. Bubble
// Tea's package init calls lipgloss.HasDarkBackground(), which asks the
// terminal for its background colour and waits up to termenv's five second
// timeout when nothing answers. Any binary that links Bubble Tea pays that on
// EVERY run, including `checkpoint doctor`, which is supposed to be the fast
// answer to "will this work here". Terminals that do not answer the query are
// not exotic: CI logs, script(1), some editor terminals and tmux without
// passthrough all fall into it, and that is exactly where the recording of the
// demo first exposed it.
//
// Splitting the binary keeps the cost where it belongs. The UI is already a
// pure client of the CLI's --json output, so it loses nothing by living in its
// own process, and `checkpoint ui` still launches it.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/manyapn/checkpoint-public/internal/tui"
)

func main() {
	fs := flag.NewFlagSet("checkpoint-ui", flag.ExitOnError)
	rootFlag := fs.String("root", "", "protected root (default: current directory)")
	storeFlag := fs.String("store", "", "store directory (default: derived from root path)")
	cliFlag := fs.String("cli", "", "path to the checkpoint binary (default: found next to this one, then $PATH)")
	fs.Parse(os.Args[1:])
	if fs.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "checkpoint-ui: unexpected argument %q (this command takes only flags)\n", fs.Arg(0))
		os.Exit(2)
	}

	root := *rootFlag
	if root == "" {
		wd, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(os.Stderr, "checkpoint-ui: %v\n", err)
			os.Exit(1)
		}
		root = wd
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "checkpoint-ui: %v\n", err)
		os.Exit(1)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}

	cli, err := checkpointBinary(*cliFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "checkpoint-ui: %v\n", err)
		os.Exit(1)
	}
	if err := tui.Run(tui.NewCLIClient(cli, abs, *storeFlag)); err != nil {
		fmt.Fprintf(os.Stderr, "checkpoint-ui: %v\n", err)
		os.Exit(1)
	}
}

// checkpointBinary finds the CLI this UI reads from. Next to this binary first,
// so a pair built or installed together stay a pair, then $PATH.
func checkpointBinary(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	if self, err := os.Executable(); err == nil {
		sibling := filepath.Join(filepath.Dir(self), "checkpoint")
		if st, err := os.Stat(sibling); err == nil && !st.IsDir() {
			return sibling, nil
		}
	}
	found, err := exec.LookPath("checkpoint")
	if err != nil {
		return "", fmt.Errorf("cannot find the checkpoint binary: not next to this one and not on $PATH; pass --cli PATH")
	}
	return found, nil
}

// Command checkpoint is version control for long-running agent sessions: it
// records every write with its author and lets you revert the agent's work
// without touching your own.
package main

import (
	"fmt"
	"os"
)

var commands = map[string]func([]string) error{
	"doctor":           cmdDoctor,
	"protect":          cmdProtect,
	"daemon":           cmdDaemon,
	"run":              cmdRun,
	"save":             cmdSave,
	"undo":             cmdUndo,
	"history":          cmdHistory,
	"status":           cmdStatus,
	"restore":          cmdRestore,
	"recover":          cmdRecover,
	"prune":            cmdPrune,
	"register-agent":   func(a []string) error { return cmdRegisterAgent(a, true) },
	"unregister-agent": func(a []string) error { return cmdRegisterAgent(a, false) },
	"ui":               cmdUI,
	"selftest":         cmdSelftest,
	"__trampoline":     cmdTrampoline,
}

func main() {
	if len(os.Args) < 2 || os.Args[1] == "help" || os.Args[1] == "--help" || os.Args[1] == "-h" {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if os.Args[1] == "version" || os.Args[1] == "--version" {
		printVersion()
		return
	}
	cmd, ok := commands[os.Args[1]]
	if !ok {
		fmt.Fprintf(os.Stderr, "checkpoint: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err := cmd(os.Args[2:]); err != nil {
		fmt.Fprintf(os.Stderr, "checkpoint: %v\n", err)
		os.Exit(1)
	}
}

const usage = `checkpoint: version control for long-running agent sessions

  checkpoint doctor                         will this work on this machine?
  checkpoint protect [--stop] [DIR]         start (or stop) recording a project
  checkpoint run -- <command...>            run an agent; its turn becomes a checkpoint
  checkpoint save [--name LABEL]            cut a checkpoint now
  checkpoint undo [--only a,b] [--save-both]  revert the latest turn's agent-only changes
  checkpoint history [--json]               every checkpoint, newest first
  checkpoint status [--json]                what is protected and what was missed
  checkpoint restore [--only a,b] ID [DIR]  rebuild the project from a checkpoint
  checkpoint recover [--to DIR]             files no checkpoint ever held
  checkpoint prune [--keep-days N] [--yes]  expire old checkpoints, reclaim space
  checkpoint ui                             terminal UI over history and status
  checkpoint selftest [--verbose]           prove the guarantees on this machine
  checkpoint version

Every command takes --root DIR (default: current directory) and --store DIR
(default: ~/.local/share/checkpoint/<project>). The store always lives outside
the project, so deleting the project cannot delete its history.

coming from git:  log -> history   checkout <sha> -> restore ID   revert -> undo
                  stash -> nothing to do   gc -> prune   (none) -> recover
`

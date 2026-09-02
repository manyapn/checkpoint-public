// Command checkpoint is the CLI surface: it establishes standing protection over
// a project, captures every completed filesystem write with the writing
// process's lineage, cuts checkpoints into an out-of-tree content-addressed
// store, and reverts an agent's changes while leaving human edits alone.
//
// Every command resolves its store through resolveStore, which enforces the
// invariant the whole design rests on: the store never lives inside the project
// it protects, so `rm -rf project` cannot destroy the means of recovering it.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "create":
		err = cmdCreate(os.Args[2:])
	case "restore":
		err = cmdRestore(os.Args[2:])
	case "capture":
		err = cmdCapture(os.Args[2:])
	case "recover":
		err = cmdRecover(os.Args[2:])
	case "daemon":
		err = cmdDaemon(os.Args[2:])
	case "protect":
		err = cmdProtect(os.Args[2:])
	case "doctor":
		err = cmdDoctor(os.Args[2:])
	case "selftest":
		err = cmdSelftest(os.Args[2:])
	case "ui":
		err = cmdUI(os.Args[2:])
	case "run":
		err = cmdRun(os.Args[2:])
	case "save":
		err = cmdSave(os.Args[2:])
	case "undo":
		err = cmdUndo(os.Args[2:])
	case "history":
		err = cmdHistory(os.Args[2:])
	case "prune":
		err = cmdPrune(os.Args[2:])
	case "status":
		err = cmdStatus(os.Args[2:])
	case "register-agent":
		err = cmdRegisterAgent(os.Args[2:], true)
	case "unregister-agent":
		err = cmdRegisterAgent(os.Args[2:], false)
	case "version", "--version":
		printVersion()
		return
	case "__trampoline":
		// Internal: `run`'s registration gate (see cmdRun). Not in usage.
		err = cmdTrampoline(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "%s: unknown command %q\n\n", prog(), os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", prog(), err)
		os.Exit(1)
	}
}

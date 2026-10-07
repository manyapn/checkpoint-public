package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/manyapn/checkpoint-public/internal/daemon"
	"github.com/manyapn/checkpoint-public/internal/lineage"
)

func cmdRun(args []string) error {
	fs, rootFlag, storeFlag := flags("run")
	noDaemon := fs.Bool("no-daemon", false, "run even with no daemon; nothing is recorded")
	fs.Parse(args)
	command := fs.Args()
	if len(command) == 0 {
		return fmt.Errorf("expected -- <command...>")
	}
	t, err := resolve(*rootFlag, *storeFlag)
	if err != nil {
		return err
	}
	if !daemon.Running(t.sock) {
		if !*noDaemon {
			return fmt.Errorf("%s is not protected, so nothing would be recorded.\n  start protection first: checkpoint protect %s\n  or run unprotected:     checkpoint run --no-daemon -- ...", t.root, t.root)
		}
		fmt.Fprintln(os.Stderr, "checkpoint: WARNING: running unprotected; nothing is recorded")
	} else {
		fmt.Fprintln(os.Stderr, "checkpoint: local file changes are recoverable; network calls, deploys and emails are not")
	}

	// The child stops itself before exec so it can be registered as the agent
	// root while frozen; otherwise a short-lived grandchild (rm, mv) could be
	// born and gone before the daemon starts tracking parents.
	self, _ := os.Executable()
	child := exec.Command(self, append([]string{"__trampoline", "--"}, command...)...)
	child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := child.Start(); err != nil {
		return err
	}
	waitStopped(child.Process.Pid)
	start, _ := lineage.StartTime(child.Process.Pid)
	id := lineage.Identity{Pid: child.Process.Pid, Start: start}
	if err := daemon.Register(t.sock, id); err != nil && !*noDaemon {
		fmt.Fprintf(os.Stderr, "checkpoint: could not register the agent: %v\n", err)
	}
	syscall.Kill(child.Process.Pid, syscall.SIGCONT)
	runErr := child.Wait()

	if !*noDaemon {
		res, err := daemon.Checkpoint(t.sock, "run: "+strings.Join(command, " "), "")
		daemon.Unregister(t.sock, id)
		report(res, err)
	}
	if exit, ok := runErr.(*exec.ExitError); ok {
		os.Exit(exit.ExitCode())
	}
	return runErr
}

func report(res daemon.Result, err error) {
	switch {
	case err != nil:
		fmt.Fprintf(os.Stderr, "checkpoint: %v\n", err)
	case res.Skipped:
		fmt.Printf("no changes since checkpoint %d\n", res.ID)
	default:
		note := ""
		if res.SettleTimedOut {
			note = " (writes were still arriving; the command may have been mid-operation)"
		}
		fmt.Printf("checkpoint %d: %s, %s%s\n", res.ID, res.Badge, plural(res.Entries, "entry", "entries"), note)
	}
}

func cmdTrampoline(args []string) error {
	if len(args) < 2 || args[0] != "--" {
		return fmt.Errorf("internal")
	}
	path, err := exec.LookPath(args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "checkpoint: %s: command not found\n", args[1])
		os.Exit(127)
	}
	syscall.Kill(os.Getpid(), syscall.SIGSTOP)
	return syscall.Exec(path, args[1:], os.Environ())
}

func waitStopped(pid int) {
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			return
		}
		s := string(b)
		if i := strings.LastIndexByte(s, ')'); i >= 0 && len(s) > i+2 && (s[i+2] == 'T' || s[i+2] == 't') {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

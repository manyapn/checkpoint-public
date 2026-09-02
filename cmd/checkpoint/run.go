package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/manyapn/checkpoint-public/internal/daemon"
	"github.com/manyapn/checkpoint-public/internal/provenance"
)

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	rootFlag := fs.String("root", "", "protected root the daemon watches (default: current directory)")
	storeFlag := fs.String("store", "", "store directory (default: derived from root path)")
	noDaemonFlag := fs.Bool("no-daemon", false, "run the command even though no daemon is protecting the root; NOTHING is captured or recoverable")
	fs.Parse(args)
	cmdArgs := fs.Args()
	if len(cmdArgs) == 0 {
		return fmt.Errorf("run: expected -- <command...>")
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

	sock := daemon.SocketPath(storeDir)
	// A wrapped run must never execute unprotected while announcing that local
	// changes are recoverable: without a live daemon nothing is captured, so
	// the promise is false and the user learns only afterwards.
	// Refuse up front, and say exactly how to get protected. --no-daemon is the
	// explicit, self-labelling override for "run it anyway, unprotected".
	if _, err := daemon.RequestStatus(sock); err != nil {
		if !*noDaemonFlag {
			return fmt.Errorf("refusing to run unprotected: no daemon is answering for %s "+
				"(store %s). Nothing would be captured and no checkpoint could be cut, so this "+
				"command would run with NO recoverability.\n"+
				"  start protection first:  %s protect --store %s %s\n"+
				"  or run without capture:  %s run --no-daemon ... (nothing is recoverable)",
				root, storeDir, prog(), storeDir, root, prog())
		}
		fmt.Fprintln(os.Stderr, prog()+": WARNING: --no-daemon was given, so this command runs with NO capture: "+
			"nothing it writes is recoverable and no checkpoint will be cut.")
	} else {
		// The recoverability boundary, shown before every protected run:
		// permission is not recoverability, and we answer only the latter.
		fmt.Fprintln(os.Stderr, prog()+": local file changes are recoverable; network requests, remote databases, deploys, emails, and other outside effects are NOT.")
	}
	// The child runs through a self-stopping trampoline: it SIGSTOPs itself
	// BEFORE exec'ing the real command, we register it as an agent root while it
	// is stopped, then SIGCONT. Without this gate, a one-shot child the command
	// forks in its first milliseconds (rm, mv) can be born before the daemon's
	// birth-parent scanner activates. Once reaped, its lineage is unresolvable,
	// so a genuine agent write would degrade to unknown.
	self, err := os.Executable()
	if err != nil {
		return err
	}
	c := exec.Command(self, append([]string{"__trampoline", "--"}, cmdArgs...)...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Start(); err != nil {
		return err
	}
	child := c.Process.Pid
	if err := waitStopped(child, 2*time.Second); err != nil {
		fmt.Fprintf(os.Stderr, "%s: trampoline gate: %v (continuing)\n", prog(), err)
	}
	// Register the command's process tree as an agent root, so its writes (and
	// its descendants') attribute to the agent by lineage rather than by
	// boundary. The registration brackets the settle so trailing writes still
	// classify.
	childStart, _ := provenance.StartTime(child)
	if err := daemon.RegisterAgentRoot(sock, child, childStart); err != nil {
		fmt.Fprintf(os.Stderr, "%s: agent-root registration failed (is the daemon running?): %v\n", prog(), err)
	}
	syscall.Kill(child, syscall.SIGCONT)
	runErr := c.Wait()

	// Request a checkpoint whether or not the command succeeded, because its
	// writes matter either way. The daemon settles, then cuts exactly one
	// checkpoint.
	resp, reqErr := daemon.RequestCheckpoint(sock, "run: "+strings.Join(cmdArgs, " "))
	daemon.UnregisterAgentRoot(sock, child, childStart)
	if reqErr != nil {
		fmt.Fprintf(os.Stderr, "%s: boundary request failed (is the daemon running?): %v\n", prog(), reqErr)
	} else if resp.SkippedEmpty {
		fmt.Printf("no changes since checkpoint %d, so no new checkpoint was created\n", resp.ID)
	} else {
		tag := ""
		if resp.SettleTimedOut {
			tag = " (settle timed out; trailing writes may be mid-operation)"
		}
		fmt.Printf("checkpoint %d %s (%d entries)%s\n", resp.ID, resp.Coverage, resp.Entries, tag)
	}

	// Propagate the wrapped command's exit code.
	if runErr != nil {
		if ee, ok := runErr.(*exec.ExitError); ok {
			os.Exit(ee.ExitCode())
		}
		return runErr
	}
	return nil
}

// cmdTrampoline stops itself, then execs the real command in place (same pid).
// The parent `run` registers this pid as an agent root while it is stopped and
// SIGCONTs it, so every descendant, however short-lived, is born with the
// birth-parent scanner already active.
func cmdTrampoline(args []string) error {
	if len(args) < 2 || args[0] != "--" {
		return fmt.Errorf("__trampoline: internal use only")
	}
	argv := args[1:]
	path, err := exec.LookPath(argv[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %s: command not found\n", prog(), argv[0])
		os.Exit(127)
	}
	syscall.Kill(os.Getpid(), syscall.SIGSTOP) // parent SIGCONTs after registering
	return syscall.Exec(path, argv, os.Environ())
}

func waitStopped(pid int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			return err
		}
		s := string(b)
		if i := strings.LastIndexByte(s, ')'); i >= 0 && len(s) > i+2 {
			if st := s[i+2]; st == 'T' || st == 't' {
				return nil
			}
		}
		time.Sleep(time.Millisecond)
	}
	return fmt.Errorf("pid %d not stopped within %s", pid, timeout)
}

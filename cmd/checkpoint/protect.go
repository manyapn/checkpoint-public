package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/manyapn/checkpoint-public/internal/daemon"
	"github.com/manyapn/checkpoint-public/internal/provenance"
)

// cmdProtect establishes standing protection: it starts the daemon DETACHED
// (own session, logs to <store>/daemon.log, pid recorded in <store>/daemon.pid)
// and confirms protection before returning. --stop tears it down. The daemon
// subcommand stays for foreground/supervised use; protect is the everyday form.
func cmdProtect(args []string) error {
	fs := flag.NewFlagSet("protect", flag.ExitOnError)
	storeFlag := fs.String("store", "", "store directory (default: derived from root path)")
	protectFlag := fs.String("protect", "", "comma-separated additional folders to protect (absolute paths)")
	stopFlag := fs.Bool("stop", false, "stop the standing daemon for this root")
	fs.Parse(args)
	root := ""
	if fs.NArg() > 1 {
		return fmt.Errorf("protect: expected at most one <root>")
	}
	if fs.NArg() == 1 {
		root = fs.Arg(0)
	}
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
	if err := checkSocketPath(storeDir); err != nil {
		return err
	}
	sock := daemon.SocketPath(storeDir)
	pidFile := filepath.Join(storeDir, "daemon.pid")

	if *stopFlag {
		b, err := os.ReadFile(pidFile)
		if err != nil {
			if _, serr := daemon.RequestStatus(sock); serr != nil {
				fmt.Println("not protected (no standing daemon)")
				return nil
			}
			return fmt.Errorf("protect --stop: a daemon answers on %s but %s is missing; it was started in the foreground, so stop it there", sock, pidFile)
		}
		var pid int
		if _, err := fmt.Sscanf(strings.TrimSpace(string(b)), "%d", &pid); err != nil || pid <= 0 {
			return fmt.Errorf("protect --stop: %s is corrupt; stop the daemon manually", pidFile)
		}
		// Identify the process before signalling, so a pid recycled during
		// shutdown cannot be mistaken for the daemon still running.
		start, haveStart := provenance.StartTime(pid)
		if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
		// Wait for the PROCESS to exit, not merely for its socket to stop
		// answering. The daemon closes its listener early in shutdown and then
		// still cuts a final checkpoint and releases the store locks, so a stop
		// that returned on socket silence would report success while the old
		// daemon still held the versionlog lock. The next `protect` would then
		// fail to start, and `prune`, which requires a stopped daemon, could run
		// against a live one.
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if !stillRunning(pid, start, haveStart) {
				os.Remove(pidFile)
				fmt.Printf("protection stopped for %s\n", root)
				return nil
			}
			time.Sleep(20 * time.Millisecond)
		}
		return fmt.Errorf("protect --stop: daemon (pid %d) did not exit within 10s", pid)
	}

	if st, err := daemon.RequestStatus(sock); err == nil {
		fmt.Printf("already protected (daemon running since %s; %d checkpoint(s))\n",
			time.Unix(0, st.SinceUnixNS).Format("2006-01-02 15:04:05"), st.Checkpoints)
		return nil
	}
	// Statically invalid configurations must fail NOW, not after a 15s readiness
	// wait on a daemon that already exited: nesting is decidable from the paths
	// alone, and the foreground `daemon` rejects it immediately.
	var protectExtras []string
	for _, p := range strings.Split(*protectFlag, ",") {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		abs, err := resolveDir(p)
		if err != nil {
			return err
		}
		if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
			return fmt.Errorf("protect: protected folder %s is not a directory", abs)
		}
		protectExtras = append(protectExtras, abs)
	}
	allRoots := append([]string{root}, protectExtras...)
	for i, a := range allRoots {
		for j, b := range allRoots {
			if i != j && (a == b || strings.HasPrefix(a, b+string(filepath.Separator))) {
				return fmt.Errorf("protect: protected folders must not nest: %s is under %s", a, b)
			}
		}
	}
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	logF, err := os.OpenFile(filepath.Join(storeDir, "daemon.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer logF.Close()
	dArgs := []string{"daemon", "--store", storeDir}
	if len(protectExtras) > 0 {
		dArgs = append(dArgs, "--protect", strings.Join(protectExtras, ","))
	}
	dArgs = append(dArgs, root)
	c := exec.Command(self, dArgs...)
	c.Stdout, c.Stderr = logF, logF
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // survives this shell/session
	if err := c.Start(); err != nil {
		return err
	}
	if err := os.WriteFile(pidFile, []byte(fmt.Sprintf("%d\n", c.Process.Pid)), 0o600); err != nil {
		return err
	}
	go c.Wait() // reap if it dies before we detach
	// Confirm protection (or fail with the daemon's own words) before returning.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if st, err := daemon.RequestStatus(sock); err == nil {
			state := "protected"
			if st.SettingUp {
				state = "setting up (first scan running)"
			} else if !st.BaselineComplete {
				state = "limited (baseline incomplete; auto-rescan running)"
			}
			fmt.Printf("protection started for %s: %s [log: %s]\n", root, state, filepath.Join(storeDir, "daemon.log"))
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	b, _ := os.ReadFile(filepath.Join(storeDir, "daemon.log"))
	tail := string(b)
	if len(tail) > 500 {
		tail = tail[len(tail)-500:]
	}
	os.Remove(pidFile)
	return fmt.Errorf("daemon did not become ready within 15s; log tail:\n%s", tail)
}

// waitStopped polls /proc until pid reaches the stopped state (the trampoline's
// self-SIGSTOP has landed).
// stillRunning reports whether pid is still the process it was when start was
// read. A pid that vanished, or that now carries a different start-time (the
// number was reused), is not the daemon any more.
func stillRunning(pid int, start uint64, haveStart bool) bool {
	now, ok := provenance.StartTime(pid)
	if !ok {
		return false // no /proc entry: the process is gone
	}
	return !haveStart || now == start
}

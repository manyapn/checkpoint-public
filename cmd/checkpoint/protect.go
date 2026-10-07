package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/manyapn/checkpoint-public/internal/daemon"
	"github.com/manyapn/checkpoint-public/internal/lineage"
)

// protect starts the daemon detached, logging to <store>/daemon.log, and
// confirms it answers before returning.
func cmdProtect(args []string) error {
	fs, rootFlag, storeFlag := flags("protect")
	stop := fs.Bool("stop", false, "stop protection for this folder")
	fs.Parse(args)
	if fs.NArg() > 1 {
		return fmt.Errorf("expected at most one folder")
	}
	if fs.NArg() == 1 {
		*rootFlag = fs.Arg(0)
	}
	t, err := resolve(*rootFlag, *storeFlag)
	if err != nil {
		return err
	}
	pidFile := filepath.Join(t.storeDir, "daemon.pid")
	if *stop {
		return stopDaemon(t, pidFile)
	}
	if st, err := daemon.GetStatus(t.sock); err == nil {
		fmt.Printf("already protected since %s (%s)\n",
			time.Unix(0, st.SinceNS).Format("2006-01-02 15:04:05"), plural(st.Checkpoints, "checkpoint", "checkpoints"))
		return nil
	}
	if err := os.MkdirAll(t.storeDir, 0o700); err != nil {
		return err
	}
	self, _ := os.Executable()
	logFile, err := os.OpenFile(filepath.Join(t.storeDir, "daemon.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	cmd := exec.Command(self, "daemon", "--store", t.storeDir, t.root)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	os.WriteFile(pidFile, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o600)
	go cmd.Wait()
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		if daemon.Running(t.sock) {
			fmt.Printf("protection started for %s (log: %s)\n", t.root, logFile.Name())
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	os.Remove(pidFile)
	tail, _ := os.ReadFile(logFile.Name())
	return fmt.Errorf("daemon did not start within 15s; log:\n%s", tail)
}

// stopDaemon signals the daemon and waits for the process (not just the
// socket) to go away, because it still holds the store while it cuts its
// final checkpoint.
func stopDaemon(t target, pidFile string) error {
	b, err := os.ReadFile(pidFile)
	if err != nil {
		if daemon.Running(t.sock) {
			return fmt.Errorf("a daemon answers on %s but was started in the foreground; stop it there", t.sock)
		}
		fmt.Println("not protected")
		return nil
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return fmt.Errorf("%s is corrupt", pidFile)
	}
	start, _ := lineage.StartTime(pid)
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		if now, ok := lineage.StartTime(pid); !ok || now != start {
			os.Remove(pidFile)
			fmt.Printf("protection stopped for %s\n", t.root)
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("daemon (pid %d) did not exit within 10s", pid)
}

// daemon runs protection in the foreground; protect uses it detached.
func cmdDaemon(args []string) error {
	fs, rootFlag, storeFlag := flags("daemon")
	fs.Parse(args)
	if fs.NArg() == 1 {
		*rootFlag = fs.Arg(0)
	}
	t, err := resolve(*rootFlag, *storeFlag)
	if err != nil {
		return err
	}
	if len(t.sock)+1 > 108 {
		return fmt.Errorf("store path too long for a Unix socket (%d bytes, max 107); use a shorter --store", len(t.sock))
	}
	ready, stop, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() { done <- daemon.Serve(daemon.Config{Root: t.root, StoreDir: t.storeDir}, ready, stop) }()
	select {
	case <-ready:
		fmt.Printf("protecting %s (store %s)\n", t.root, t.storeDir)
	case err := <-done:
		return err
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-sig:
		close(stop)
		return <-done
	case err := <-done:
		return err
	}
}

func cmdRegisterAgent(args []string, register bool) error {
	fs, rootFlag, storeFlag := flags("register-agent")
	pid := fs.Int("pid", 0, "pid of the agent process")
	fs.Parse(args)
	t, err := resolve(*rootFlag, *storeFlag)
	if err != nil {
		return err
	}
	start, ok := lineage.StartTime(*pid)
	if !ok {
		return fmt.Errorf("no live process with pid %d", *pid)
	}
	id := lineage.Identity{Pid: *pid, Start: start}
	if register {
		return daemon.Register(t.sock, id)
	}
	return daemon.Unregister(t.sock, id)
}

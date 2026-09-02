package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/manyapn/checkpoint-public/internal/daemon"
)

func cmdDaemon(args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	storeFlag := fs.String("store", "", "store directory (default: derived from root path)")
	protectFlag := fs.String("protect", "", "comma-separated additional folders to protect (absolute paths)")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("daemon: expected <root>")
	}
	root, err := resolveDir(fs.Arg(0))
	if err != nil {
		return err
	}
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return fmt.Errorf("daemon: %s is not a directory", root)
	}
	var extra []string
	for _, p := range strings.Split(*protectFlag, ",") {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			return err
		}
		if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
			return fmt.Errorf("daemon: protected folder %s is not a directory", abs)
		}
		extra = append(extra, abs)
	}
	storeDir, err := resolveStore(*storeFlag, root)
	if err != nil {
		return err
	}
	if err := checkSocketPath(storeDir); err != nil {
		return err
	}
	ready := make(chan struct{})
	stop := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		errCh <- daemon.Serve(daemon.Config{Workspace: root, Extra: extra, StoreDir: storeDir}, ready, stop)
	}()
	select {
	case <-ready:
		fmt.Printf("READY root=%s store=%s socket=%s\n", root, storeDir, daemon.SocketPath(storeDir))
	case err := <-errCh:
		return err // failed before it was listening
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-sig:
		close(stop)
		return <-errCh
	case err := <-errCh:
		return err
	}
}

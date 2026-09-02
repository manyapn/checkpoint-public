package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/manyapn/checkpoint-public/internal/capture"
)

func cmdCapture(args []string) error {
	fs := flag.NewFlagSet("capture", flag.ExitOnError)
	storeFlag := fs.String("store", "", "store directory (default: derived from workspace path)")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("capture: expected <workspace>")
	}
	ws, err := resolveDir(fs.Arg(0))
	if err != nil {
		return err
	}
	if fi, err := os.Stat(ws); err != nil || !fi.IsDir() {
		return fmt.Errorf("capture: %s is not a directory", ws)
	}
	storeDir, err := resolveStore(*storeFlag, ws)
	if err != nil {
		return err
	}
	w, err := capture.New(capture.Config{Workspace: ws, StoreDir: storeDir})
	if err != nil {
		return err
	}
	defer w.Close()

	stop := make(chan struct{})
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		close(stop)
	}()
	fmt.Printf("capturing %s -> %s (Ctrl-C to stop)\n", ws, storeDir)
	runErr := w.Run(stop)
	fmt.Printf("\ncaptured %d versions (%d missed)\n", len(w.Versions()), w.Missed())
	return runErr
}

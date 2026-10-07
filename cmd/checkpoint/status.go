package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/manyapn/checkpoint-public/internal/daemon"
	"github.com/manyapn/checkpoint-public/internal/snapshot"
)

type statusView struct {
	daemon.Status
	Store        string `json:"store"`
	StorageBytes int64  `json:"storage_bytes"`
}

func cmdStatus(args []string) error {
	fs, rootFlag, storeFlag := flags("status")
	asJSON := fs.Bool("json", false, "machine-readable output")
	fs.Parse(args)
	if err := noArgs(fs); err != nil {
		return err
	}
	t, err := resolve(*rootFlag, *storeFlag)
	if err != nil {
		return err
	}
	usage := snapshot.MeasureUsage(t.storeDir)
	v := statusView{Store: t.storeDir, StorageBytes: usage.Bytes}
	v.Status, err = daemon.GetStatus(t.sock)
	if err != nil {
		v.Status = daemon.Status{Root: t.root, Checkpoints: usage.Checkpoints, Missed: []string{}, Outside: []string{}}
		if latest, _ := snapshot.Latest(t.storeDir); latest != nil {
			v.LastCheckpointNS = latest.TimeNS
		}
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(v)
	}
	switch {
	case !v.Protected:
		fmt.Println("Protection: not running (checkpoint protect to start)")
	case v.Limited():
		fmt.Println("Protection: limited")
	default:
		fmt.Println("Protection: on")
	}
	fmt.Printf("Root: %s\nStore: %s (%s, %s)\n", t.root, t.storeDir, humanBytes(usage.Bytes), plural(v.Checkpoints, "checkpoint", "checkpoints"))
	fmt.Printf("Last checkpoint: %s\n", ago(v.LastCheckpointNS))
	if !v.Protected {
		return nil
	}
	fmt.Printf("Protecting since: %s\n", time.Unix(0, v.SinceNS).Format("2006-01-02 15:04:05"))
	fmt.Printf("Agent sessions: %d\n", v.AgentSessions)
	if v.FeedActive {
		fmt.Println("Change feed: on (deletions attributed, checkpoints scale with changes)")
	} else {
		fmt.Println("Change feed: off on this filesystem (deletions not attributed, full scan per checkpoint)")
	}
	if v.Overflowed {
		fmt.Println("  ! the kernel dropped events since the last checkpoint; some writes may be unrecorded")
	}
	for _, p := range v.Missed {
		fmt.Printf("  ! write not captured: %s\n", p)
	}
	if v.OutsideCount > 0 {
		fmt.Printf("  ! the agent wrote %s outside the protected folder (not recorded):\n", plural(v.OutsideCount, "time", "times"))
		for _, p := range v.Outside {
			fmt.Printf("      %s\n", p)
		}
	}
	if latest, _ := snapshot.Latest(t.storeDir); latest != nil && len(latest.Exceptions) > 0 {
		fmt.Printf("Not covered by checkpoint %d:\n", latest.ID)
		for _, ex := range latest.Exceptions {
			fmt.Printf("  ! %s (%s)\n", ex.Path, ex.Reason)
		}
	}
	return nil
}

func ago(ns int64) string {
	if ns <= 0 {
		return "none yet"
	}
	return time.Since(time.Unix(0, ns)).Round(time.Second).String() + " ago"
}

func humanBytes(n int64) string {
	units := []string{"B", "kB", "MB", "GB", "TB"}
	v := float64(n)
	i := 0
	for v >= 1000 && i < len(units)-1 {
		v /= 1000
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}

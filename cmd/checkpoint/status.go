package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/manyapn/checkpoint-public/internal/daemon"
	"github.com/manyapn/checkpoint-public/internal/store"
)

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	rootFlag := fs.String("root", "", "protected root (default: current directory)")
	storeFlag := fs.String("store", "", "store directory (default: derived from root path)")
	jsonFlag := fs.Bool("json", false, "emit JSON")
	fs.Parse(args)
	if fs.NArg() != 0 {
		return fmt.Errorf("status: unexpected argument %q (this command takes only flags)", fs.Arg(0))
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
	warnStoreFor(storeDir, root)
	st, err := daemon.RequestStatus(daemon.SocketPath(storeDir))
	if err != nil {
		// Not protected: no daemon running for this project. Honest, not an error.
		ids, _ := store.ValidIDs(storeDir) // a torn manifest is not a checkpoint
		lastNS := int64(0)
		if m, ok, _ := store.Latest(storeDir); ok {
			lastNS = m.TimeNS
		}
		if *jsonFlag {
			// List fields are ALWAYS arrays, even with no daemon: a client
			// doing status.outside.length must never hit null/undefined.
			return json.NewEncoder(os.Stdout).Encode(daemon.Status{
				Protected: false, Root: root, Checkpoints: len(ids), LastCkptNS: lastNS,
				ProtectedDirs: []string{root}, Outside: []string{},
			})
		}
		fmt.Printf("Protection: Not protected (no daemon running for this project)\n")
		fmt.Printf("Root: %s\nStore: %s\nCheckpoints on record: %d\n", root, storeDir, len(ids))
		if u, uerr := store.MeasureUsage(storeDir); uerr == nil {
			fmt.Printf("Storage: %s\n", u.Human())
		}
		fmt.Printf("Last complete checkpoint: %s\n", agoOrNone(lastNS))
		return nil
	}
	if *jsonFlag {
		return json.NewEncoder(os.Stdout).Encode(st)
	}
	state := "Protected"
	if st.Limited {
		state = "Limited protection"
	}
	if st.SettingUp {
		state = "Setting up (first scan still running)"
		if st.SetupScanned > 0 {
			state = fmt.Sprintf("Setting up (first scan running, %d files scanned so far)", st.SetupScanned)
		}
	}
	fmt.Printf("Protection: %s\n", state)
	fmt.Printf("Root: %s\n", st.Root)
	for _, d := range st.ProtectedDirs {
		if d != st.Root {
			fmt.Printf("Also protecting: %s\n", d)
		}
	}
	fmt.Printf("Store: %s\n", storeDir)
	if u, uerr := store.MeasureUsage(storeDir); uerr == nil {
		fmt.Printf("Storage: %s\n", u.Human())
	}
	fmt.Printf("Checkpoints: %d\n", st.Checkpoints)
	fmt.Printf("Last complete checkpoint: %s\n", agoOrNone(st.LastCkptNS))
	if st.BaselineComplete {
		fmt.Println("Complete baseline: yes")
	} else {
		fmt.Println("Complete baseline: NO")
	}
	if st.FeedActive {
		fmt.Println("Change feed: active (delete attribution + change-scaled checkpoints)")
	} else {
		fmt.Println("Change feed: unavailable on this filesystem; delete attribution is unavailable and checkpoints use full scans")
	}
	fmt.Printf("Active agent sessions: %d\n", st.AgentSessions)
	fmt.Printf("Protecting since: %s\n", time.Unix(0, st.SinceUnixNS).Format("2006-01-02 15:04:05"))
	if st.Limited {
		fmt.Println("Why limited:")
		if !st.BaselineComplete {
			fmt.Println("  ! first scan incomplete (event overflow during setup): there is no complete baseline yet, and a clean rescan runs automatically until one succeeds")
		}
		if st.Overflowed {
			fmt.Println("  ! a burst overflowed the watch queue, so some changes since the last checkpoint may be uncaptured (unbounded)")
		}
		if st.Missed > 0 {
			fmt.Printf("  ! %d file(s) since the last checkpoint could not be captured\n", st.Missed)
		}
		if st.OutsideCount > 0 {
			fmt.Printf("  ! unprotected changes: the agent wrote %d time(s) outside the protected folders (not captured, not restorable):\n", st.OutsideCount)
			for _, p := range st.Outside {
				fmt.Printf("      %s\n", p)
			}
			if st.OutsideCount > len(st.Outside) {
				fmt.Printf("      … more paths beyond the %d listed\n", len(st.Outside))
			}
			fmt.Println("      (add folders with: daemon --protect DIR,DIR)")
		}
	}
	// Current exceptions: what the LATEST checkpoint could not cover, by name.
	if m, ok, _ := store.Latest(storeDir); ok && len(m.Exceptions) > 0 {
		fmt.Printf("Current exceptions (checkpoint %d):\n", m.ID)
		for _, ex := range m.Exceptions {
			fmt.Printf("  ! %s (%s)\n", ex.Path, ex.Reason)
		}
	}
	return nil
}

// agoOrNone renders a checkpoint timestamp as a worst-case-loss age ("how much
// could you lose right now"), or "none yet".
func agoOrNone(ns int64) string {
	if ns <= 0 {
		return "none yet"
	}
	d := time.Since(time.Unix(0, ns)).Round(time.Second)
	if d < 0 {
		d = 0
	}
	return fmt.Sprintf("%s ago", d)
}

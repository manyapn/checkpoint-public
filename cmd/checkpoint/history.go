package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/manyapn/checkpoint-public/internal/status"
	"github.com/manyapn/checkpoint-public/internal/store"
)

func cmdHistory(args []string) error {
	fs := flag.NewFlagSet("history", flag.ExitOnError)
	rootFlag := fs.String("root", "", "protected root (default: current directory)")
	storeFlag := fs.String("store", "", "store directory (default: derived from root path)")
	jsonFlag := fs.Bool("json", false, "emit JSON (a pure client contract; the TUI reads this)")
	fs.Parse(args)
	if fs.NArg() != 0 {
		return fmt.Errorf("history: unexpected argument %q (this command takes only flags)", fs.Arg(0))
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
	ids, err := store.IDs(storeDir)
	if err != nil {
		return err
	}

	// Newest first: the most recent checkpoint is the one a user is looking for.
	type row struct {
		ID             int               `json:"id"`
		TimeNS         int64             `json:"time_ns"`
		Badge          string            `json:"badge"`
		Source         string            `json:"source"`
		Name           string            `json:"name"` // "" when unnamed (always present; client contract)
		SettleTimedOut bool              `json:"settle_timed_out"`
		Missed         int               `json:"missed"`
		Exceptions     []store.Exception `json:"exceptions"`
	}
	rows := []row{} // never null in --json: "checkpoints" is [] on an empty store (client contract)
	for i := len(ids) - 1; i >= 0; i-- {
		m, err := store.Load(storeDir, ids[i])
		if err != nil {
			continue
		}
		exc := m.Exceptions
		if exc == nil {
			exc = []store.Exception{} // never null (client contract)
		}
		rows = append(rows, row{m.ID, m.TimeNS, status.Of(m).String(), m.Source, m.Name, m.SettleTimedOut, m.Missed, exc})
	}

	if *jsonFlag {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"checkpoints": rows})
	}
	if len(rows) == 0 {
		fmt.Println("no checkpoints yet")
		return nil
	}
	for _, r := range rows {
		when := time.Unix(0, r.TimeNS).Format("2006-01-02 15:04:05")
		note := ""
		if r.SettleTimedOut {
			note = "  [settle timed out]"
		}
		if r.Missed > 0 {
			note += fmt.Sprintf("  [%d file(s) uncaptured]", r.Missed)
		}
		src := r.Source
		if src == "" {
			src = "(unlabeled)"
		}
		if r.Name != "" {
			src += fmt.Sprintf("  name:%q", r.Name)
		}
		fmt.Printf("#%-3d %s  %-28s  %s%s\n", r.ID, when, r.Badge, src, note)
		for _, ex := range r.Exceptions {
			fmt.Printf("      ! %s (%s)\n", ex.Path, ex.Reason)
		}
	}
	return nil
}

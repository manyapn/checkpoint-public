package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/manyapn/checkpoint-public/internal/snapshot"
)

type historyRow struct {
	ID             int                  `json:"id"`
	TimeNS         int64                `json:"time_ns"`
	Badge          string               `json:"badge"`
	Source         string               `json:"source"`
	Name           string               `json:"name"`
	SettleTimedOut bool                 `json:"settle_timed_out"`
	Exceptions     []snapshot.Exception `json:"exceptions"`
}

func cmdHistory(args []string) error {
	fs, rootFlag, storeFlag := flags("history")
	asJSON := fs.Bool("json", false, "machine-readable output")
	fs.Parse(args)
	if err := noArgs(fs); err != nil {
		return err
	}
	t, err := resolve(*rootFlag, *storeFlag)
	if err != nil {
		return err
	}
	all, err := snapshot.All(t.storeDir)
	if err != nil {
		return err
	}
	rows := []historyRow{}
	for i := len(all) - 1; i >= 0; i-- {
		c := all[i]
		exc := c.Exceptions
		if exc == nil {
			exc = []snapshot.Exception{}
		}
		rows = append(rows, historyRow{c.ID, c.TimeNS, c.Badge(), c.Source, c.Name, c.SettleTimedOut, exc})
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"checkpoints": rows})
	}
	if len(rows) == 0 {
		fmt.Println("no checkpoints yet")
		return nil
	}
	for _, r := range rows {
		label := r.Source
		if r.Name != "" {
			label += fmt.Sprintf("  name:%q", r.Name)
		}
		if r.SettleTimedOut {
			label += "  [settle timed out]"
		}
		fmt.Printf("#%-3d %s  %-28s %s\n", r.ID, time.Unix(0, r.TimeNS).Format("2006-01-02 15:04:05"), r.Badge, label)
		for _, ex := range r.Exceptions {
			fmt.Printf("      ! %s (%s)\n", ex.Path, ex.Reason)
		}
	}
	return nil
}

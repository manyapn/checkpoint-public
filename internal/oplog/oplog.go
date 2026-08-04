// Package oplog is the operation journal for the two commands that rewrite the
// working tree from recorded history: undo (the agent-scoped revert) and
// restore (checking a recorded checkpoint back out).
//
// The contract: the full plan is journaled BEFORE the first mutation, and the
// journal is cleared only after the operation completes. An operation
// interrupted mid-execution therefore leaves its journal behind, making the
// interruption diagnosable (what ran, when, against what, how far it planned)
// and safely retryable (plans are idempotent: restoring a file rewrites the
// same bytes, deleting an already-deleted path is a no-op).
//
// One in-flight operation per store: undo/restore are already serialized per
// store, and the journal mirrors that.
package oplog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Action is one planned mutation, in execution order. Restore-class actions
// carry their payload (ref/mode/link) so an interrupted operation can be
// REPLAYED from the journal alone. Recomputing a plan after a partial undo is
// wrong: the undo's own writes have already shifted the provenance window, so
// the recomputed plan comes back empty and a "safe rerun" would silently
// abandon the actions that never ran.
type Action struct {
	Do   string `json:"do"`             // restore-file | delete | save-both | restore-dir | restore-symlink
	Path string `json:"path"`           // absolute path the action touches
	Kind string `json:"kind,omitempty"` // file | symlink (payload kind)
	Ref  string `json:"ref,omitempty"`  // object-store ref of the content to write
	Mode uint32 `json:"mode,omitempty"`
	Link string `json:"link,omitempty"`
}

// Op is one journaled operation.
type Op struct {
	Kind         string   `json:"kind"` // undo | restore
	StartedNS    int64    `json:"started_ns"`
	Root         string   `json:"root"`                    // workspace the operation runs against
	Target       string   `json:"target,omitempty"`        // restore: the directory being written
	CheckpointID int      `json:"checkpoint_id,omitempty"` // restore: source id; undo: the undone turn
	Only         []string `json:"only,omitempty"`
	IncludeExtra bool     `json:"include_extra,omitempty"`
	Actions      []Action `json:"actions"`
}

func path(storeDir string) string { return filepath.Join(storeDir, "operation.json") }

// Begin journals op atomically before any mutation. A prior in-flight journal
// is overwritten: rerunning is the sanctioned retry path, and the caller has
// already surfaced the interruption via CheckInterrupted.
func Begin(storeDir string, op Op) error {
	if op.StartedNS == 0 {
		op.StartedNS = time.Now().UnixNano()
	}
	b, err := json.Marshal(op)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(storeDir, "op-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path(storeDir))
}

// Done clears the journal: the operation completed. Leaving it in place on
// partial failure is deliberate: the caller only calls Done on full success.
func Done(storeDir string) error {
	err := os.Remove(path(storeDir))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// CheckInterrupted reports a leftover journal from an operation that never
// completed. ok=false means no interruption. A corrupt journal is still
// reported (interrupted mid-Begin) with a nil Op.
func CheckInterrupted(storeDir string) (op *Op, ok bool) {
	b, err := os.ReadFile(path(storeDir))
	if err != nil {
		return nil, false
	}
	var o Op
	if err := json.Unmarshal(b, &o); err != nil {
		return nil, true // present but unreadable: still an interruption signal
	}
	return &o, true
}

// Describe renders an interruption for humans: what, when, how many actions,
// the first few paths.
func Describe(op *Op) string {
	if op == nil {
		return "an earlier operation was interrupted (journal unreadable); rerunning the intended command is safe"
	}
	age := time.Since(time.Unix(0, op.StartedNS)).Round(time.Second)
	s := fmt.Sprintf("an earlier %s (started %s ago, %d planned action(s) against %s) did not complete",
		op.Kind, age, len(op.Actions), op.Root)
	n := len(op.Actions)
	if n > 3 {
		n = 3
	}
	for i := 0; i < n; i++ {
		s += fmt.Sprintf("\n    %s %s", op.Actions[i].Do, op.Actions[i].Path)
	}
	if len(op.Actions) > 3 {
		s += fmt.Sprintf("\n    … and %d more", len(op.Actions)-3)
	}
	s += "\n  rerunning the operation is safe (plans are idempotent)"
	return s
}

package oplog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInterruptedOpIsDiagnosableAndCleared pins the journal contract: the plan
// is on disk from Begin until Done; an interruption (no Done) is reported with
// what/when/how much; Done clears it; a corrupt journal still signals.
func TestInterruptedOpIsDiagnosableAndCleared(t *testing.T) {
	dir := t.TempDir()

	if _, ok := CheckInterrupted(dir); ok {
		t.Fatal("fresh store must report no interruption")
	}

	op := Op{Kind: "undo", Root: "/w", Actions: []Action{
		{Do: "restore-file", Path: "/w/a.txt"},
		{Do: "delete", Path: "/w/gen.txt"},
		{Do: "restore-file", Path: "/w/b.txt"},
		{Do: "restore-file", Path: "/w/c.txt"},
	}}
	if err := Begin(dir, op); err != nil {
		t.Fatal(err)
	}
	got, ok := CheckInterrupted(dir)
	if !ok || got == nil || got.Kind != "undo" || len(got.Actions) != 4 {
		t.Fatalf("in-flight op must be diagnosable, got %+v ok=%v", got, ok)
	}
	d := Describe(got)
	for _, want := range []string{"undo", "4 planned action", "/w/a.txt", "and 1 more", "safe"} {
		if !strings.Contains(d, want) {
			t.Fatalf("description must carry %q, got:\n%s", want, d)
		}
	}

	if err := Done(dir); err != nil {
		t.Fatal(err)
	}
	if _, ok := CheckInterrupted(dir); ok {
		t.Fatal("completed op must clear the journal")
	}
	if err := Done(dir); err != nil {
		t.Fatal("Done is idempotent")
	}

	// Corrupt journal: still an interruption signal, never silently ignored.
	if err := os.WriteFile(filepath.Join(dir, "operation.json"), []byte("{torn"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, ok = CheckInterrupted(dir)
	if !ok || got != nil {
		t.Fatalf("corrupt journal must signal interruption with nil op, got %+v ok=%v", got, ok)
	}
	if !strings.Contains(Describe(nil), "unreadable") {
		t.Fatal("corrupt-journal description must say so")
	}
}

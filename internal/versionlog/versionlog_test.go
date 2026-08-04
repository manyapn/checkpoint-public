package versionlog

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAppendReadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "versionlog")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []Version{
		{Op: OpCreate, Path: "/w/a.txt", Ref: refA, Mode: 0o644},
		{Op: OpModify, Path: "/w/a.txt", Ref: refB, Mode: 0o644},
		{Op: OpDelete, Path: "/w/a.txt"},
	}
	for _, v := range want {
		if err := l.Append(v); err != nil {
			t.Fatalf("Append(%+v): %v", v, err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen: records survive, oldest-first, unchanged.
	l2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	got := l2.Versions()
	if len(got) != len(want) {
		t.Fatalf("got %d records, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("record %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestReopenAppendsAfterExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "versionlog")
	l, _ := Open(path)
	l.Append(Version{Op: OpCreate, Path: "/w/1", Ref: refA})
	l.Close()

	l2, _ := Open(path)
	l2.Append(Version{Op: OpCreate, Path: "/w/2", Ref: refB})
	if n := len(l2.Versions()); n != 2 {
		t.Fatalf("after reopen+append want 2 records, got %d", n)
	}
	l2.Close()

	l3, _ := Open(path)
	defer l3.Close()
	got := l3.Versions()
	if len(got) != 2 || got[0].Path != "/w/1" || got[1].Path != "/w/2" {
		t.Fatalf("append after reopen lost order/records: %+v", got)
	}
}

// TestTornTailRecovery is the kill-9 safety property: a partial final append
// (bytes written but no terminating newline, the trace a process killed
// mid-write leaves) is discarded on reopen, and the file is repaired to the
// last complete record so future appends stay clean. No valid record is ever
// lost.
func TestTornTailRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "versionlog")
	l, _ := Open(path)
	l.Append(Version{Op: OpCreate, Path: "/w/good1", Ref: refA})
	l.Append(Version{Op: OpCreate, Path: "/w/good2", Ref: refB})
	l.Close()

	// Simulate a torn final record: append a partial, unterminated JSON fragment.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"op":"create","path":"/w/torn`); err != nil {
		t.Fatal(err)
	}
	f.Close()

	l2, err := Open(path)
	if err != nil {
		t.Fatalf("Open must recover, not error: %v", err)
	}
	got := l2.Versions()
	if len(got) != 2 || got[0].Path != "/w/good1" || got[1].Path != "/w/good2" {
		t.Fatalf("torn tail not repaired cleanly: %+v", got)
	}
	// The repaired log must accept a fresh append that then reads back.
	if err := l2.Append(Version{Op: OpCreate, Path: "/w/good3", Ref: refA}); err != nil {
		t.Fatal(err)
	}
	l2.Close()

	l3, _ := Open(path)
	defer l3.Close()
	if got := l3.Versions(); len(got) != 3 || got[2].Path != "/w/good3" {
		t.Fatalf("append after torn-tail repair failed: %+v", got)
	}
}

// TestCorruptMidRecordStopsAtValidPrefix: a complete line that does not parse
// as JSON ends the recovery: records before it survive; it and anything after
// are dropped (never a half-valid record presented as real).
func TestCorruptMidRecordStopsAtValidPrefix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "versionlog")
	l, _ := Open(path)
	l.Append(Version{Op: OpCreate, Path: "/w/keep", Ref: refA})
	l.Close()

	f, _ := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	f.WriteString("{garbage not json}\n")
	f.WriteString(`{"op":"create","path":"/w/after","ref":"` + refB + `"}` + "\n")
	f.Close()

	l2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	got := l2.Versions()
	if len(got) != 1 || got[0].Path != "/w/keep" {
		t.Fatalf("corrupt record should stop recovery at valid prefix, got %+v", got)
	}
}

func TestSecondOpenIsBlocked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "versionlog")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if l2, err := Open(path); err == nil {
		l2.Close()
		t.Fatal("second Open of a held log should fail (single-writer flock)")
	}
}

const (
	refA = "0000000000000000000000000000000000000000"
	refB = "1111111111111111111111111111111111111111"
)

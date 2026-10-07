package writelog

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAppendThenRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "writes.jsonl")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	l.Append(Entry{Op: Write, Path: "/p/a", Ref: "r1", Writer: "agent", TimeNS: 1})
	l.Append(Entry{Op: Delete, Path: "/p/a", Writer: "human", TimeNS: 2})
	l.Close()
	got, err := Read(path)
	if err != nil || len(got) != 2 || got[1].Op != Delete || got[0].Writer != "agent" {
		t.Fatalf("Read = %+v, %v", got, err)
	}
}

func TestTornTailIsDroppedOnOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "writes.jsonl")
	l, _ := Open(path)
	l.Append(Entry{Op: Write, Path: "/p/a", TimeNS: 1})
	l.Close()
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"op":"write","path":"/p/b"`)
	f.Close()
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	l.Append(Entry{Op: Write, Path: "/p/c", TimeNS: 3})
	l.Close()
	got, _ := Read(path)
	if len(got) != 2 || got[1].Path != "/p/c" {
		t.Fatalf("torn line should be gone, got %+v", got)
	}
}

func TestSecondWriterIsRefused(t *testing.T) {
	lockWait = 50 * time.Millisecond
	path := filepath.Join(t.TempDir(), "writes.jsonl")
	l, _ := Open(path)
	defer l.Close()
	done := make(chan error)
	go func() {
		_, err := Open(path)
		done <- err
	}()
	if err := <-done; err == nil {
		t.Fatal("second Open must fail while the first holds the lock")
	}
}

func TestRewriteKeepsOnlyGivenEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "writes.jsonl")
	l, _ := Open(path)
	l.Append(Entry{Op: Write, Path: "/p/a", TimeNS: 1})
	l.Append(Entry{Op: Write, Path: "/p/b", TimeNS: 2})
	l.Close()
	if err := Rewrite(path, []Entry{{Op: Write, Path: "/p/b", TimeNS: 2}}); err != nil {
		t.Fatal(err)
	}
	got, _ := Read(path)
	if len(got) != 1 || got[0].Path != "/p/b" {
		t.Fatalf("Rewrite kept %+v", got)
	}
}

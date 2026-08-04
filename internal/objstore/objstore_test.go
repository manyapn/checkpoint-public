package objstore

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Canonical git object ids for known content, computed by git itself
// (`printf "" | git hash-object --stdin`, `printf 'hello\n' | git hash-object
// --stdin`). If our loose-object encoding is byte-for-byte git-compatible, our
// ref must equal these. This is the load-bearing "git-backed" assertion.
const (
	emptyBlobRef = "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391"
	helloBlobRef = "ce013625030ba8dba906f756967f9e9ca394464a"
)

func TestPutRefMatchesGitHashObject(t *testing.T) {
	s := mustOpen(t)
	cases := []struct {
		content []byte
		want    string
	}{
		{[]byte(""), emptyBlobRef},
		{[]byte("hello\n"), helloBlobRef},
	}
	for _, c := range cases {
		ref, _, err := s.Put(c.content)
		if err != nil {
			t.Fatalf("Put(%q): %v", c.content, err)
		}
		if ref != c.want {
			t.Fatalf("Put(%q) ref = %s, want %s (git sha1)", c.content, ref, c.want)
		}
	}
}

func TestPutGetRoundTrip(t *testing.T) {
	s := mustOpen(t)
	for _, content := range [][]byte{
		[]byte(""),
		[]byte("hello\n"),
		[]byte("no trailing newline"),
		bytes.Repeat([]byte{0}, 4096), // binary, NULs
		mustRandom(t, 1<<20),          // 1 MiB
	} {
		ref, _, err := s.Put(content)
		if err != nil {
			t.Fatalf("Put: %v", err)
		}
		got, err := s.Get(ref)
		if err != nil {
			t.Fatalf("Get(%s): %v", ref, err)
		}
		if !bytes.Equal(got, content) {
			t.Fatalf("round-trip mismatch for ref %s: got %d bytes, want %d", ref, len(got), len(content))
		}
	}
}

func TestPutIsIdempotentAndDedups(t *testing.T) {
	s := mustOpen(t)
	content := []byte("dedup me\n")
	ref1, new1, err := s.Put(content)
	if err != nil {
		t.Fatal(err)
	}
	if !new1 {
		t.Fatal("first Put should report isNew=true")
	}
	ref2, new2, err := s.Put(content)
	if err != nil {
		t.Fatal(err)
	}
	if ref2 != ref1 {
		t.Fatalf("same content, different refs: %s vs %s", ref1, ref2)
	}
	if new2 {
		t.Fatal("second Put of identical content should report isNew=false (dedup hit)")
	}
}

func TestOnDiskLayoutIsGitLoose(t *testing.T) {
	s := mustOpen(t)
	ref, _, err := s.Put([]byte("hello\n"))
	if err != nil {
		t.Fatal(err)
	}
	// git loose layout: objects/<first-2>/<remaining-38>
	p := filepath.Join(s.dir, "objects", ref[:2], ref[2:])
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("expected loose object at %s: %v", p, err)
	}
}

func TestGetMissingRefErrors(t *testing.T) {
	s := mustOpen(t)
	if _, err := s.Get(emptyBlobRef); err == nil {
		t.Fatal("Get of absent ref should error")
	}
}

func TestGetRejectsInvalidRef(t *testing.T) {
	s := mustOpen(t)
	for _, bad := range []string{"", "xyz", "E69DE29BB2D1D6434B8B29AE775AD8C2E48C5391", "../etc/passwd"} {
		if _, err := s.Get(bad); err == nil {
			t.Fatalf("Get(%q) should reject invalid ref", bad)
		}
	}
}

func TestGetDetectsCorruption(t *testing.T) {
	s := mustOpen(t)
	ref, _, err := s.Put([]byte("integrity\n"))
	if err != nil {
		t.Fatal(err)
	}
	// Overwrite the object with garbage that is not even valid zlib.
	p := filepath.Join(s.dir, "objects", ref[:2], ref[2:])
	if err := os.WriteFile(p, []byte("not a git object"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ref); err == nil {
		t.Fatal("Get of a corrupted object should error, never return wrong bytes")
	}
}

// TestRealGitCanReadOurObjects proves the objects are genuinely git-format:
// a real `git` reads content back by our ref. Skipped when git is absent.
func TestRealGitCanReadOurObjects(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command(git, args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "--quiet")

	// Our store writes into this repo's real object dir.
	s, err := Open(filepath.Join(repo, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("git-compatible content\n")
	ref, _, err := s.Put(content)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(git, "cat-file", "blob", ref)
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git cat-file blob %s: %v\n%s", ref, err, out)
	}
	if !bytes.Equal(out, content) {
		t.Fatalf("git read back %q, want %q", out, content)
	}
}

func mustOpen(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

func mustRandom(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	// deterministic-ish fill; content value is irrelevant, only round-trip is.
	for i := range b {
		b[i] = byte(i*31 + 7)
	}
	return b
}

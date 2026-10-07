package objects

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestPutGetRoundTrip(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("hello\n")
	ref, err := s.Put(content)
	if err != nil {
		t.Fatal(err)
	}
	// git's own id for "hello\n" as a blob
	if ref != "ce013625030ba8dba906f756967f9e9ca394464a" {
		t.Fatalf("ref = %s, want git's blob id", ref)
	}
	got, err := s.Get(ref)
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("Get = %q, %v", got, err)
	}
	again, _ := s.Put(content)
	if again != ref {
		t.Fatal("same content must give the same ref")
	}
}

func TestGetRejectsCorruptObject(t *testing.T) {
	s, _ := Open(t.TempDir())
	ref, _ := s.Put([]byte("original"))
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	zw.Write([]byte("blob 7\x00tampered"))
	zw.Close()
	os.WriteFile(s.path(ref), buf.Bytes(), 0o600)
	if _, err := s.Get(ref); err == nil {
		t.Fatal("tampered object must not read back as valid")
	}
}

func TestListIgnoresTempFiles(t *testing.T) {
	s, _ := Open(t.TempDir())
	ref, _ := s.Put([]byte("x"))
	os.WriteFile(filepath.Join(s.dir, ref[:2], "tmp-123"), []byte("junk"), 0o600)
	refs, err := s.List()
	if err != nil || len(refs) != 1 || refs[0] != ref {
		t.Fatalf("List = %v, %v", refs, err)
	}
	if err := s.Delete(ref); err != nil {
		t.Fatal(err)
	}
	if s.Has(ref) {
		t.Fatal("deleted ref still present")
	}
}

func TestGitCanReadOurObjects(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	exec.Command(git, "-C", repo, "init", "-q").Run()
	s, _ := Open(filepath.Join(repo, ".git"))
	content := []byte("read me with git\n")
	ref, _ := s.Put(content)
	sum := sha1.Sum(append([]byte("blob 17\x00"), content...))
	if hex.EncodeToString(sum[:]) != ref {
		t.Fatal("ref is not the git blob id")
	}
	out, err := exec.Command(git, "-C", repo, "cat-file", "-p", ref).Output()
	if err != nil || !bytes.Equal(out, content) {
		t.Fatalf("git cat-file = %q, %v", out, err)
	}
}

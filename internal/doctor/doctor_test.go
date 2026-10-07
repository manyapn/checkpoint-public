package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStoreInsideProjectIsFatal(t *testing.T) {
	root := t.TempDir()
	c := storeCheck(root, filepath.Join(root, "store"))
	if c.OK || !c.Fatal || !strings.Contains(c.Detail, "inside") {
		t.Fatalf("%+v", c)
	}
	if c := storeCheck(root, filepath.Join(t.TempDir(), "store")); !c.OK {
		t.Fatalf("%+v", c)
	}
}

func TestSymlinkedWorkspaceIsRefused(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	os.Symlink(real, link)
	if c := workspaceCheck(link); c.OK || !strings.Contains(c.Remedy, real) {
		t.Fatalf("%+v", c)
	}
}

func TestReportVerdictFollowsWorstCheck(t *testing.T) {
	r := Report{Checks: []Check{{Name: "a", OK: true}, {Name: "b", OK: false, Fatal: false, Detail: "d", Remedy: "r"}}}
	if !r.Healthy() || !strings.Contains(r.Text(), "1 limitation") {
		t.Fatal(r.Text())
	}
	r.Checks[1].Fatal = true
	if r.Healthy() || !strings.Contains(r.Text(), "cannot run") {
		t.Fatal(r.Text())
	}
}

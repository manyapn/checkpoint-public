package selftest

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The whole product, end to end, through the real binary.
func TestGuaranteesHoldHere(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root for fanotify")
	}
	bin := filepath.Join(t.TempDir(), "checkpoint")
	build := exec.Command("go", "build", "-o", bin, "github.com/manyapn/checkpoint-public/cmd/checkpoint")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	rep := Run(bin, t.TempDir(), testing.Verbose())
	t.Log("\n" + rep.Text())
	if !rep.OK() {
		t.Fatal("a guarantee failed")
	}
}

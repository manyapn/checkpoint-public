package lineage

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestParseStatSurvivesHostileComm(t *testing.T) {
	start, ppid, ok := parseStat("123 (a b) c) S 77 1 1 0 -1 4194560 0 0 0 0 0 0 0 0 20 0 1 0 999 0 0")
	if !ok || ppid != 77 || start != 999 {
		t.Fatalf("got %d %d %v", start, ppid, ok)
	}
	if _, _, ok := parseStat("garbage"); ok {
		t.Fatal("garbage must not parse")
	}
}

func TestLineageOfOwnChildReachesUs(t *testing.T) {
	tr := NewTracker()
	stop := make(chan struct{})
	defer close(stop)
	go tr.Run(stop)
	cmd := exec.Command("sleep", "5")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	meStart, _ := StartTime(os.Getpid())
	me := Identity{os.Getpid(), meStart}
	roots := map[Identity]bool{me: true}
	chain, ok := tr.Lineage(cmd.Process.Pid, roots)
	if !ok || len(chain) != 2 || chain[1] != me {
		t.Fatalf("chain = %v, %v", chain, ok)
	}
	if got := tr.Who(cmd.Process.Pid, roots); got != Agent {
		t.Fatalf("child of a root should be Agent, got %s", got)
	}
	if got := tr.Who(1, roots); got != Human {
		t.Fatalf("init is not under the root, got %s", got)
	}
}

func TestReapedOneShotChildStillResolves(t *testing.T) {
	tr := NewTracker()
	stop := make(chan struct{})
	defer close(stop)
	go tr.Run(stop)
	tr.SetActive(true)
	time.Sleep(50 * time.Millisecond)
	meStart, _ := StartTime(os.Getpid())
	roots := map[Identity]bool{{os.Getpid(), meStart}: true}
	cmd := exec.Command("true")
	cmd.Start()
	pid := cmd.Process.Pid
	cmd.Wait()
	if got := tr.Who(pid, roots); got != Agent {
		t.Fatalf("reaped child should still classify as Agent via the cache, got %s", got)
	}
}

func TestUnknownPidIsUnknown(t *testing.T) {
	tr := NewTracker()
	if got := tr.Who(999999, nil); got != Unknown {
		t.Fatal(got)
	}
}

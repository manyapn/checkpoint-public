//go:build linux

package provenance

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

func selfIdentity(t *testing.T) Identity {
	t.Helper()
	start, ok := StartTime(os.Getpid())
	if !ok {
		t.Fatal("could not read own start-time")
	}
	return Identity{Pid: os.Getpid(), Start: start}
}

// TestTrackerResolvesSelfAsAgent: with this process registered as the agent root,
// a write it makes resolves to Agent.
func TestTrackerResolvesSelfAsAgent(t *testing.T) {
	tr := NewTracker()
	self := selfIdentity(t)
	chain, resolved := tr.Lineage(os.Getpid(), map[Identity]bool{self: true})
	if !resolved {
		t.Fatal("lineage of self should resolve")
	}
	if chain[0] != self {
		t.Fatalf("chain should start at self %v, got %v", self, chain[0])
	}
	if got := Classify(chain, resolved, map[Identity]bool{self: true}); got != Agent {
		t.Fatalf("self registered as agent root must classify Agent, got %s", got)
	}
}

// TestTrackerChildOfAgentRootIsAgent: a child process (descendant of the
// registered agent root, i.e. this test process) classifies Agent.
func TestTrackerChildOfAgentRootIsAgent(t *testing.T) {
	tr := NewTracker()
	self := selfIdentity(t)

	cmd := exec.Command("sleep", "5")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()

	chain, resolved := tr.Lineage(cmd.Process.Pid, map[Identity]bool{self: true})
	if !resolved {
		t.Fatalf("lineage of child pid %d should resolve", cmd.Process.Pid)
	}
	if got := Classify(chain, resolved, map[Identity]bool{self: true}); got != Agent {
		t.Fatalf("a descendant of the agent root must be Agent, got %s (chain %v)", got, chain)
	}
}

// TestTrackerNonDescendantIsHuman: with an UNRELATED pid registered as the agent
// root, this process's write is Human: it does not descend from that root.
func TestTrackerNonDescendantIsHuman(t *testing.T) {
	tr := NewTracker()

	// A registered "agent" that is not an ancestor of this test process.
	other := exec.Command("sleep", "5")
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { other.Process.Kill(); other.Wait() }()
	otherStart, ok := StartTime(other.Process.Pid)
	if !ok {
		t.Fatal("could not read other's start-time")
	}
	agentRoots := map[Identity]bool{{Pid: other.Process.Pid, Start: otherStart}: true}

	chain, resolved := tr.Lineage(os.Getpid(), agentRoots)
	if !resolved {
		t.Fatal("lineage of self should resolve")
	}
	if got := Classify(chain, resolved, agentRoots); got != Human {
		t.Fatalf("a write not descended from the registered agent root must be Human, got %s", got)
	}
}

// TestScannerCachesBirthParent: the scan loop caches a newborn's parent so a
// short-lived process is still resolvable via the cache. Best-effort timing.
func TestScannerCachesBirthParent(t *testing.T) {
	tr := NewTracker()
	stop := make(chan struct{})
	go tr.Run(stop)
	defer close(stop)
	time.Sleep(50 * time.Millisecond) // let the scanner make a pass

	self := selfIdentity(t)
	chain, resolved := tr.Lineage(os.Getpid(), map[Identity]bool{self: true})
	if !resolved || Classify(chain, resolved, map[Identity]bool{self: true}) != Agent {
		t.Fatalf("self should resolve to Agent with the scanner running (chain %v resolved %v)", chain, resolved)
	}
}

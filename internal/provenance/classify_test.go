package provenance

import "testing"

// Stable identities used across the cases. Two processes may share a pid at
// different times; the Start disambiguates them.
var (
	root   = Identity{Pid: 100, Start: 5000} // the registered agent root
	child  = Identity{Pid: 101, Start: 5001}
	gkid   = Identity{Pid: 102, Start: 5002}
	human  = Identity{Pid: 200, Start: 6000}
	editor = Identity{Pid: 201, Start: 6001}
	init1  = Identity{Pid: 1, Start: 1}
)

func agentSet(ids ...Identity) map[Identity]bool {
	m := map[Identity]bool{}
	for _, id := range ids {
		m[id] = true
	}
	return m
}

func TestClassifyAgentDescendant(t *testing.T) {
	// grandchild -> child -> root(agent). Terminates AT the agent root.
	chain := []Identity{gkid, child, root}
	if got := Classify(chain, true, agentSet(root)); got != Agent {
		t.Fatalf("descendant of the agent root must be Agent, got %s", got)
	}
}

func TestClassifyDirectAgentRootWrite(t *testing.T) {
	chain := []Identity{root}
	if got := Classify(chain, true, agentSet(root)); got != Agent {
		t.Fatalf("the agent root itself writing must be Agent, got %s", got)
	}
}

func TestClassifyConcurrentHumanEdit(t *testing.T) {
	// A human edit DURING the agent window: its lineage reaches init, never the
	// agent root. Boundary membership is irrelevant: this must be Human.
	chain := []Identity{editor, human, init1}
	if got := Classify(chain, true, agentSet(root)); got != Human {
		t.Fatalf("a write not descended from the agent root must be Human, got %s", got)
	}
}

func TestClassifyUnrelatedProcessIsHuman(t *testing.T) {
	chain := []Identity{human, init1}
	if got := Classify(chain, true, agentSet(root)); got != Human {
		t.Fatalf("an unrelated process's write must be Human, got %s", got)
	}
}

func TestClassifySessionRootTerminalIsHuman(t *testing.T) {
	// A container session leader (walk stopped at ppid 0) that is not the agent
	// root: a complete, provably-outside chain -> Human.
	sessionLeader := Identity{Pid: 300, Start: 7000}
	chain := []Identity{editor, sessionLeader}
	if got := Classify(chain, true, agentSet(root)); got != Human {
		t.Fatalf("a non-agent session-root terminal must be Human, got %s", got)
	}
}

func TestClassifyUnresolvedIsUnknown(t *testing.T) {
	// The walk died mid-chain (a raced/reaped one-shot). Uncertainty preserved.
	chain := []Identity{gkid, child}
	if got := Classify(chain, false, agentSet(root)); got != Unknown {
		t.Fatalf("an unresolved chain must be Unknown, never Agent, got %s", got)
	}
}

func TestClassifyEmptyIsUnknown(t *testing.T) {
	if got := Classify(nil, true, agentSet(root)); got != Unknown {
		t.Fatalf("an empty chain must be Unknown, got %s", got)
	}
}

func TestClassifyDuplicateInChainIsUnknown(t *testing.T) {
	// A pid appearing twice is an impossible ppid walk -> malformed -> Unknown.
	chain := []Identity{gkid, child, gkid, root}
	if got := Classify(chain, true, agentSet(root)); got != Unknown {
		t.Fatalf("a duplicate in the chain must be Unknown, got %s", got)
	}
}

func TestClassifyNonPositivePidIsUnknown(t *testing.T) {
	chain := []Identity{{Pid: 0, Start: 1}, root}
	if got := Classify(chain, true, agentSet(root)); got != Unknown {
		t.Fatalf("a non-positive pid in the chain must be Unknown, got %s", got)
	}
}

// TestClassifyPidReuseDoesNotGrantAgency is the whole point of stable identity:
// a chain terminating at the agent root's PID but with a DIFFERENT start-time is
// a recycled pid, not the agent, so it must not be credited as Agent.
func TestClassifyPidReuseDoesNotGrantAgency(t *testing.T) {
	recycled := Identity{Pid: root.Pid, Start: root.Start + 999} // same pid, later start
	chain := []Identity{child, recycled}
	if got := Classify(chain, true, agentSet(root)); got == Agent {
		t.Fatal("a recycled root pid (different start) must NOT be Agent")
	}
}

func TestClassifyNoAgentRootsIsHuman(t *testing.T) {
	// No agent registered (e.g. a bare edit outside any run): a clean chain is
	// Human, not Unknown.
	chain := []Identity{editor, init1}
	if got := Classify(chain, true, nil); got != Human {
		t.Fatalf("with no agent roots a clean chain is Human, got %s", got)
	}
}

// TestSelfClassString pins the wire value the ledger and undo agree on: undo
// matches writers by this string, so renaming it silently breaks the
// resumable-undo contract.
func TestSelfClassString(t *testing.T) {
	if Self.String() != "checkpoint" {
		t.Fatalf("Self must serialize as %q (undo matches on it), got %q", "checkpoint", Self.String())
	}
	if Self == Unknown || Self == Agent || Self == Human {
		t.Fatal("Self must be a distinct class")
	}
}

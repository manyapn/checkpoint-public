// Package provenance is where checkpoint's per-file authorship comes from: it
// records, for every captured write, the stable identity of the writing process
// and its lineage, and classifies the write as agent, human, or unknown,
// preserving uncertainty honestly. Authorship lands on the individual write
// rather than on a checkpoint as a whole, which is what lets one turn hold the
// agent's work and yours and still be told apart afterwards. Processes are keyed
// by stable identity rather than by bare pid, so a recycled pid cannot inherit
// agent credit.
//
// Attribution is by PROCESS LINEAGE, never by which boundary a write fell in:
// boundary ids group operations, but a write is agent only if its process chain
// provably terminates at a registered agent root. A concurrent human edit during
// an agent turn is Human, not Agent.
package provenance

// Identity is a stable process identity: a pid plus its start-time (from
// /proc/<pid>/stat field 22, in clock ticks since boot). The pair survives pid
// reuse: two processes that share a pid at different times differ in Start.
type Identity struct {
	Pid   int    `json:"pid"`
	Start uint64 `json:"start"`
}

// Class is the attribution verdict for a single write.
type Class int

const (
	// Unknown: lineage missing, unresolved, or malformed. The zero value, and
	// NEVER treated as agent; uncertainty is preserved, not guessed away.
	Unknown Class = iota
	// Agent: the writer's chain is internally consistent and terminates exactly
	// at a registered agent root.
	Agent
	// Human: a complete, internally-consistent chain that terminates outside
	// every agent root (at init or a session root), so provably not the agent.
	Human
	// Self: checkpoint's OWN write, made by the checkpoint binary while
	// restoring or undoing. It is neither the agent's change nor the human's,
	// and treating it as either corrupts the next decision: as "human" it
	// conflicts the very file just reverted (so a second `undo --only` refuses
	// it), and as "agent" a later undo would revert the restore itself. Undo
	// ignores Self writes entirely.
	Self
)

func (c Class) String() string {
	switch c {
	case Agent:
		return "agent"
	case Human:
		return "human"
	case Self:
		return "checkpoint"
	default:
		return "unknown"
	}
}

// Classify decides who made a write. chain is the writer's lineage, writer-first,
// each entry a stable Identity; resolved reports whether the lineage walk reached
// a clean terminal (an agent root, init, or a session root). A walk that died
// mid-chain is not resolved. agentRoots is the set of currently-registered agent
// roots (may be nil).
//
// The rule is deliberately strict: a chain is Agent only if it is internally
// consistent (positive pids, and no duplicate identities, which would mean an
// impossible ppid walk) AND its terminal is exactly an agent root. A resolved,
// consistent chain whose terminal is not an agent root is Human. Anything else,
// whether unresolved, empty, or malformed, is Unknown. A recorder bug can only
// mislabel toward Human (a skipped undo), never toward Agent (a reverted human
// file).
func Classify(chain []Identity, resolved bool, agentRoots map[Identity]bool) Class {
	if !resolved || len(chain) == 0 {
		return Unknown
	}
	for i, id := range chain {
		if id.Pid <= 0 {
			return Unknown // no valid walk contains a non-positive pid
		}
		for _, prev := range chain[:i] {
			if prev == id {
				return Unknown // duplicate identity: impossible ppid walk
			}
		}
		// An agent root may only appear as the terminal: the lineage walk stops
		// at the first agent root it meets, so a root seen mid-chain means the
		// chain was assembled wrong.
		if i < len(chain)-1 && agentRoots[id] {
			return Unknown
		}
	}
	if agentRoots[chain[len(chain)-1]] {
		return Agent
	}
	return Human
}

// Package lineage decides who made a write: the agent, the human, checkpoint
// itself, or unknown. A write is the agent's only if the writing process
// descends from a registered agent process; a human edit made while the
// agent runs stays the human's.
package lineage

const (
	Agent   = "agent"
	Human   = "human"
	Self    = "self"
	Unknown = "unknown"
)

// Identity is a pid plus its start time, so a recycled pid is a different
// process.
type Identity struct {
	Pid   int    `json:"pid"`
	Start uint64 `json:"start"`
}

// Classify reads a chain (writer first, ending at an agent root, init, or a
// session root). An unresolved or malformed chain is Unknown, never Agent:
// a mistake must under-revert, not touch a human file.
func Classify(chain []Identity, resolved bool, roots map[Identity]bool) string {
	if !resolved || len(chain) == 0 {
		return Unknown
	}
	seen := map[Identity]bool{}
	for i, id := range chain {
		if id.Pid <= 0 || seen[id] || (roots[id] && i != len(chain)-1) {
			return Unknown
		}
		seen[id] = true
	}
	if roots[chain[len(chain)-1]] {
		return Agent
	}
	return Human
}

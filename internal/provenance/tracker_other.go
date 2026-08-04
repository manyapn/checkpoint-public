//go:build !linux

package provenance

// Non-Linux stub: lineage resolution needs /proc. On a dev host the pure
// Classify still runs (and its unit tests pass), but a live Tracker resolves
// nothing: every write would classify Unknown, which is the safe direction. The
// product is Linux-only.

type Tracker struct{}

func NewTracker() *Tracker { return &Tracker{} }

func (t *Tracker) Run(stop <-chan struct{}) {}

func (t *Tracker) SetActive(active bool) {}

func (t *Tracker) StartTime(pid int) (uint64, bool) { return 0, false }

func (t *Tracker) Lineage(pid int, agentRoots map[Identity]bool) ([]Identity, bool) {
	return nil, false
}

// StartTime resolves a pid's start-time (used by `run`). Unavailable off Linux.
func StartTime(pid int) (uint64, bool) { return 0, false }

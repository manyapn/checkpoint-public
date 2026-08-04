// Package status grades how completely a recorded checkpoint restores, mapping
// its internal coverage to the badge shown beside it in the history. Worst level
// wins.
//
//	Fully recoverable:           every file and folder restores (Durable, no loss).
//	Recoverable with exceptions: restores except specific, counted items; the rest
//	                             is intact (Durable + bounded, per-file misses).
//	Incomplete:                  a gap was detected but cannot be bounded. We know
//	                             something was lost but not what (Partial: a
//	                             fanotify queue overflow or an unreadable event).
package status

import "github.com/manyapn/checkpoint-public/internal/store"

// Badge is the graded per-checkpoint recovery status shown to users.
type Badge int

const (
	Fully Badge = iota
	RecoverableWithExceptions
	Incomplete
)

func (b Badge) String() string {
	switch b {
	case Fully:
		return "Fully recoverable"
	case RecoverableWithExceptions:
		return "Recoverable with exceptions"
	default:
		return "Incomplete"
	}
}

// Of grades a manifest. PARTIAL coverage (unbounded, detected loss) is Incomplete
// and dominates. A DURABLE manifest is Fully recoverable, or Recoverable-with-
// exceptions when it carries bounded loss: per-file misses (Missed > 0) or named
// exceptions (unreadable / unsupported / uncaptured items, listed by path).
func Of(m *store.Manifest) Badge {
	if m.Coverage == store.PARTIAL {
		return Incomplete
	}
	if m.Missed > 0 || len(m.Exceptions) > 0 {
		return RecoverableWithExceptions
	}
	return Fully
}

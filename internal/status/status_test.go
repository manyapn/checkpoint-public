package status

import (
	"testing"

	"github.com/manyapn/checkpoint-public/internal/store"
)

func TestFullyRecoverable(t *testing.T) {
	m := &store.Manifest{Coverage: store.DURABLE, Missed: 0}
	if b := Of(m); b != Fully {
		t.Fatalf("durable + no misses must be Fully, got %s", b)
	}
}

func TestRecoverableWithExceptions(t *testing.T) {
	m := &store.Manifest{Coverage: store.DURABLE, Missed: 3}
	if b := Of(m); b != RecoverableWithExceptions {
		t.Fatalf("durable + bounded misses must be Recoverable-with-exceptions, got %s", b)
	}
}

func TestNamedExceptionsGrade(t *testing.T) {
	// A named exception (unreadable / unsupported / uncaptured item) is bounded
	// loss even when the miss counter is zero.
	m := &store.Manifest{Coverage: store.DURABLE, Missed: 0,
		Exceptions: []store.Exception{{Path: "pipe", Reason: "unsupported kind: p"}}}
	if b := Of(m); b != RecoverableWithExceptions {
		t.Fatalf("durable + named exceptions must be Recoverable-with-exceptions, got %s", b)
	}
}

func TestIncomplete(t *testing.T) {
	m := &store.Manifest{Coverage: store.PARTIAL, Missed: 0}
	if b := Of(m); b != Incomplete {
		t.Fatalf("partial (overflow) must be Incomplete, got %s", b)
	}
}

func TestIncompleteWinsOverExceptions(t *testing.T) {
	// Unbounded loss dominates: worst level wins.
	m := &store.Manifest{Coverage: store.PARTIAL, Missed: 5}
	if b := Of(m); b != Incomplete {
		t.Fatalf("partial must be Incomplete regardless of missed count, got %s", b)
	}
}

func TestBadgeStrings(t *testing.T) {
	cases := map[Badge]string{
		Fully:                     "Fully recoverable",
		RecoverableWithExceptions: "Recoverable with exceptions",
		Incomplete:                "Incomplete",
	}
	for b, want := range cases {
		if b.String() != want {
			t.Fatalf("badge %d string = %q, want %q", b, b.String(), want)
		}
	}
}

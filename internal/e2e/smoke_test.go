//go:build linux

package e2e

import "testing"

// TestHarnessSmoke proves the harness itself works before other files rely on
// it: build the real binary, protect a workspace, see the setup checkpoint.
func TestHarnessSmoke(t *testing.T) {
	e := newEnv(t)
	e.Write("a.txt", "hello\n")
	e.Protect()
	st := e.StatusJSON()
	if !st.Protected || st.Root != e.WS {
		t.Fatalf("protected status wrong: %+v", st)
	}
	if len(e.HistoryJSON()) < 1 {
		t.Fatal("setup checkpoint missing")
	}
}

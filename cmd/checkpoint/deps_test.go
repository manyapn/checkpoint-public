package main

import (
	"os/exec"
	"strings"
	"testing"
)

// TestCLIDoesNotLinkTheTerminalUI pins why checkpoint-ui is a separate binary.
// Bubble Tea's package init calls lipgloss.HasDarkBackground(), which asks the
// terminal for its background colour and waits out termenv's five second
// timeout when nothing answers. Linking it here puts that delay in front of
// EVERY command, including `checkpoint doctor`, whose whole job is to answer
// quickly. Measured on a pty that does not answer the query: 6.4s for doctor
// with the UI linked in, 2.1s without it, and `version` went from about five
// seconds to ten milliseconds.
//
// Terminals that do not answer OSC 11 are ordinary, not exotic: CI logs,
// script(1), some editor terminals, and tmux without passthrough.
func TestCLIDoesNotLinkTheTerminalUI(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	for _, forbidden := range []string{
		"github.com/charmbracelet/bubbletea",
		"github.com/charmbracelet/lipgloss",
		"github.com/muesli/termenv",
		"github.com/manyapn/checkpoint-public/internal/tui",
	} {
		if strings.Contains(string(out), forbidden) {
			t.Errorf("the checkpoint binary links %s, which costs every command a terminal query;\n"+
				"the interactive screen belongs in cmd/checkpoint-ui, which `checkpoint ui` launches", forbidden)
		}
	}
}

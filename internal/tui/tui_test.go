package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestViewRendersRowsAndSelection(t *testing.T) {
	m := New("checkpoint", "/p", "/s")
	next, _ := m.Update(loaded{
		status: status{Protected: true},
		rows: []row{
			{ID: 1, Badge: "Fully recoverable", Source: "run: agent"},
			{ID: 0, Badge: "Recoverable with exceptions", Source: "setup",
				Exceptions: []struct {
					Path   string `json:"path"`
					Reason string `json:"reason"`
				}{{".env", "not captured"}}},
		},
	})
	m = next.(Model)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	view := next.(Model).View()
	if !strings.Contains(view, "Protection: on") || !strings.Contains(view, "> #0") || !strings.Contains(view, "! .env") {
		t.Fatalf("view:\n%s", view)
	}
}

func TestQuitKey(t *testing.T) {
	_, cmd := New("c", "/p", "/s").Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd == nil {
		t.Fatal("q should quit")
	}
}

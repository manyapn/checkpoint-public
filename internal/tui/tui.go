// Package tui is a terminal view over `checkpoint status --json` and
// `history --json`. It can show nothing the JSON cannot, which keeps the
// JSON contract honest.
package tui

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

type status struct {
	Protected        bool     `json:"protected"`
	Root             string   `json:"root"`
	Store            string   `json:"store"`
	Checkpoints      int      `json:"checkpoints"`
	LastCheckpointNS int64    `json:"last_checkpoint_ns"`
	FeedActive       bool     `json:"feed_active"`
	AgentSessions    int      `json:"agent_sessions"`
	Missed           []string `json:"missed"`
	Overflowed       bool     `json:"overflowed"`
	OutsideCount     int      `json:"outside_count"`
}

type row struct {
	ID         int    `json:"id"`
	TimeNS     int64  `json:"time_ns"`
	Badge      string `json:"badge"`
	Source     string `json:"source"`
	Name       string `json:"name"`
	Exceptions []struct {
		Path   string `json:"path"`
		Reason string `json:"reason"`
	} `json:"exceptions"`
}

type Model struct {
	cli, root, store string
	status           status
	rows             []row
	selected         int
	err              error
}

func New(cli, root, store string) Model { return Model{cli: cli, root: root, store: store} }

type loaded struct {
	status status
	rows   []row
	err    error
}

func (m Model) load() tea.Msg {
	var l loaded
	l.err = m.query("status", &l.status)
	var h struct {
		Checkpoints []row `json:"checkpoints"`
	}
	if err := m.query("history", &h); err != nil {
		l.err = err
	}
	l.rows = h.Checkpoints
	return l
}

func (m Model) query(cmd string, out any) error {
	b, err := exec.Command(m.cli, cmd, "--root", m.root, "--store", m.store, "--json").Output()
	if err != nil {
		return fmt.Errorf("%s --json: %w", cmd, err)
	}
	return json.Unmarshal(b, out)
}

func (m Model) Init() tea.Cmd { return m.load }

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case loaded:
		m.status, m.rows, m.err = msg.status, msg.rows, msg.err
		if m.selected >= len(m.rows) {
			m.selected = 0
		}
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "r":
			return m, m.load
		case "j", "down":
			if m.selected < len(m.rows)-1 {
				m.selected++
			}
		case "k", "up":
			if m.selected > 0 {
				m.selected--
			}
		}
	}
	return m, nil
}

func (m Model) View() string {
	var b strings.Builder
	switch {
	case m.err != nil:
		fmt.Fprintf(&b, "error: %v\n", m.err)
	case !m.status.Protected:
		b.WriteString("Protection: not running\n")
	case m.status.Overflowed || len(m.status.Missed) > 0 || m.status.OutsideCount > 0:
		b.WriteString("Protection: limited (see checkpoint status)\n")
	default:
		b.WriteString("Protection: on\n")
	}
	fmt.Fprintf(&b, "Root: %s\nStore: %s\nLast checkpoint: %s\n\n", m.root, m.store, ago(m.status.LastCheckpointNS))
	if len(m.rows) == 0 {
		b.WriteString("no checkpoints yet\n")
	}
	for i, r := range m.rows {
		cursor := "  "
		if i == m.selected {
			cursor = "> "
		}
		label := r.Source
		if r.Name != "" {
			label += fmt.Sprintf("  name:%q", r.Name)
		}
		fmt.Fprintf(&b, "%s#%-3d %s  %-28s %s\n", cursor, r.ID, time.Unix(0, r.TimeNS).Format("2006-01-02 15:04:05"), r.Badge, label)
		if i == m.selected {
			for _, ex := range r.Exceptions {
				fmt.Fprintf(&b, "        ! %s (%s)\n", ex.Path, ex.Reason)
			}
		}
	}
	b.WriteString("\nj/k move   r refresh   q quit\n")
	return b.String()
}

func ago(ns int64) string {
	if ns <= 0 {
		return "none yet"
	}
	return time.Since(time.Unix(0, ns)).Round(time.Second).String() + " ago"
}

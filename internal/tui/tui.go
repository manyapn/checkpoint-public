// Package tui is the terminal UI over the checkpoint CLI's --json output.
// It is a PURE CLIENT: it must not import any github.com/manyapn/checkpoint-public/internal/*
// package (TestPureClientImports enforces this). Everything it knows arrives
// as JSON through the Client interface, exactly what a third-party UI could
// build from the documented CLI contract (`status --json`, `history --json`,
// `undo`). Keeping the UI on the same contract as any other client is what
// stops the JSON from quietly becoming insufficient.
package tui

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// Disclaimer states the recoverability boundary (what checkpoint can and
// cannot put back) and is rendered on the undo confirm surface, where the user
// is about to rely on it.
const Disclaimer = "note: checkpoint restores captured filesystem changes only. " +
	"external side effects (network, db migrations, package-manager global state, " +
	"running processes) are not reversed."

// Client supplies the CLI's --json output and triggers undo. The production
// implementation shells out to the checkpoint binary.
type Client interface {
	HistoryJSON() ([]byte, error)
	StatusJSON() ([]byte, error)
	Undo(saveBoth bool) (out string, err error)
}

// --- production client: shells out to the real binary ----------------------

type cliClient struct {
	bin, root, store string
}

// NewCLIClient returns a Client that runs binPath with --root/--store flags
// (either may be "" to let the CLI use its defaults).
func NewCLIClient(binPath, root, storeDir string) Client {
	return &cliClient{bin: binPath, root: root, store: storeDir}
}

func (c *cliClient) args(sub string, extra ...string) []string {
	a := []string{sub}
	if c.root != "" {
		a = append(a, "--root", c.root)
	}
	if c.store != "" {
		a = append(a, "--store", c.store)
	}
	return append(a, extra...)
}

func (c *cliClient) HistoryJSON() ([]byte, error) {
	return exec.Command(c.bin, c.args("history", "--json")...).Output()
}

func (c *cliClient) StatusJSON() ([]byte, error) {
	return exec.Command(c.bin, c.args("status", "--json")...).Output()
}

// Undo runs the CLI undo and returns its combined output verbatim. On
// failure (e.g. the conflict-floor abort) the output names the files, so it
// is returned alongside the error rather than swallowed.
func (c *cliClient) Undo(saveBoth bool) (string, error) {
	var extra []string
	if saveBoth {
		extra = append(extra, "--save-both")
	}
	out, err := exec.Command(c.bin, c.args("undo", extra...)...).CombinedOutput()
	return string(out), err
}

// --- JSON shapes (the CLI's documented client contract; parsed locally) ----

type exception struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

type ckptRow struct {
	ID             int         `json:"id"`
	TimeNS         int64       `json:"time_ns"`
	Badge          string      `json:"badge"`
	Source         string      `json:"source"`
	Name           string      `json:"name"`
	SettleTimedOut bool        `json:"settle_timed_out"`
	Missed         int         `json:"missed"`
	Exceptions     []exception `json:"exceptions"`
}

type historyPayload struct {
	Checkpoints []ckptRow `json:"checkpoints"`
}

type statusPayload struct {
	Protected        bool     `json:"protected"`
	Root             string   `json:"root"`
	ProtectedDirs    []string `json:"protected_dirs"`
	Checkpoints      int      `json:"checkpoints"`
	Limited          bool     `json:"limited"`
	Missed           int      `json:"missed"`
	Overflowed       bool     `json:"overflowed"`
	AgentSessions    int      `json:"agent_sessions"`
	SettingUp        bool     `json:"setting_up"`
	LastCkptNS       int64    `json:"last_ckpt_ns"`
	FeedActive       bool     `json:"feed_active"`
	BaselineComplete bool     `json:"baseline_complete"`
	Outside          []string `json:"outside"`
	OutsideCount     int      `json:"outside_count"`
}

// --- model -----------------------------------------------------------------

type screen int

const (
	screenTimeline screen = iota
	screenDetail
	screenConfirm
	screenResult
)

// Model is the bubbletea model. Construct with New.
type Model struct {
	client Client

	screen   screen
	status   *statusPayload
	rows     []ckptRow
	cursor   int
	saveBoth bool
	result   string
	err      error
}

func New(client Client) Model {
	return Model{client: client}
}

// Run starts the program loop over the given client.
func Run(c Client) error {
	_, err := tea.NewProgram(New(c), tea.WithAltScreen()).Run()
	return err
}

// Init loads status + history.
func (m Model) Init() tea.Cmd {
	return m.reload
}

type loadedMsg struct {
	status statusPayload
	rows   []ckptRow
}
type undoDoneMsg struct {
	out string
	err error
}
type errMsg struct{ err error }

func (m Model) reload() tea.Msg {
	sb, err := m.client.StatusJSON()
	if err != nil {
		return errMsg{fmt.Errorf("status: %w", err)}
	}
	var st statusPayload
	if err := json.Unmarshal(sb, &st); err != nil {
		return errMsg{fmt.Errorf("status json: %w", err)}
	}
	hb, err := m.client.HistoryJSON()
	if err != nil {
		return errMsg{fmt.Errorf("history: %w", err)}
	}
	var h historyPayload
	if err := json.Unmarshal(hb, &h); err != nil {
		return errMsg{fmt.Errorf("history json: %w", err)}
	}
	return loadedMsg{status: st, rows: h.Checkpoints}
}

func (m Model) runUndo() tea.Cmd {
	saveBoth := m.saveBoth
	return func() tea.Msg {
		out, err := m.client.Undo(saveBoth)
		return undoDoneMsg{out: out, err: err}
	}
}

// Update handles messages. Pure: no I/O outside returned commands.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case loadedMsg:
		m.status = &msg.status
		m.rows = msg.rows
		if m.cursor >= len(m.rows) {
			m.cursor = max(0, len(m.rows)-1)
		}
		return m, nil
	case undoDoneMsg:
		m.result = msg.out
		m.err = msg.err
		m.screen = screenResult
		return m, m.reload
	case errMsg:
		m.err = msg.err
		m.result = ""
		m.screen = screenResult
		return m, nil
	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m Model) handleKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := k.String()
	if key == "ctrl+c" {
		return m, tea.Quit
	}
	switch m.screen {
	case screenTimeline:
		switch key {
		case "q", "esc":
			return m, tea.Quit
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.rows)-1 {
				m.cursor++
			}
		case "enter":
			if len(m.rows) > 0 {
				m.screen = screenDetail
			}
		case "u":
			if len(m.rows) > 0 {
				m.saveBoth = false
				m.screen = screenConfirm
			}
		case "r":
			return m, m.reload
		}
	case screenDetail:
		switch key {
		case "q", "esc":
			m.screen = screenTimeline
		case "u":
			m.saveBoth = false
			m.screen = screenConfirm
		}
	case screenConfirm:
		switch key {
		case "s":
			m.saveBoth = !m.saveBoth
		case "y", "enter":
			return m, m.runUndo()
		case "n", "q", "esc":
			m.screen = screenTimeline
		}
	case screenResult:
		switch key {
		case "q", "esc", "enter":
			m.err = nil
			m.result = ""
			m.screen = screenTimeline
		}
	}
	return m, nil
}

// --- view ------------------------------------------------------------------

// View renders the current screen as plain text.
func (m Model) View() string {
	var b strings.Builder
	switch m.screen {
	case screenTimeline:
		m.viewHeader(&b)
		b.WriteString("\ncheckpoints (enter: detail, u: undo latest turn, r: refresh, q: quit)\n\n")
		if len(m.rows) == 0 {
			b.WriteString("  no checkpoints yet\n")
		}
		for i, r := range m.rows {
			cur := "  "
			if i == m.cursor {
				cur = "> "
			}
			b.WriteString(cur + rowLine(r) + "\n")
		}
	case screenDetail:
		if m.cursor >= len(m.rows) {
			b.WriteString("no checkpoint selected\n")
			break
		}
		r := m.rows[m.cursor]
		fmt.Fprintf(&b, "checkpoint #%d detail (q: back, u: undo latest turn)\n\n", r.ID)
		fmt.Fprintf(&b, "Time:   %s\n", time.Unix(0, r.TimeNS).Format("2006-01-02 15:04:05"))
		fmt.Fprintf(&b, "Badge:  %s\n", r.Badge)
		src := r.Source
		if src == "" {
			src = "(unlabeled)"
		}
		fmt.Fprintf(&b, "Source: %s\n", src)
		if r.Name != "" {
			fmt.Fprintf(&b, "Name:   %q\n", r.Name)
		}
		if r.SettleTimedOut {
			b.WriteString("Note:   settle timed out; a writer was still active when this checkpoint was cut\n")
		}
		if r.Missed > 0 {
			fmt.Fprintf(&b, "Missed: %d file(s) uncaptured in this window\n", r.Missed)
		}
		if len(r.Exceptions) > 0 {
			b.WriteString("Exceptions (named items this checkpoint does not cover):\n")
			for _, ex := range r.Exceptions {
				fmt.Fprintf(&b, "  ! %s (%s)\n", ex.Path, ex.Reason)
			}
		} else {
			b.WriteString("Exceptions: none\n")
		}
	case screenConfirm:
		b.WriteString("undo the agent's latest turn? [y/n]\n\n")
		sb := "OFF"
		if m.saveBoth {
			sb = "ON"
		}
		fmt.Fprintf(&b, "save-both: %s (press 's' to toggle; conflicts save the checkpoint side alongside the live file)\n\n", sb)
		b.WriteString(Disclaimer + "\n")
	case screenResult:
		if m.result != "" {
			b.WriteString(m.result)
			if !strings.HasSuffix(m.result, "\n") {
				b.WriteString("\n")
			}
		}
		if m.err != nil {
			fmt.Fprintf(&b, "error: %v\n", m.err)
		}
		b.WriteString("\n(press enter to return)\n")
	}
	return b.String()
}

func rowLine(r ckptRow) string {
	when := time.Unix(0, r.TimeNS).Format("2006-01-02 15:04:05")
	src := r.Source
	if src == "" {
		src = "(unlabeled)"
	}
	line := fmt.Sprintf("#%-3d %s  %-28s  %s", r.ID, when, r.Badge, src)
	if r.Name != "" {
		line += fmt.Sprintf("  name:%q", r.Name)
	}
	if r.SettleTimedOut {
		line += "  [settle timed out]"
	}
	if r.Missed > 0 {
		line += fmt.Sprintf("  [%d file(s) uncaptured]", r.Missed)
	}
	if len(r.Exceptions) > 0 {
		line += fmt.Sprintf("  [%d exception(s)]", len(r.Exceptions))
	}
	return line
}

// viewHeader renders protection status above the timeline, from the status
// JSON alone. A dead daemon (protected=false) renders "Not protected", never
// a fabricated Protected state.
func (m Model) viewHeader(b *strings.Builder) {
	if m.status == nil {
		b.WriteString("checkpoint: loading status...\n")
		return
	}
	st := m.status
	state := "Protected"
	switch {
	case !st.Protected:
		state = "Not protected (no daemon running for this project)"
	case st.SettingUp:
		state = "Setting up (first scan still running)"
	case st.Limited:
		state = "Limited protection"
	}
	fmt.Fprintf(b, "checkpoint: %s\n", state)
	if st.Root != "" {
		fmt.Fprintf(b, "Root: %s\n", st.Root)
	}
	for _, d := range st.ProtectedDirs {
		if d != st.Root {
			fmt.Fprintf(b, "Also protecting: %s\n", d)
		}
	}
	fmt.Fprintf(b, "Last complete checkpoint: %s\n", agoOrNone(st.LastCkptNS))
	if st.Protected {
		if st.BaselineComplete {
			b.WriteString("Complete baseline: yes\n")
		} else {
			b.WriteString("Complete baseline: NO\n")
		}
		if st.FeedActive {
			b.WriteString("Change feed: active (delete attribution + change-scaled checkpoints)\n")
		} else {
			b.WriteString("Change feed: unavailable on this filesystem; delete attribution is unavailable and checkpoints use full scans\n")
		}
		if st.AgentSessions > 0 {
			fmt.Fprintf(b, "Active agent sessions: %d\n", st.AgentSessions)
		}
	}
	if st.Protected && st.Limited {
		b.WriteString("Why limited:\n")
		if !st.BaselineComplete {
			b.WriteString("  ! first scan incomplete: there is no complete baseline yet, and a clean rescan runs automatically until one succeeds\n")
		}
		if st.Overflowed {
			b.WriteString("  ! a burst overflowed the watch queue, so some changes since the last checkpoint may be uncaptured (unbounded)\n")
		}
		if st.Missed > 0 {
			fmt.Fprintf(b, "  ! %d file(s) since the last checkpoint could not be captured\n", st.Missed)
		}
		if st.OutsideCount > 0 {
			fmt.Fprintf(b, "  ! unprotected changes: the agent wrote %d time(s) outside the protected folders (not captured, not restorable):\n", st.OutsideCount)
			for _, p := range st.Outside {
				fmt.Fprintf(b, "      %s\n", p)
			}
			if st.OutsideCount > len(st.Outside) {
				fmt.Fprintf(b, "      … more paths beyond the %d listed\n", len(st.Outside))
			}
			b.WriteString("      (add folders with: daemon --protect DIR,DIR)\n")
		}
	}
}

func agoOrNone(ns int64) string {
	if ns == 0 {
		return "none yet"
	}
	d := time.Since(time.Unix(0, ns))
	if d < 0 {
		d = 0
	}
	return fmt.Sprintf("%s ago", d.Round(time.Second))
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

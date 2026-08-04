package tui

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// fakeClient serves canned CLI --json output, because the TUI must work from
// JSON alone, exactly like a third-party client. No daemon is involved.
type fakeClient struct {
	status   string
	history  string
	undoOut  string
	undoErr  error
	undoRuns int
	saveBoth []bool // the save-both value of each Undo call
}

func (f *fakeClient) HistoryJSON() ([]byte, error) { return []byte(f.history), nil }
func (f *fakeClient) StatusJSON() ([]byte, error)  { return []byte(f.status), nil }
func (f *fakeClient) Undo(saveBoth bool) (string, error) {
	f.undoRuns++
	f.saveBoth = append(f.saveBoth, saveBoth)
	return f.undoOut, f.undoErr
}

const statusFixture = `{
  "protected": true, "root": "/w",
  "protected_dirs": ["/w", "/etc/extra"],
  "checkpoints": 2, "limited": true, "missed": 2, "overflowed": false,
  "agent_sessions": 1, "since_unix_ns": 1, "setting_up": false,
  "last_ckpt_ns": 1700000000000000000, "feed_active": true,
  "baseline_complete": true,
  "outside": ["/tmp/outside.txt"], "outside_count": 3
}`

const notProtectedFixture = `{
  "protected": false, "root": "/w", "checkpoints": 2,
  "limited": false, "missed": 0, "overflowed": false,
  "agent_sessions": 0, "setting_up": false, "last_ckpt_ns": 0,
  "feed_active": false, "baseline_complete": false, "outside_count": 0
}`

const historyFixture = `{"checkpoints":[
  {"id":7,"time_ns":1700000000000000000,"badge":"Recoverable with exceptions",
   "source":"agent-turn","name":"wip-auth","settle_timed_out":true,"missed":1,
   "exceptions":[{"path":"pipes/build.fifo","reason":"unsupported kind"},
                 {"path":"locked.db","reason":"unreadable"}]},
  {"id":6,"time_ns":1699999000000000000,"badge":"Fully recoverable",
   "source":"run: go test","name":"","settle_timed_out":false,"missed":0,
   "exceptions":[]}
]}`

func pump(t *testing.T, m tea.Model, msg tea.Msg) tea.Model {
	t.Helper()
	next, cmd := m.Update(msg)
	for cmd != nil {
		out := cmd()
		if out == nil {
			break
		}
		next, cmd = next.Update(out)
	}
	return next
}

func loaded(t *testing.T, c Client) tea.Model {
	t.Helper()
	m := New(c)
	var tm tea.Model = m
	if msg := m.Init()(); msg != nil {
		tm = pump(t, tm, msg)
	}
	return tm
}

func key(s string) tea.KeyMsg {
	if len(s) == 1 {
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	}
	panic("unknown key " + s)
}

func TestTimelineRendersFromFixture(t *testing.T) {
	m := loaded(t, &fakeClient{status: statusFixture, history: historyFixture})
	v := m.View()
	for _, want := range []string{
		// header: state + evidence
		"Limited protection",
		"Last complete checkpoint:",
		"Complete baseline: yes",
		"Change feed: active",
		"2 file(s) since the last checkpoint could not be captured",
		"/tmp/outside.txt",
		"Also protecting: /etc/extra",
		// timeline rows: id, badge, source, name, settle note
		"#7", "#6",
		"Recoverable with exceptions",
		"Fully recoverable",
		"agent-turn",
		"run: go test",
		`name:"wip-auth"`,
		"[settle timed out]",
	} {
		if !strings.Contains(v, want) {
			t.Fatalf("timeline missing %q:\n%s", want, v)
		}
	}
}

func TestDetailShowsNamedExceptions(t *testing.T) {
	m := loaded(t, &fakeClient{status: statusFixture, history: historyFixture})
	// cursor starts on #7 (newest first), which carries the exceptions
	m = pump(t, m, key("enter"))
	v := m.View()
	for _, want := range []string{
		"checkpoint #7",
		"Recoverable with exceptions",
		"pipes/build.fifo", "unsupported kind",
		"locked.db", "unreadable",
		"settle timed out",
	} {
		if !strings.Contains(v, want) {
			t.Fatalf("detail missing %q:\n%s", want, v)
		}
	}
	// the clean checkpoint reports no exceptions
	m = pump(t, m, key("esc"))
	m = pump(t, m, key("down"))
	m = pump(t, m, key("enter"))
	if v := m.View(); !strings.Contains(v, "Exceptions: none") {
		t.Fatalf("clean checkpoint detail should say none:\n%s", v)
	}
}

func TestUndoConfirmFlowTriggersClient(t *testing.T) {
	c := &fakeClient{
		status:  statusFixture,
		history: historyFixture,
		undoOut: "reverted 2 file(s); saved both sides of 1 conflict",
	}
	m := loaded(t, c)

	// 'u' opens confirm; nothing runs yet; disclaimer is on the surface
	m = pump(t, m, key("u"))
	v := m.View()
	if !strings.Contains(v, "undo the agent's latest turn?") {
		t.Fatalf("confirm screen wrong:\n%s", v)
	}
	if !strings.Contains(v, "external side effects") {
		t.Fatal("confirm surface lacks the non-file disclaimer")
	}
	if !strings.Contains(v, "save-both: OFF") {
		t.Fatalf("save-both should start OFF:\n%s", v)
	}
	if c.undoRuns != 0 {
		t.Fatal("undo ran before confirmation")
	}

	// 'q' from confirm aborts without calling
	m = pump(t, m, key("q"))
	if c.undoRuns != 0 {
		t.Fatal("undo ran after 'q' abort")
	}

	// reopen, toggle save-both with 's', confirm with 'y'
	m = pump(t, m, key("u"))
	m = pump(t, m, key("s"))
	if v := m.View(); !strings.Contains(v, "save-both: ON") {
		t.Fatalf("'s' did not toggle save-both:\n%s", v)
	}
	m = pump(t, m, key("y"))
	if c.undoRuns != 1 {
		t.Fatalf("undo ran %d times, want 1", c.undoRuns)
	}
	if len(c.saveBoth) != 1 || !c.saveBoth[0] {
		t.Fatalf("undo called with saveBoth=%v, want [true]", c.saveBoth)
	}
	// result shows the CLI output verbatim
	if v := m.View(); !strings.Contains(v, "reverted 2 file(s); saved both sides of 1 conflict") {
		t.Fatalf("result view missing verbatim undo output:\n%s", v)
	}
}

func TestNotProtectedRenders(t *testing.T) {
	m := loaded(t, &fakeClient{status: notProtectedFixture, history: historyFixture})
	v := m.View()
	if !strings.Contains(v, "Not protected") {
		t.Fatalf("dead-daemon status must render Not protected:\n%s", v)
	}
	if strings.Contains(v, "checkpoint: Protected\n") {
		t.Fatalf("fabricated Protected state:\n%s", v)
	}
	// history still renders: checkpoints on record are real without a daemon
	if !strings.Contains(v, "#7") {
		t.Fatalf("timeline missing with dead daemon:\n%s", v)
	}
	if !strings.Contains(v, "Last complete checkpoint: none yet") {
		t.Fatalf("zero last_ckpt_ns should render none yet:\n%s", v)
	}
}

// TestPureClientImports enforces the pure-client rule mechanically: the TUI
// must import no github.com/manyapn/checkpoint-public/internal/* package, so it
// can render only what the CLI's JSON contract actually carries.
func TestPureClientImports(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", e.Name()), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		checked++
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if strings.HasPrefix(path, "github.com/manyapn/checkpoint-public/internal/") {
				t.Errorf("%s imports %s; the TUI must be a pure client of the CLI's JSON", e.Name(), path)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no Go source files checked")
	}
}

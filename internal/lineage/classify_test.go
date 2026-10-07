package lineage

import "testing"

func TestClassify(t *testing.T) {
	root := Identity{Pid: 100, Start: 5}
	roots := map[Identity]bool{root: true}
	cases := []struct {
		name     string
		chain    []Identity
		resolved bool
		want     string
	}{
		{"descendant of agent", []Identity{{300, 9}, {200, 7}, root}, true, Agent},
		{"ends at init", []Identity{{300, 9}, {1, 1}}, true, Human},
		{"unresolved walk", []Identity{{300, 9}}, false, Unknown},
		{"empty", nil, true, Unknown},
		{"recycled pid same number different start", []Identity{{300, 9}, {100, 6}, {1, 1}}, true, Human},
		{"root mid-chain is malformed", []Identity{{300, 9}, root, {1, 1}}, true, Unknown},
		{"duplicate identity is malformed", []Identity{{300, 9}, {300, 9}, root}, true, Unknown},
		{"nonpositive pid", []Identity{{0, 0}, root}, true, Unknown},
	}
	for _, c := range cases {
		if got := Classify(c.chain, c.resolved, roots); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

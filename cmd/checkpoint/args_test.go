package main

import (
	"flag"
	"strings"
	"testing"
)

// TestParseOnlyRefusesEmptyAndEscapes pins two failure modes that share a root
// cause: a selector that silently means something WIDER or OTHER than what the
// user wrote.
//
//   - `--only ”` (trivially produced by an unset shell/CI variable) must never
//     widen into a full destructive operation.
//   - A path outside the workspace must be refused, not silently ignored
//     (reporting "nothing to revert" reads as "that file was clean").
func TestParseOnlyRefusesEmptyAndEscapes(t *testing.T) {
	const root = "/w"
	if _, present, err := parseOnly("", false, root); err != nil || present {
		t.Fatalf("an absent --only is the whole operation: present=%v err=%v", present, err)
	}
	for _, bad := range []string{"", "   ", ",", " , "} {
		_, present, err := parseOnly(bad, true, root)
		if err == nil {
			t.Fatalf("--only %q was given but names nothing: must refuse, not widen", bad)
		}
		if !present {
			t.Fatalf("--only %q must report as given", bad)
		}
		if !strings.Contains(err.Error(), "refusing to widen") {
			t.Fatalf("the refusal must explain why: %v", err)
		}
	}
	for _, bad := range []string{"../outside", "../../etc/passwd", "/etc/passwd", "a/../../b"} {
		if _, _, err := parseOnly(bad, true, root); err == nil {
			t.Fatalf("--only %q escapes the workspace and must be refused", bad)
		}
	}
	only, present, err := parseOnly(" a.txt , sub/b.txt ", true, root)
	if err != nil || !present || len(only) != 2 || only[0] != "a.txt" || only[1] != "sub/b.txt" {
		t.Fatalf("a real selector must survive intact: %v %v %v", only, present, err)
	}
	// An absolute path INSIDE the workspace is accepted, normalized to relative.
	if got, _, err := parseOnly("/w/a.txt", true, root); err != nil || len(got) != 1 || got[0] != "a.txt" {
		t.Fatalf("in-workspace absolute path should normalize: %v %v", got, err)
	}
}

// TestParseCheckpointIDRejectsSuffixes pins whole-token id parsing:
// fmt.Sscanf("%d") stops at the first non-digit, so a typo would silently
// restore the numeric PREFIX, which is a destructive operation against the
// wrong checkpoint.
func TestParseCheckpointIDRejectsSuffixes(t *testing.T) {
	for _, bad := range []string{"0junk", "1.0", "1/2", "0x1", "", " 1", "1 ", "-1", "+1", "01junk", "9999999999999999999"} {
		if _, err := parseCheckpointID(bad); err == nil {
			t.Fatalf("id %q must be rejected outright, not partially parsed", bad)
		}
	}
	for in, want := range map[string]int{"0": 0, "1": 1, "42": 42, "007": 7} {
		got, err := parseCheckpointID(in)
		if err != nil || got != want {
			t.Fatalf("id %q: got %d err=%v, want %d", in, got, err, want)
		}
	}
}

// TestFlagWasSetDistinguishesEmptyFromAbsent pins the mechanism the empty-
// selector refusal depends on: Go's flag package reports "" for both
// `--only ”` and an absent --only, so the refusal cannot be driven by the
// value alone.
func TestFlagWasSetDistinguishesEmptyFromAbsent(t *testing.T) {
	newFS := func() (*flag.FlagSet, *string) {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		v := fs.String("only", "", "")
		return fs, v
	}
	fs, v := newFS()
	if err := fs.Parse([]string{}); err != nil {
		t.Fatal(err)
	}
	if flagWasSet(fs, "only") || *v != "" {
		t.Fatal("absent --only must report not-set")
	}
	fs, v = newFS()
	if err := fs.Parse([]string{"--only", ""}); err != nil {
		t.Fatal(err)
	}
	if !flagWasSet(fs, "only") || *v != "" {
		t.Fatal("an explicitly empty --only must report SET (same value, different meaning)")
	}
}

package main

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// printVersion reports which SOURCE this binary was built from. Go stamps the
// VCS revision, commit time, and dirty flag into the build automatically, so
// this needs no build-time flags and cannot drift from reality. Without it
// there is no way to tell a fixed build from a stale binary left in bin/, and a
// bug report against the wrong artifact costs more than it reports.
func printVersion() {
	rev, when, dirty := "unknown", "unknown", false
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.time":
				when = s.Value
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
	}
	mark := ""
	if dirty {
		mark = " (built from a MODIFIED working tree)"
	}
	fmt.Printf("%s\ncommit: %s%s\ncommit time: %s\n", prog(), rev, mark, when)
	fmt.Printf("go: %s\n", runtime.Version())
	fmt.Println("(compare `commit` against the revision you expect to be testing)")
}

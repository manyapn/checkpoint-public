package main

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

func printVersion() {
	rev, dirty := "unknown", ""
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" {
				rev = s.Value
			}
			if s.Key == "vcs.modified" && s.Value == "true" {
				dirty = " (modified working tree)"
			}
		}
	}
	fmt.Printf("checkpoint %s%s, %s\n", rev, dirty, runtime.Version())
}

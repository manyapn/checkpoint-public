//go:build !linux

// Non-linux stubs so the tree builds on a macOS dev host. checkpoint's capture
// is fanotify-only, so every probe here reports the same honest answer: this
// platform cannot run checkpoint at all. That is a FATAL finding, not a silent
// pass: a user on macOS must be told "Linux only", not handed a green report.
package doctor

import (
	"fmt"
	"os"
	"runtime"
)

func probeKernel() Check {
	return Check{
		Name:   "kernel",
		Fatal:  true,
		Detail: fmt.Sprintf("this is %s; checkpoint captures writes with Linux fanotify and has no equivalent here", runtime.GOOS),
		Remedy: "run checkpoint on Linux (kernel 5.1+), e.g. in a VM or a container started with --cap-add SYS_ADMIN",
	}
}

func probeFanotify() Check {
	return Check{
		Name:   "fanotify capability",
		Fatal:  true,
		Detail: fmt.Sprintf("fanotify does not exist on %s", runtime.GOOS),
		Remedy: "run checkpoint on Linux (kernel 5.1+)",
	}
}

func probeFilesystem(workspace string, valid bool) Check {
	return Check{
		Name:   "workspace filesystem",
		Detail: fmt.Sprintf("not checked: the dirent change feed is Linux-only (%s)", runtime.GOOS),
		Remedy: "run checkpoint on Linux (kernel 5.17+ for the change feed)",
	}
}

// fsStat cannot name the filesystem portably; free space is left to the caller
// to report as unavailable rather than guessed.
func fsStat(path string) (fsType string, free, total uint64, err error) {
	if _, serr := os.Stat(path); serr != nil {
		return "", 0, 0, serr
	}
	return "", 0, 0, fmt.Errorf("free space reporting is not implemented on %s", runtime.GOOS)
}

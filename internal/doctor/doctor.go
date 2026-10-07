// Package doctor answers "will checkpoint work on this machine?" by probing,
// not predicting: it arms fanotify for real and reads the filesystem type.
package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/manyapn/checkpoint-public/internal/snapshot"
)

type Check struct {
	Name   string
	OK     bool
	Fatal  bool // a failed fatal check means checkpoint cannot run here
	Detail string
	Remedy string
}

type Report struct{ Checks []Check }

func Run(root, storeDir string) Report {
	return Report{Checks: []Check{
		kernelCheck(),
		workspaceCheck(root),
		fanotifyCheck(root),
		filesystemCheck(root),
		storeCheck(root, storeDir),
	}}
}

func (r Report) Healthy() bool {
	for _, c := range r.Checks {
		if !c.OK && c.Fatal {
			return false
		}
	}
	return true
}

func (r Report) Text() string {
	var b strings.Builder
	fails, warns := 0, 0
	for _, c := range r.Checks {
		label := "ok  "
		switch {
		case c.OK:
		case c.Fatal:
			label, fails = "FAIL", fails+1
		default:
			label, warns = "WARN", warns+1
		}
		fmt.Fprintf(&b, "%s  %-22s %s\n", label, c.Name, c.Detail)
		if !c.OK && c.Remedy != "" {
			fmt.Fprintf(&b, "      %-22s remedy: %s\n", "", c.Remedy)
		}
	}
	switch {
	case fails > 0:
		fmt.Fprintf(&b, "\ncheckpoint cannot run here: %d blocking problem(s) above.\n", fails)
	case warns > 0:
		fmt.Fprintf(&b, "\ncheckpoint will work here, with %d limitation(s) above.\n", warns)
	default:
		b.WriteString("\ncheckpoint should work on this machine.\n")
	}
	return b.String()
}

func workspaceCheck(root string) Check {
	c := Check{Name: "workspace", Fatal: true}
	fi, err := os.Lstat(root)
	switch {
	case err != nil:
		c.Detail = fmt.Sprintf("%s: %v", root, err)
		c.Remedy = "pass an existing project directory with --root"
	case fi.Mode()&os.ModeSymlink != 0:
		resolved, _ := filepath.EvalSymlinks(root)
		c.Detail = root + " is a symlink; a symlinked root would capture nothing"
		c.Remedy = "use the real path: " + resolved
	case !fi.IsDir():
		c.Detail = root + " is not a directory"
		c.Remedy = "pass the project directory"
	default:
		c.OK = true
		c.Detail = root
	}
	return c
}

func storeCheck(root, storeDir string) Check {
	c := Check{Name: "store location", Fatal: true}
	if err := snapshot.CheckStoreLocation(storeDir, root); err != nil {
		c.Detail = err.Error()
		c.Remedy = "omit --store for the default under ~/.local/share/checkpoint, or point it outside the project"
		return c
	}
	parent := storeDir
	for {
		if fi, err := os.Stat(parent); err == nil && fi.IsDir() {
			break
		}
		next := filepath.Dir(parent)
		if next == parent {
			c.Detail = "no existing parent directory for " + storeDir
			c.Remedy = "create a parent directory for the store"
			return c
		}
		parent = next
	}
	probe, err := os.CreateTemp(parent, ".checkpoint-doctor-")
	if err != nil {
		c.Detail = fmt.Sprintf("%s is not writable: %v", parent, err)
		c.Remedy = "fix permissions, or pass --store to a directory you own"
		return c
	}
	probe.Close()
	os.Remove(probe.Name())
	c.OK = true
	c.Detail = storeDir + " (writable, outside the project)"
	return c
}

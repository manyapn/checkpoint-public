// Package doctor is checkpoint's preflight self-diagnosis: it answers, on a
// machine nobody who built checkpoint has ever seen, the only question that
// matters before the first run: "will this actually work here, and if not,
// what do I type?"
//
// The failure it exists to prevent: a user on the wrong kernel, the wrong
// filesystem, or without CAP_SYS_ADMIN today gets a raw errno from deep inside
// fanotify ("fanotify_init: operation not permitted") and has no way to know
// whether that is a bug, a missing flag, or an unsupported host. Every check
// here therefore reports (a) what was actually probed, (b) what was found, and
// (c) a concrete remedy the user can run.
//
// Design rules for this package:
//
//   - PROBE, DO NOT PREDICT. Whether fanotify can be armed is decided by
//     arming it, not by reading a kernel version. Version numbers are used only
//     to EXPLAIN a probe result and to name the supported floor.
//   - HONEST DEGRADATION. Only checks that make capture impossible are Fatal.
//     A filesystem without a change-feed (overlayfs) is not a failure of the
//     product; it is a named, bounded loss of function, and the Detail says
//     exactly which function.
//   - NOTHING IS SILENT. A check that could not run says it could not run and
//     why, rather than reporting OK.
//
// The store contributes two lines rather than one (a location check and a
// free-space check) because a store that is writable but nearly full is a
// different problem with a different remedy.
package doctor

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/manyapn/checkpoint-public/internal/store"
)

// Check is one preflight finding.
//
// OK=false && Fatal=true  -> checkpoint cannot work on this machine.
// OK=false && Fatal=false -> checkpoint works, with the named function reduced.
// Remedy is required whenever OK is false: a finding the user cannot act on is
// not worth printing.
type Check struct {
	Name   string
	OK     bool
	Fatal  bool
	Detail string
	Remedy string
}

// Report is the full preflight result, in display order.
type Report struct {
	Checks []Check
}

// Run performs every probe against this machine and returns the findings.
// It is side-effect-light but not side-effect-free: it creates and removes a
// temporary directory (for the fanotify probe) and a temporary file under the
// store's nearest existing ancestor (for the writability probe). It never
// writes inside the workspace.
//
// workspace is the directory that would be protected; storeDir is where the
// checkpoints would live. Either may be a path that does not exist yet, which
// is itself one of the findings.
func Run(workspace, storeDir string) Report {
	ws, wsErr := absPath(workspace)
	st, stErr := absPath(storeDir)

	// The filesystem probe arms a real mark on the workspace, so it only runs
	// once the workspace itself is known good; the store-location check works
	// on paths alone (both sides may not exist yet), so it only needs a path.
	wsCheck := workspaceCheck(ws, wsErr)

	return Report{Checks: []Check{
		probeKernel(),
		probeFanotify(),
		wsCheck,
		probeFilesystem(ws, wsCheck.OK),
		storeLocationCheck(st, stErr, ws, wsErr == nil),
		storeSpaceCheck(st, stErr),
	}}
}

// Healthy reports whether checkpoint can run here at all: true unless some
// Fatal check failed. Non-fatal failures (reduced function) keep it true; the
// caller is expected to print the report either way.
func (r Report) Healthy() bool {
	for _, c := range r.Checks {
		if !c.OK && c.Fatal {
			return false
		}
	}
	return true
}

// Text renders the report for a terminal: one line per check, with each failed
// check's remedy indented on the line(s) below it, then a one-line verdict.
//
// Labels: "ok" (passed), "WARN" (failed but not fatal, so some function is
// reduced), "FAIL" (failed and fatal, so checkpoint cannot work).
func (r Report) Text() string {
	width := 0
	for _, c := range r.Checks {
		if len(c.Name) > width {
			width = len(c.Name)
		}
	}
	var b strings.Builder
	fatals, warns := 0, 0
	for _, c := range r.Checks {
		label := "ok  "
		switch {
		case c.OK:
		case c.Fatal:
			label = "FAIL"
			fatals++
		default:
			label = "WARN"
			warns++
		}
		fmt.Fprintf(&b, "%s  %-*s  %s\n", label, width, c.Name, c.Detail)
		if !c.OK && c.Remedy != "" {
			for _, line := range strings.Split(c.Remedy, "\n") {
				fmt.Fprintf(&b, "      %-*s  remedy: %s\n", width, "", line)
			}
		}
	}
	b.WriteString("\n")
	switch {
	case fatals > 0:
		fmt.Fprintf(&b, "checkpoint cannot protect this workspace: %s above (FAIL).\n",
			plural(fatals, "blocking problem", "blocking problems"))
	case warns > 0:
		fmt.Fprintf(&b, "checkpoint will work here, with %s above (WARN).\n",
			plural(warns, "limitation", "limitations"))
	default:
		b.WriteString("checkpoint should work on this machine.\n")
	}
	return b.String()
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

func absPath(p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", fmt.Errorf("empty path")
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

// ---------------------------------------------------------------- workspace

// maxWalkEntries and walkBudget bound the workspace sizing walk. A preflight
// check must never be the reason a command hangs, so the walk stops at
// whichever limit is hit first and SAYS that the number is a floor, not a
// total. Both are vars so tests can shrink them.
var (
	maxWalkEntries = 50000
	walkBudget     = 2 * time.Second
)

// walkSkip mirrors the store's bulk-exclusion list: these directories are never
// captured, so counting them would misreport how much work checkpoint has to do.
var walkSkip = map[string]bool{
	".git": true, "node_modules": true, "build": true, "target": true,
	"dist": true, "__pycache__": true, ".venv": true,
}

func workspaceCheck(ws string, wsErr error) Check {
	c := Check{Name: "workspace", Fatal: true}
	if wsErr != nil {
		c.Detail = fmt.Sprintf("no workspace path given (%v)", wsErr)
		c.Remedy = "pass the project directory, e.g. `checkpoint status --root /path/to/project`"
		return c
	}
	fi, err := os.Lstat(ws)
	if err != nil {
		if os.IsNotExist(err) {
			c.Detail = fmt.Sprintf("%s does not exist", ws)
			c.Remedy = fmt.Sprintf("create it (`mkdir -p %s`) or point checkpoint at the right directory", ws)
			return c
		}
		c.Detail = fmt.Sprintf("%s cannot be inspected: %v", ws, err)
		c.Remedy = "check the path and its parent directories' permissions"
		return c
	}
	// Symlink roots are refused by the store layer (store.ErrSymlinkRoot):
	// a walk of a symlink visits only the link, so the checkpoint would hold
	// ZERO entries and still be badged recoverable. Catch it here, in words.
	if fi.Mode()&fs.ModeSymlink != 0 {
		resolved, rerr := filepath.EvalSymlinks(ws)
		c.Detail = fmt.Sprintf("%s is a symlink; a symlinked root captures nothing while still looking protected, so it is refused", ws)
		if rerr == nil {
			c.Remedy = fmt.Sprintf("use the resolved path instead: %s", resolved)
		} else {
			c.Remedy = "use the resolved path instead (the symlink target)"
		}
		return c
	}
	if !fi.IsDir() {
		c.Detail = fmt.Sprintf("%s is not a directory", ws)
		c.Remedy = "checkpoint protects a directory tree; pass the project directory"
		return c
	}

	files, bytes, unreadable, capped := walkSize(ws)
	c.OK = true
	noun := "files"
	if files == 1 {
		noun = "file"
	}
	size := fmt.Sprintf("%s %s, %s", commas(files), noun, humanBytes(bytes))
	if capped {
		size = fmt.Sprintf("at least %s %s, at least %s (stopped counting early; the tree is bigger)",
			commas(files), noun, humanBytes(bytes))
	}
	c.Detail = fmt.Sprintf("%s: %s to protect", ws, size)
	if unreadable > 0 {
		c.Detail += fmt.Sprintf("; %s unreadable and would be named as checkpoint exceptions", commas(unreadable))
	}
	return c
}

// walkSize measures the tree, bounded by BOTH an entry cap and a time budget.
// capped=true means the returned counts are lower bounds.
func walkSize(root string) (files int, bytes int64, unreadable int, capped bool) {
	deadline := time.Now().Add(walkBudget)
	seen := 0
	stop := fmt.Errorf("doctor: walk budget reached")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			unreadable++
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if path != root && walkSkip[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		seen++
		if seen%256 == 0 && time.Now().After(deadline) {
			return stop
		}
		if seen > maxWalkEntries {
			return stop
		}
		if d.Type().IsRegular() {
			if info, ierr := d.Info(); ierr == nil {
				files++
				bytes += info.Size()
			} else {
				unreadable++
			}
		}
		return nil
	})
	if err != nil {
		capped = true // either the budget stop, or an unwalkable root
	}
	return files, bytes, unreadable, capped
}

// ------------------------------------------------------------------- store

func storeLocationCheck(st string, stErr error, ws string, wsValid bool) Check {
	c := Check{Name: "store location", Fatal: true}
	if stErr != nil {
		c.Detail = fmt.Sprintf("no store path given (%v)", stErr)
		c.Remedy = "pass --store DIR, or let checkpoint use its default under $XDG_DATA_HOME/checkpoint"
		return c
	}
	// Out-of-tree is the whole guarantee: the session history has to outlive
	// the workspace it describes, and an in-tree store dies with
	// `rm -rf project`, taking that history with it.
	if wsValid {
		if err := store.CheckStoreLocation(st, ws); err != nil {
			c.Detail = fmt.Sprintf("%s is not out-of-tree relative to %s: %v", st, ws, err)
			c.Remedy = "put the store outside the project, e.g. --store $HOME/.local/share/checkpoint/<project>\n" +
				"(a store inside the project is deleted by the same `rm -rf` it is supposed to outlive)"
			return c
		}
	}
	existing, missing := nearestExisting(st)
	if existing == "" {
		c.Detail = fmt.Sprintf("no existing parent directory for %s", st)
		c.Remedy = "create the parent directory, or choose a store path under an existing one"
		return c
	}
	probeDir := existing
	if err := writeProbe(probeDir); err != nil {
		c.Detail = fmt.Sprintf("%s is not writable: %v", probeDir, err)
		c.Remedy = fmt.Sprintf("fix ownership/permissions (`sudo chown -R $(id -u):$(id -g) %s`), or pass --store to a directory you own", probeDir)
		return c
	}
	c.OK = true
	if missing == "" {
		c.Detail = fmt.Sprintf("%s: writable, out of the workspace tree", st)
	} else {
		c.Detail = fmt.Sprintf("%s: will be created (%s exists and is writable), out of the workspace tree", st, existing)
	}
	return c
}

func storeSpaceCheck(st string, stErr error) Check {
	c := Check{Name: "store free space"}
	if stErr != nil {
		c.Detail = "not checked: no store path given"
		c.Remedy = "pass --store DIR"
		return c
	}
	existing, _ := nearestExisting(st)
	if existing == "" {
		c.Detail = fmt.Sprintf("not checked: no existing parent directory for %s", st)
		c.Remedy = "create the parent directory, then re-run the check"
		return c
	}
	_, free, total, err := fsStat(existing)
	if err != nil {
		c.Detail = fmt.Sprintf("could not read free space for %s: %v", existing, err)
		c.Remedy = "check the path manually with `df -h`"
		return c
	}
	// Checkpoints store one blob per distinct file version; the floor below is
	// a "you will hit the wall almost immediately" line, not a sizing model.
	const lowWater = 500 * 1000 * 1000
	if free < lowWater {
		c.Detail = fmt.Sprintf("only %s free of %s on the store filesystem (%s): very little room for checkpoints",
			humanBytes(int64(free)), humanBytes(int64(total)), existing)
		c.Remedy = "free space on that filesystem, or pass --store DIR on a larger one; `checkpoint prune --keep-days N` reclaims old checkpoints"
		return c
	}
	c.OK = true
	c.Detail = fmt.Sprintf("%s free of %s on the store filesystem (%s)",
		humanBytes(int64(free)), humanBytes(int64(total)), existing)
	return c
}

// nearestExisting splits p into its nearest existing ancestor and the tail that
// does not exist yet ("" when p itself exists).
func nearestExisting(p string) (existing, missing string) {
	cur := p
	var tail []string
	for {
		if fi, err := os.Stat(cur); err == nil && fi.IsDir() {
			return cur, filepath.Join(tail...)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", p
		}
		tail = append([]string{filepath.Base(cur)}, tail...)
		cur = parent
	}
}

// writeProbe proves writability by writing, not by reading permission bits:
// root bypasses mode bits, read-only mounts and SELinux denials do not show up
// in them, and a wrong answer here is exactly the vague failure this package
// exists to remove.
func writeProbe(dir string) error {
	f, err := os.CreateTemp(dir, ".checkpoint-doctor-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_, werr := f.Write([]byte("checkpoint doctor write probe\n"))
	cerr := f.Close()
	os.Remove(name)
	if werr != nil {
		return werr
	}
	return cerr
}

// ------------------------------------------------------------------ format

func humanBytes(n int64) string {
	if n < 0 {
		return "unknown"
	}
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d bytes", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "kMGTP"[exp])
}

func commas(n int) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}

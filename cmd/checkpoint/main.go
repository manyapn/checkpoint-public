// Command checkpoint is the CLI surface: it establishes standing protection over
// a project, captures every completed filesystem write with the writing
// process's lineage, cuts checkpoints into an out-of-tree content-addressed
// store, and reverts an agent's changes while leaving human edits alone.
//
// Every command resolves its store through resolveStore, which enforces the
// invariant the whole design rests on: the store never lives inside the project
// it protects, so `rm -rf project` cannot destroy the means of recovering it.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/manyapn/checkpoint-public/internal/capture"
	"github.com/manyapn/checkpoint-public/internal/daemon"
	"github.com/manyapn/checkpoint-public/internal/doctor"
	"github.com/manyapn/checkpoint-public/internal/objstore"
	"github.com/manyapn/checkpoint-public/internal/oplog"
	"github.com/manyapn/checkpoint-public/internal/provenance"
	"github.com/manyapn/checkpoint-public/internal/selftest"
	"github.com/manyapn/checkpoint-public/internal/status"
	"github.com/manyapn/checkpoint-public/internal/store"
	"github.com/manyapn/checkpoint-public/internal/tui"
	"github.com/manyapn/checkpoint-public/internal/undo"
	"github.com/manyapn/checkpoint-public/internal/versionlog"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "create":
		err = cmdCreate(os.Args[2:])
	case "restore":
		err = cmdRestore(os.Args[2:])
	case "capture":
		err = cmdCapture(os.Args[2:])
	case "recover":
		err = cmdRecover(os.Args[2:])
	case "daemon":
		err = cmdDaemon(os.Args[2:])
	case "protect":
		err = cmdProtect(os.Args[2:])
	case "doctor":
		err = cmdDoctor(os.Args[2:])
	case "selftest":
		err = cmdSelftest(os.Args[2:])
	case "ui":
		err = cmdUI(os.Args[2:])
	case "run":
		err = cmdRun(os.Args[2:])
	case "save":
		err = cmdSave(os.Args[2:])
	case "undo":
		err = cmdUndo(os.Args[2:])
	case "history":
		err = cmdHistory(os.Args[2:])
	case "prune":
		err = cmdPrune(os.Args[2:])
	case "status":
		err = cmdStatus(os.Args[2:])
	case "register-agent":
		err = cmdRegisterAgent(os.Args[2:], true)
	case "unregister-agent":
		err = cmdRegisterAgent(os.Args[2:], false)
	case "version", "--version":
		printVersion()
		return
	case "__trampoline":
		// Internal: `run`'s registration gate (see cmdRun). Not in usage.
		err = cmdTrampoline(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "%s: unknown command %q\n\n", prog(), os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", prog(), err)
		os.Exit(1)
	}
}

// prog is the name this binary was invoked as. It ships as `checkpoint` but may
// be renamed by a packager, so every message that names the command reads it
// from argv rather than hardcoding one.
func prog() string {
	if len(os.Args) > 0 && os.Args[0] != "" {
		return filepath.Base(os.Args[0])
	}
	return "checkpoint"
}

func usage() {
	fmt.Fprintf(os.Stderr, `%[1]s: provenance-aware undo and durable checkpoints for coding agents

usage (flags must precede positional args):
  %[1]s create  [--store DIR] <project-dir>
  %[1]s restore [--store DIR] [--include-extra [--yes]] [--only rel,rel] <id> <target-dir>
  %[1]s capture [--store DIR] <workspace>            (linux; needs CAP_SYS_ADMIN)
  %[1]s recover [--store DIR] [--to DIR] <workspace>
  %[1]s daemon  [--store DIR] [--protect DIR,DIR] <root>   (linux; needs CAP_SYS_ADMIN)
  %[1]s protect [--store DIR] [--protect DIR,DIR] [--stop] [<root>]   (standing protection, detached)
  %[1]s doctor  [--root DIR] [--store DIR]        (will this work on THIS machine?)
  %[1]s selftest [--json]                        (prove the guarantees hold HERE)
  %[1]s run     [--root DIR] [--store DIR] -- <command...>
  %[1]s save    [--root DIR] [--store DIR] [--source LABEL] [--name LABEL]
  %[1]s undo    [--root DIR] [--store DIR] [--only rel,rel] [--save-both]
  %[1]s history [--root DIR] [--store DIR] [--json]
  %[1]s prune   [--root DIR] [--store DIR] [--keep-days N] [--dry-run] [--yes]
  %[1]s status  [--root DIR] [--store DIR] [--json]
  %[1]s ui      [--root DIR] [--store DIR]
  %[1]s version                          (which commit this binary was built from)
  %[1]s register-agent   [--root DIR] [--store DIR] --pid N
  %[1]s unregister-agent [--root DIR] [--store DIR] --pid N

typical use:
  %[1]s doctor                   will this work on this machine?
  %[1]s protect                  start standing protection for this project
  %[1]s run -- <agent command>   run an agent; checkpoint its turn on exit
  %[1]s undo                     revert that turn's agent-only changes

create/restore: checkpoint the whole project and restore it by id.
capture: continuously save every completed write, so a file created and
deleted before any checkpoint is still recoverable. recover: list (or extract
with --to) those transient files no checkpoint holds. daemon: run the always-on
protector for a root. run: execute a command and, when it exits, ask the daemon
to cut one checkpoint (settling briefly to absorb trailing writes). save: ask the
daemon to cut one checkpoint now. Save is the source-agnostic boundary, used both
by an agent-turn hook (e.g. Claude Code's Stop hook) and by hand; --name names the
checkpoint (named checkpoints survive pruning and always cut, even with no
changes). undo: revert the latest checkpoint's agent-only changes, preserving
human edits; snapshots the present first so the undo is itself undoable.
prune: delete unnamed checkpoints older than --keep-days (default 7), then
reclaim unreferenced content; named checkpoints and the latest durable baseline
always survive. Prune requires the daemon stopped.

The store lives outside the project (default: $XDG_DATA_HOME/checkpoint/<key>),
so deleting the project cannot destroy its checkpoints. Restoring in place uses
the same default store as create; use --store to point elsewhere.
`, prog())
}

func cmdCreate(args []string) error {
	fs := flag.NewFlagSet("create", flag.ExitOnError)
	storeFlag := fs.String("store", "", "store directory (default: derived from project path)")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("create: expected <project-dir>")
	}
	project, err := resolveDir(fs.Arg(0))
	if err != nil {
		return err
	}
	if fi, err := os.Stat(project); err != nil || !fi.IsDir() {
		return fmt.Errorf("create: %s is not a directory", project)
	}
	storeDir, err := resolveStore(*storeFlag, project)
	if err != nil {
		return err
	}
	if err := requireStoreFor(storeDir, project); err != nil {
		return err
	}
	oc, err := objstore.Open(storeDir)
	if err != nil {
		return err
	}
	prev, _, err := store.Latest(storeDir)
	if err != nil {
		return err
	}
	id, err := store.NextID(storeDir)
	if err != nil {
		return err
	}
	// Concurrent creates race for the next id; store.Write refuses to replace an
	// existing manifest (that refusal is what stops one writer destroying
	// another's checkpoint), so take the next free id and retry rather than
	// failing the user's create.
	var m *store.Manifest
	for attempt := 0; attempt < 8; attempt++ {
		cand, err := store.Snapshot(project, oc, prev, id, time.Now().UnixNano(), store.DURABLE, 0)
		if err != nil {
			return err
		}
		werr := store.Write(storeDir, cand)
		if werr == nil {
			m = cand
			break
		}
		if !errors.Is(werr, os.ErrExist) {
			return werr
		}
		if id, err = store.NextID(storeDir); err != nil {
			return err
		}
	}
	if m == nil {
		return fmt.Errorf("create: could not claim a checkpoint id after 8 attempts (heavy concurrent writes); retry")
	}
	fmt.Printf("created checkpoint %d (%d entries) [store: %s]\n", m.ID, len(m.Entries), storeDir)
	return nil
}

func cmdRestore(args []string) error {
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	storeFlag := fs.String("store", "", "store directory (default: derived from target path)")
	includeExtra := fs.Bool("include-extra", false, "also restore extra protected folders IN PLACE (to their original absolute locations)")
	yesFlag := fs.Bool("yes", false, "skip the --include-extra confirmation (noninteractive)")
	onlyFlag := fs.String("only", "", "comma-separated workspace-relative paths: restore only these entries from the checkpoint")
	fs.Parse(args)
	if fs.NArg() != 2 {
		return fmt.Errorf("restore: expected <id> <target-dir>")
	}
	id, err := parseCheckpointID(fs.Arg(0))
	if err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	target, err := resolveDir(fs.Arg(1))
	if err != nil {
		return err
	}
	storeDir, err := resolveStore(*storeFlag, target)
	if err != nil {
		return err
	}
	m, err := store.Load(storeDir, id)
	if err != nil {
		if os.IsNotExist(err) && *storeFlag == "" {
			// The store was DERIVED from the target path: restoring into a new
			// directory therefore looked in a store that does not exist. This
			// is the single most confusing failure in the CLI, so name it.
			return fmt.Errorf("no checkpoint %d in %s.\n"+
				"  the store was derived from the target directory, and restoring into a NEW directory\n"+
				"  looks in a different (empty) store. Name the project's store explicitly:\n"+
				"    %s restore --store <the project's store> %d %s\n"+
				"  (%s status --root <project> shows its store path)", id, storeDir, prog(), id, target, prog())
		}
		return fmt.Errorf("restore: load checkpoint %d: %w", id, err)
	}
	// Single-file restore: --only keeps just the named workspace entries.
	// Workspace-scoped like undo's --only, so it cannot be combined with
	// --include-extra.
	only, onlyGiven, err := parseOnly(*onlyFlag, flagWasSet(fs, "only"), m.Root)
	if err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	if onlyGiven {
		if *includeExtra {
			return fmt.Errorf("restore: --only is workspace-scoped and cannot be combined with --include-extra")
		}
		filtered := map[string]store.Entry{}
		for _, rel := range only {
			e, ok := m.Entries[rel]
			if !ok {
				return fmt.Errorf("restore: %s is not in checkpoint %d", rel, id)
			}
			filtered[rel] = e
		}
		m = &store.Manifest{ID: m.ID, TimeNS: m.TimeNS, Root: m.Root, Coverage: m.Coverage, Entries: filtered}
	}
	oc, err := objstore.Open(storeDir)
	if err != nil {
		return err
	}
	if op, interrupted := oplog.CheckInterrupted(storeDir); interrupted {
		fmt.Printf("note: %s\n", oplog.Describe(op))
	}
	// The include-extra confirmation happens BEFORE any mutation (incl. the
	// pre-restore checkpoint): the user must see the actual outside-workspace
	// paths this will rewrite, not a count.
	if *includeExtra && len(m.Extra) > 0 {
		// LOAD the identity; never stamp it here. EnsureMeta on this path would
		// mint current-machine identity for exactly the carried/copied store this
		// check exists to refuse; only the daemon, running on the store's home
		// machine, stamps.
		meta, err := store.LoadMeta(storeDir)
		if os.IsNotExist(err) {
			return fmt.Errorf("extra-root restore refused: this store has no identity record (it predates identity stamping or was copied without it); run the daemon on the store's original machine to stamp it, or restore the extra folders manually")
		}
		if err != nil {
			return err
		}
		if err := store.CheckExtraRestoreIdentity(meta, m.Root); err != nil {
			return err
		}
		if err := confirmExtraPaths(m, *yesFlag); err != nil {
			return err
		}
	}
	// Restore auto-checkpoints the present FIRST, so overwriting an existing
	// tree is itself undoable. A missing/empty target has nothing to save. A
	// taken id (the daemon's autonomous cuts race this) is retried, never
	// overwritten.
	if ents, err := os.ReadDir(target); err == nil && len(ents) > 0 {
		var pre *store.Manifest
		for attempt := 0; attempt < 3 && pre == nil; attempt++ {
			preID, err := store.NextID(storeDir)
			if err != nil {
				return err
			}
			p, err := store.Snapshot(target, oc, m, preID, time.Now().UnixNano(), store.DURABLE, 0)
			if err != nil {
				return err
			}
			p.Source = "pre-restore"
			switch err := store.Write(storeDir, p); {
			case err == nil:
				pre = p
			case errors.Is(err, os.ErrExist):
				continue
			default:
				return err
			}
		}
		if pre == nil {
			return fmt.Errorf("could not cut the pre-restore checkpoint (id contention with the daemon); retry")
		}
		fmt.Printf("pre-restore checkpoint %d saved (restore it to undo this restore)\n", pre.ID)
	}
	// Journal the whole plan before the first mutation, so an interrupted
	// restore is diagnosable and safely retryable.
	var acts []oplog.Action
	for rel, e := range m.Entries {
		acts = append(acts, oplog.Action{Do: "restore-" + e.Kind, Path: filepath.Join(target, rel)})
	}
	if *includeExtra {
		for r, entries := range m.Extra {
			for rel, e := range entries {
				acts = append(acts, oplog.Action{Do: "restore-" + e.Kind, Path: filepath.Join(r, rel)})
			}
		}
	}
	sort.Slice(acts, func(i, j int) bool { return acts[i].Path < acts[j].Path })
	// The journal exists to make an INTERRUPTED mutation diagnosable. A failure
	// before the first mutation (bad target, unreadable object) must not leave
	// one behind, because the next unrelated restore would then report a phantom
	// incomplete operation.
	if err := oplog.Begin(storeDir, oplog.Op{Kind: "restore", Root: m.Root, CheckpointID: m.ID,
		Target: target, IncludeExtra: *includeExtra, Actions: acts}); err != nil {
		return err
	}
	mutated := false
	defer func() {
		if !mutated {
			oplog.Done(storeDir) // nothing was touched: no interruption to report
		}
	}()
	if fi, err := os.Lstat(target); err == nil && !fi.IsDir() {
		return fmt.Errorf("restore: target %s exists and is not a directory", target)
	}
	mutated = true

	// A WHOLE-checkpoint restore reproduces the tree exactly: paths the
	// checkpoint does not contain are removed (recoverable via the pre-restore
	// checkpoint cut above). A --only restore materializes just the named
	// entries and removes nothing: there, "absent from the manifest" says
	// nothing about what the user wants kept.
	var removed, keptExceptions []string
	if !onlyGiven {
		removed, keptExceptions, err = store.RestoreExact(m, oc, target)
	} else {
		err = store.Restore(m, oc, target)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "the operation journal is kept (%s); rerunning restore is safe\n",
			filepath.Join(storeDir, "operation.json"))
		return err
	}
	fmt.Printf("restored checkpoint %d (%d entries) to %s [coverage: %s]\n", m.ID, len(m.Entries), target, m.Coverage)
	if len(removed) > 0 {
		shown := removed
		if len(shown) > 10 {
			shown = shown[:10]
		}
		fmt.Printf("removed %d path(s) the checkpoint does not contain:\n", len(removed))
		for _, rel := range shown {
			fmt.Printf("  - %q\n", rel) // %q: a newline in a filename must not fake a log line
		}
		if len(removed) > len(shown) {
			fmt.Printf("  … and %d more\n", len(removed)-len(shown))
		}
		fmt.Println("  (default-skipped folders such as node_modules/.git were left untouched;")
		fmt.Println("   restore the pre-restore checkpoint above to get these back)")
	}
	// Named exceptions are things this checkpoint COULD NOT capture (a fifo, an
	// unreadable file). They are absent from the manifest for that reason, not
	// because the user does not want them, and no checkpoint can put them back,
	// so removing them would be irreversible. They are left alone, said plainly,
	// and never counted as "restored".
	if len(keptExceptions) > 0 {
		fmt.Printf("left %d path(s) this checkpoint could not capture (unrestorable, so never removed):\n", len(keptExceptions))
		for _, rel := range keptExceptions {
			fmt.Printf("  ~ %q\n", rel)
		}
	}
	if len(m.Extra) > 0 {
		if *includeExtra {
			if err := store.RestoreExtras(m, oc); err != nil {
				fmt.Fprintf(os.Stderr, "the operation journal is kept (%s); rerunning restore is safe\n",
					filepath.Join(storeDir, "operation.json"))
				return err
			}
			for r, entries := range m.Extra {
				fmt.Printf("restored extra protected folder %s (%d entries) in place\n", r, len(entries))
			}
		} else {
			fmt.Printf("note: checkpoint also covers %d extra protected folder(s); rerun with --include-extra to restore them in place\n", len(m.Extra))
		}
	}
	return oplog.Done(storeDir)
}

// confirmExtraPaths shows the ACTUAL outside-workspace paths an
// --include-extra restore will rewrite (first 10 + count) and requires an
// explicit yes. Noninteractive runs must pass --yes; a non-terminal stdin
// without it is a refusal, never a silent proceed.
func confirmExtraPaths(m *store.Manifest, yes bool) error {
	var paths []string
	for r, entries := range m.Extra {
		for rel := range entries {
			paths = append(paths, filepath.Join(r, rel))
		}
	}
	sort.Strings(paths)
	fmt.Printf("--include-extra will rewrite these paths OUTSIDE the workspace, in place:\n")
	n := len(paths)
	show := n
	if show > 10 {
		show = 10
	}
	for _, p := range paths[:show] {
		fmt.Printf("  %s\n", p)
	}
	if n > show {
		fmt.Printf("  … and %d more (%d total)\n", n-show, n)
	}
	if yes {
		return nil
	}
	if fi, err := os.Stdin.Stat(); err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return fmt.Errorf("refusing to rewrite %d outside-workspace path(s) without confirmation: rerun with --yes", n)
	}
	fmt.Printf("rewrite these in place? [y/N] ")
	var answer string
	if _, err := fmt.Fscanln(os.Stdin, &answer); err != nil {
		return fmt.Errorf("no confirmation received: rerun with --yes for noninteractive use")
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return nil
	}
	return fmt.Errorf("aborted: extra folders not restored")
}

func cmdCapture(args []string) error {
	fs := flag.NewFlagSet("capture", flag.ExitOnError)
	storeFlag := fs.String("store", "", "store directory (default: derived from workspace path)")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("capture: expected <workspace>")
	}
	ws, err := resolveDir(fs.Arg(0))
	if err != nil {
		return err
	}
	if fi, err := os.Stat(ws); err != nil || !fi.IsDir() {
		return fmt.Errorf("capture: %s is not a directory", ws)
	}
	storeDir, err := resolveStore(*storeFlag, ws)
	if err != nil {
		return err
	}
	w, err := capture.New(capture.Config{Workspace: ws, StoreDir: storeDir})
	if err != nil {
		return err
	}
	defer w.Close()

	stop := make(chan struct{})
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		close(stop)
	}()
	fmt.Printf("capturing %s -> %s (Ctrl-C to stop)\n", ws, storeDir)
	runErr := w.Run(stop)
	fmt.Printf("\ncaptured %d versions (%d missed)\n", len(w.Versions()), w.Missed())
	return runErr
}

func cmdRecover(args []string) error {
	fs := flag.NewFlagSet("recover", flag.ExitOnError)
	storeFlag := fs.String("store", "", "store directory (default: derived from workspace path)")
	toFlag := fs.String("to", "", "extract recoverable files under this directory (default: list only)")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("recover: expected <workspace>")
	}
	ws, err := resolveDir(fs.Arg(0))
	if err != nil {
		return err
	}
	storeDir, err := resolveStore(*storeFlag, ws)
	if err != nil {
		return err
	}
	if err := requireStoreFor(storeDir, ws); err != nil {
		return err
	}
	versions, err := versionlog.Read(filepath.Join(storeDir, "versionlog"))
	if err != nil {
		return err
	}
	oc, err := objstore.Open(storeDir)
	if err != nil {
		return err
	}
	// A path ANY checkpoint holds is restorable by id, not a "recovered file".
	ids, err := store.IDs(storeDir)
	if err != nil {
		return err
	}
	var ms []*store.Manifest
	for _, id := range ids {
		if m, err := store.Load(storeDir, id); err == nil {
			ms = append(ms, m)
		}
	}
	salv, evicted := store.Salvage(versions, ms, oc)
	// A path currently on disk is not lost, so there is nothing to recover.
	for p := range salv {
		if _, err := os.Lstat(p); err == nil {
			delete(salv, p)
		}
	}
	if evicted > 0 {
		fmt.Printf("note: %d older recoverable file(s) beyond the %d-entry cap are NOT listed here\n", evicted, store.MaxSalvageEntries)
	}
	if len(salv) == 0 {
		fmt.Println("no recoverable files (nothing captured that a checkpoint doesn't already hold)")
		return nil
	}
	paths := make([]string, 0, len(salv))
	for p := range salv {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	toDir := ""
	if *toFlag != "" {
		if toDir, err = resolveDir(*toFlag); err != nil {
			return err
		}
	}
	skipped := 0
	for _, p := range paths {
		if toDir == "" {
			fmt.Println(p)
			continue
		}
		rel, err := filepath.Rel(ws, p)
		if err != nil || rel == "" || rel[0] == '.' {
			rel = filepath.Base(p) // outside ws or odd: fall back to base name
		}
		// Recovery writes into a directory the user named; it must stay inside
		// it and must never write THROUGH something already sitting at the
		// destination. A symlink there would send salvaged bytes outside the
		// target, and an ordinary file there is live data that recovery must
		// never overwrite. Both are skipped and reported: recovery is an escape
		// hatch, never a silent overwrite.
		dst, err := store.SafeJoinUnder(toDir, rel)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  skipped %s: %v\n", p, err)
			skipped++
			continue
		}
		content, err := oc.Get(salv[p])
		if err != nil {
			return fmt.Errorf("recover %s: %w", p, err)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := store.WriteFileNoFollow(dst, content, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "  skipped %s: %v\n", p, err)
			skipped++
			continue
		}
		fmt.Printf("recovered %s -> %s\n", p, dst)
	}
	if skipped > 0 {
		fmt.Fprintf(os.Stderr, "%d file(s) not recovered because something already occupies the "+
			"destination (recovery never overwrites or follows an existing entry); "+
			"remove or rename it, or use a fresh --to directory\n", skipped)
		return fmt.Errorf("recover: %d file(s) skipped", skipped)
	}
	return nil
}

func cmdDaemon(args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	storeFlag := fs.String("store", "", "store directory (default: derived from root path)")
	protectFlag := fs.String("protect", "", "comma-separated additional folders to protect (absolute paths)")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("daemon: expected <root>")
	}
	root, err := resolveDir(fs.Arg(0))
	if err != nil {
		return err
	}
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return fmt.Errorf("daemon: %s is not a directory", root)
	}
	var extra []string
	for _, p := range strings.Split(*protectFlag, ",") {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			return err
		}
		if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
			return fmt.Errorf("daemon: protected folder %s is not a directory", abs)
		}
		extra = append(extra, abs)
	}
	storeDir, err := resolveStore(*storeFlag, root)
	if err != nil {
		return err
	}
	if err := checkSocketPath(storeDir); err != nil {
		return err
	}
	ready := make(chan struct{})
	stop := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		errCh <- daemon.Serve(daemon.Config{Workspace: root, Extra: extra, StoreDir: storeDir}, ready, stop)
	}()
	select {
	case <-ready:
		fmt.Printf("READY root=%s store=%s socket=%s\n", root, storeDir, daemon.SocketPath(storeDir))
	case err := <-errCh:
		return err // failed before it was listening
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-sig:
		close(stop)
		return <-errCh
	case err := <-errCh:
		return err
	}
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	rootFlag := fs.String("root", "", "protected root the daemon watches (default: current directory)")
	storeFlag := fs.String("store", "", "store directory (default: derived from root path)")
	noDaemonFlag := fs.Bool("no-daemon", false, "run the command even though no daemon is protecting the root; NOTHING is captured or recoverable")
	fs.Parse(args)
	cmdArgs := fs.Args()
	if len(cmdArgs) == 0 {
		return fmt.Errorf("run: expected -- <command...>")
	}
	root := *rootFlag
	if root == "" {
		if wd, err := os.Getwd(); err == nil {
			root = wd
		}
	}
	root, err := resolveDir(root)
	if err != nil {
		return err
	}
	storeDir, err := resolveStore(*storeFlag, root)
	if err != nil {
		return err
	}

	sock := daemon.SocketPath(storeDir)
	// A wrapped run must never execute unprotected while announcing that local
	// changes are recoverable: without a live daemon nothing is captured, so
	// the promise is false and the user learns only afterwards.
	// Refuse up front, and say exactly how to get protected. --no-daemon is the
	// explicit, self-labelling override for "run it anyway, unprotected".
	if _, err := daemon.RequestStatus(sock); err != nil {
		if !*noDaemonFlag {
			return fmt.Errorf("refusing to run unprotected: no daemon is answering for %s "+
				"(store %s). Nothing would be captured and no checkpoint could be cut, so this "+
				"command would run with NO recoverability.\n"+
				"  start protection first:  %s protect --store %s %s\n"+
				"  or run without capture:  %s run --no-daemon ... (nothing is recoverable)",
				root, storeDir, prog(), storeDir, root, prog())
		}
		fmt.Fprintln(os.Stderr, prog()+": WARNING: --no-daemon was given, so this command runs with NO capture: "+
			"nothing it writes is recoverable and no checkpoint will be cut.")
	} else {
		// The recoverability boundary, shown before every protected run:
		// permission is not recoverability, and we answer only the latter.
		fmt.Fprintln(os.Stderr, prog()+": local file changes are recoverable; network requests, remote databases, deploys, emails, and other outside effects are NOT.")
	}
	// The child runs through a self-stopping trampoline: it SIGSTOPs itself
	// BEFORE exec'ing the real command, we register it as an agent root while it
	// is stopped, then SIGCONT. Without this gate, a one-shot child the command
	// forks in its first milliseconds (rm, mv) can be born before the daemon's
	// birth-parent scanner activates. Once reaped, its lineage is unresolvable,
	// so a genuine agent write would degrade to unknown.
	self, err := os.Executable()
	if err != nil {
		return err
	}
	c := exec.Command(self, append([]string{"__trampoline", "--"}, cmdArgs...)...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Start(); err != nil {
		return err
	}
	child := c.Process.Pid
	if err := waitStopped(child, 2*time.Second); err != nil {
		fmt.Fprintf(os.Stderr, "%s: trampoline gate: %v (continuing)\n", prog(), err)
	}
	// Register the command's process tree as an agent root, so its writes (and
	// its descendants') attribute to the agent by lineage rather than by
	// boundary. The registration brackets the settle so trailing writes still
	// classify.
	childStart, _ := provenance.StartTime(child)
	if err := daemon.RegisterAgentRoot(sock, child, childStart); err != nil {
		fmt.Fprintf(os.Stderr, "%s: agent-root registration failed (is the daemon running?): %v\n", prog(), err)
	}
	syscall.Kill(child, syscall.SIGCONT)
	runErr := c.Wait()

	// Request a checkpoint whether or not the command succeeded, because its
	// writes matter either way. The daemon settles, then cuts exactly one
	// checkpoint.
	resp, reqErr := daemon.RequestCheckpoint(sock, "run: "+strings.Join(cmdArgs, " "))
	daemon.UnregisterAgentRoot(sock, child, childStart)
	if reqErr != nil {
		fmt.Fprintf(os.Stderr, "%s: boundary request failed (is the daemon running?): %v\n", prog(), reqErr)
	} else if resp.SkippedEmpty {
		fmt.Printf("no changes since checkpoint %d, so no new checkpoint was created\n", resp.ID)
	} else {
		tag := ""
		if resp.SettleTimedOut {
			tag = " (settle timed out; trailing writes may be mid-operation)"
		}
		fmt.Printf("checkpoint %d %s (%d entries)%s\n", resp.ID, resp.Coverage, resp.Entries, tag)
	}

	// Propagate the wrapped command's exit code.
	if runErr != nil {
		if ee, ok := runErr.(*exec.ExitError); ok {
			os.Exit(ee.ExitCode())
		}
		return runErr
	}
	return nil
}

func cmdSave(args []string) error {
	fs := flag.NewFlagSet("save", flag.ExitOnError)
	rootFlag := fs.String("root", "", "protected root the daemon watches (default: current directory)")
	storeFlag := fs.String("store", "", "store directory (default: derived from root path)")
	sourceFlag := fs.String("source", "manual", "label recorded on the checkpoint (e.g. the agent turn)")
	nameFlag := fs.String("name", "", "name this checkpoint; named checkpoints survive pruning and always cut, even with no changes")
	fs.Parse(args)
	if fs.NArg() != 0 {
		return fmt.Errorf("save: unexpected argument %q (this command takes only flags)", fs.Arg(0))
	}
	root := *rootFlag
	if root == "" {
		if wd, err := os.Getwd(); err == nil {
			root = wd
		}
	}
	root, err := resolveDir(root)
	if err != nil {
		return err
	}
	storeDir, err := resolveStore(*storeFlag, root)
	if err != nil {
		return err
	}
	sock := daemon.SocketPath(storeDir)
	resp, err := daemon.RequestNamedCheckpoint(sock, *sourceFlag, *nameFlag)
	if err != nil {
		// Turn hooks call save, so a raw "connect: no such file or directory"
		// is the message a stranger hits most often. Carry the remedy, the way
		// run's refusal does.
		if _, serr := daemon.RequestStatus(sock); serr != nil {
			return fmt.Errorf("no daemon is protecting %s (store %s), so there is nothing to checkpoint into.\n"+
				"  start protection:  %s protect --store %s %s", root, storeDir, prog(), storeDir, root)
		}
		return err
	}
	if resp.SkippedEmpty {
		fmt.Printf("no changes since checkpoint %d, so no new checkpoint was created\n", resp.ID)
		return nil
	}
	tag := ""
	if resp.SettleTimedOut {
		tag = " (settle timed out; trailing writes may be mid-operation)"
	}
	name := ""
	if *nameFlag != "" {
		name = fmt.Sprintf(" %q", *nameFlag)
	}
	fmt.Printf("checkpoint %d%s %s (%d entries)%s\n", resp.ID, name, resp.Coverage, resp.Entries, tag)
	return nil
}

func cmdUndo(args []string) error {
	fs := flag.NewFlagSet("undo", flag.ExitOnError)
	rootFlag := fs.String("root", "", "protected root (default: current directory)")
	storeFlag := fs.String("store", "", "store directory (default: derived from root path)")
	onlyFlag := fs.String("only", "", "comma-separated relative paths to limit the undo to")
	saveBoth := fs.Bool("save-both", false, "for each conflict, write the checkpoint version alongside the live file (<file>.checkpoint-<id>); the live file is never modified")
	fs.Parse(args)
	if fs.NArg() != 0 {
		return fmt.Errorf("undo: unexpected argument %q (this command takes only flags)", fs.Arg(0))
	}
	root := *rootFlag
	if root == "" {
		if wd, err := os.Getwd(); err == nil {
			root = wd
		}
	}
	root, err := resolveDir(root)
	if err != nil {
		return err
	}
	storeDir, err := resolveStore(*storeFlag, root)
	if err != nil {
		return err
	}
	if err := requireStoreFor(storeDir, root); err != nil {
		return err
	}
	oc, err := objstore.Open(storeDir)
	if err != nil {
		return err
	}
	// An interrupted undo is finished by REPLAYING its journal, never by
	// recomputing a plan: the interrupted run's own writes already shifted the
	// provenance window, so a recomputed plan would be empty and the "safe
	// rerun" would silently abandon the remaining actions.
	if op, interrupted := oplog.CheckInterrupted(storeDir); interrupted {
		if op != nil && op.Kind == "undo" {
			fmt.Printf("finishing interrupted undo: %s\n", oplog.Describe(op))
			res := undo.ReplayJournal(op.Actions, oc)
			fmt.Printf("replay: restored %d, removed %d\n", len(res.Restored), len(res.Deleted))
			for _, e := range res.Errors {
				fmt.Fprintf(os.Stderr, "  error: %s\n", e)
			}
			if len(res.Errors) > 0 {
				return fmt.Errorf("replay completed with %d error(s); the journal is kept, so rerun undo to retry", len(res.Errors))
			}
			if err := oplog.Done(storeDir); err != nil {
				return err
			}
			fmt.Println("interrupted undo completed; rerun undo if you want to undo further")
			return nil
		}
		fmt.Printf("note: %s\n", oplog.Describe(op))
	}
	// Undo targets the latest AGENT TURN, not merely the latest manifest: its
	// own bookkeeping cuts (`pre-undo`, and the `pre-restore` a restore makes)
	// are checkpoint's records of what it was about to do, never turns the user
	// performed. Counting them made selective undo ONE-SHOT: after
	// `undo --only a`, the pre-undo cut became `latest`, so `undo --only b`
	// found an empty window and silently did nothing.
	latest, ok, err := latestTurn(storeDir)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Println("nothing to undo (no checkpoints yet)")
		return nil
	}
	// Baseline = the checkpoint before the latest (the pre-turn state). nil if the
	// latest is the first checkpoint.
	var baseline *store.Manifest
	baseTime := int64(0)
	if latest.ID > 0 {
		// The pre-turn state is the newest DURABLE checkpoint BELOW the target
		// that is not bookkeeping: id-1 may be a pre-undo/pre-restore cut from
		// an earlier partial undo, whose tree already has some of the turn's
		// changes reverted.
		if b, ok := manifestBelow(storeDir, latest.ID); ok {
			baseline, baseTime = b, b.TimeNS
		}
	}
	// The provenance window: every captured write since the baseline.
	all, err := versionlog.Read(filepath.Join(storeDir, "versionlog"))
	if err != nil {
		return err
	}
	var window []versionlog.Version
	for _, v := range all {
		if v.TimeNS > baseTime {
			window = append(window, v)
		}
	}

	// Extra protected folders are recorded on the checkpoints themselves; the
	// undo covers every root the latest checkpoint covers.
	var extraRoots []string
	for r := range latest.Extra {
		extraRoots = append(extraRoots, r)
	}
	sort.Strings(extraRoots)

	only, _, err := parseOnly(*onlyFlag, flagWasSet(fs, "only"), root)
	if err != nil {
		return fmt.Errorf("undo: %w", err)
	}
	// Build every root's plan BEFORE mutating anything, so the whole operation
	// can be journaled first: an interrupted undo must be diagnosable and safely
	// retryable. --only is workspace-scoped, and that exclusion is stated in the
	// OUTPUT whenever it actually bites, not just in the help text.
	type rootPlan struct {
		root string
		plan *undo.Plan
	}
	plans := []rootPlan{{root, undo.BuildPlan(baseline, window, root, only)}}
	if len(only) > 0 && len(extraRoots) > 0 {
		fmt.Printf("note: --only is workspace-scoped; %d extra protected folder(s) excluded from this undo\n", len(extraRoots))
	}
	if len(only) == 0 {
		for _, r := range extraRoots {
			var exBase *store.Manifest
			if baseline != nil {
				if entries, ok := baseline.Extra[r]; ok {
					exBase = &store.Manifest{Entries: entries}
				}
			}
			plans = append(plans, rootPlan{r, undo.BuildPlan(exBase, window, r, nil)})
		}
	}

	// Conflict floor, FAIL-FAST form: unresolved conflicts abort BEFORE any
	// mutation. Failing after reverting (the earlier shape) advanced "latest"
	// via the pre-undo checkpoint, so the advertised "rerun with --save-both"
	// silently no-opped against the wrong turn. Nothing has been changed at
	// this point, so the rerun genuinely targets the same turn.
	var conflicts []string
	conflictWith := map[string]undo.Other{}
	for i, rp := range plans {
		for _, e := range rp.plan.Entries {
			if e.Action == undo.Conflict {
				name := e.Path
				if i == 0 {
					name = e.Rel
				}
				conflicts = append(conflicts, name)
				conflictWith[name] = e.Other
			}
		}
	}
	if len(conflicts) > 0 && !*saveBoth {
		for _, c := range conflicts {
			fmt.Printf("  needs review (%s): %s\n", reviewNote(conflictWith[c]), c)
		}
		return fmt.Errorf("%d file(s) need review; NOTHING was changed. Rerun with --save-both to revert the rest and keep each conflict's checkpoint version alongside", len(conflicts))
	}

	// The saved side of a conflict is what undo would have restored: the
	// PRE-TURN state, i.e. the baseline checkpoint's version. The sibling is
	// named after that id, so its content is exactly what
	// `restore --only <file> <id>` produces.
	suffix := ".checkpoint-version"
	if baseline != nil {
		suffix = fmt.Sprintf(".checkpoint-%d", baseline.ID)
	}
	// The journal carries every mutation (reverts, deletes, and save-both
	// siblings) WITH its payload, so an interrupted run replays from the
	// journal alone (see the replay block above).
	var acts []oplog.Action
	for _, rp := range plans {
		for _, e := range rp.plan.Entries {
			switch e.Action {
			case undo.Restore:
				acts = append(acts, oplog.Action{Do: "restore-file", Path: e.Path,
					Kind: e.Target.Kind, Ref: e.Target.Ref, Mode: e.Target.Mode, Link: e.Target.Link})
			case undo.Delete:
				acts = append(acts, oplog.Action{Do: "delete", Path: e.Path})
			case undo.Conflict:
				if *saveBoth && e.Target != nil {
					acts = append(acts, oplog.Action{Do: "save-both", Path: e.Path + suffix,
						Kind: e.Target.Kind, Ref: e.Target.Ref, Mode: e.Target.Mode, Link: e.Target.Link})
				}
			}
		}
	}

	// Auto-checkpoint the present before mutating, so this undo is itself
	// undoable, but ONLY when something will actually be mutated (an empty
	// undo must not advance "latest"). The daemon's autonomous cuts (setup /
	// baseline rescans) can race this id, so a taken id is retried, never
	// overwritten (store.Write refuses to replace).
	var present *store.Manifest
	if len(acts) > 0 {
		for attempt := 0; attempt < 3 && present == nil; attempt++ {
			preID, err := store.NextID(storeDir)
			if err != nil {
				return err
			}
			m, err := store.Snapshot(root, oc, latest, preID, time.Now().UnixNano(), store.DURABLE, 0, extraRoots...)
			if err != nil {
				return err
			}
			m.Source = "pre-undo"
			switch err := store.Write(storeDir, m); {
			case err == nil:
				present = m
			case errors.Is(err, os.ErrExist):
				continue // the daemon cut this id meanwhile; take the next one
			default:
				return err
			}
		}
		if present == nil {
			return fmt.Errorf("could not cut the pre-undo checkpoint (id contention with the daemon); retry")
		}
		if err := oplog.Begin(storeDir, oplog.Op{Kind: "undo", Root: root, CheckpointID: latest.ID, Only: only, Actions: acts}); err != nil {
			return err
		}
	}

	res := undo.Apply(plans[0].plan, oc, plans[0].root)
	for _, rp := range plans[1:] {
		exRes := undo.Apply(rp.plan, oc, rp.root)
		res.Restored = append(res.Restored, prefixAll(rp.root, exRes.Restored)...)
		res.Deleted = append(res.Deleted, prefixAll(rp.root, exRes.Deleted)...)
		res.Conflicts = append(res.Conflicts, prefixAll(rp.root, exRes.Conflicts)...)
		// Extra roots report absolute paths, so the reason map must be rekeyed
		// to match what is printed; otherwise every extra-root conflict would
		// fall through to the unknown-writer wording.
		for rel, other := range exRes.ConflictWith {
			if res.ConflictWith == nil {
				res.ConflictWith = map[string]undo.Other{}
			}
			res.ConflictWith[filepath.Join(rp.root, rel)] = other
		}
		res.Errors = append(res.Errors, exRes.Errors...)
	}

	fmt.Printf("undo of checkpoint %d: reverted %d, removed %d, skipped %d for review\n",
		latest.ID, len(res.Restored), len(res.Deleted), len(res.Conflicts))
	for _, c := range res.Conflicts {
		fmt.Printf("  needs review (%s, left untouched): %s\n", reviewNote(res.ConflictWith[c]), c)
	}
	if *saveBoth && len(res.Conflicts) > 0 {
		for _, rp := range plans {
			saved, skippedNoBase, kept, errs := undo.MaterializeConflicts(rp.plan, oc, suffix)
			for _, s := range saved {
				fmt.Printf("  saved checkpoint version alongside: %s\n", s)
			}
			for _, s := range kept {
				fmt.Printf("  existing sibling left untouched (it may hold your merge): %s\n", s)
			}
			for _, s := range skippedNoBase {
				fmt.Printf("  no checkpoint version to save for %s (created this turn); live file kept\n", s)
			}
			res.Errors = append(res.Errors, errs...)
		}
	}
	// The journal clears only after EVERY mutation has succeeded (reverts,
	// deletes, and save-both siblings); the kept-journal message is then true.
	if len(res.Errors) == 0 && len(acts) > 0 {
		if err := oplog.Done(storeDir); err != nil {
			return err
		}
	}
	if present != nil {
		fmt.Printf("pre-undo checkpoint %d saved (restore it to undo this undo)\n", present.ID)
	} else if len(res.Conflicts) == 0 {
		fmt.Println("nothing to revert (no agent-only changes in the latest turn)")
		// On a filesystem without the dirent change feed (overlayfs, the
		// Docker default), deletions carry no provenance, so a file the agent
		// deleted is invisible to undo. Saying only "nothing to revert" there
		// reads as "nothing was deleted", which is the opposite of the truth.
		// Name the missing files and the command that actually gets them back.
	}
	// Deletions the filesystem cannot attribute are invisible to undo whether
	// or not it reverted anything else, so report them either way (comparing
	// against the PRE-TURN baseline: a file deleted during the turn is already
	// absent from the turn's own manifest).
	delRef := baseline
	if delRef == nil {
		delRef = latest
	}
	reportUnattributableDeletions(storeDir, root, delRef)
	for _, e := range res.Errors {
		fmt.Fprintf(os.Stderr, "  error: %s\n", e)
	}
	if len(res.Errors) > 0 {
		fmt.Fprintf(os.Stderr, "  the operation journal is kept (%s); rerunning undo replays the remaining actions\n",
			filepath.Join(storeDir, "operation.json"))
		return fmt.Errorf("undo completed with %d error(s)", len(res.Errors))
	}
	return nil
}

func cmdHistory(args []string) error {
	fs := flag.NewFlagSet("history", flag.ExitOnError)
	rootFlag := fs.String("root", "", "protected root (default: current directory)")
	storeFlag := fs.String("store", "", "store directory (default: derived from root path)")
	jsonFlag := fs.Bool("json", false, "emit JSON (a pure client contract; the TUI reads this)")
	fs.Parse(args)
	if fs.NArg() != 0 {
		return fmt.Errorf("history: unexpected argument %q (this command takes only flags)", fs.Arg(0))
	}
	root := *rootFlag
	if root == "" {
		if wd, err := os.Getwd(); err == nil {
			root = wd
		}
	}
	root, err := resolveDir(root)
	if err != nil {
		return err
	}
	storeDir, err := resolveStore(*storeFlag, root)
	if err != nil {
		return err
	}
	warnStoreFor(storeDir, root)
	ids, err := store.IDs(storeDir)
	if err != nil {
		return err
	}

	// Newest first: the most recent checkpoint is the one a user is looking for.
	type row struct {
		ID             int               `json:"id"`
		TimeNS         int64             `json:"time_ns"`
		Badge          string            `json:"badge"`
		Source         string            `json:"source"`
		Name           string            `json:"name"` // "" when unnamed (always present; client contract)
		SettleTimedOut bool              `json:"settle_timed_out"`
		Missed         int               `json:"missed"`
		Exceptions     []store.Exception `json:"exceptions"`
	}
	rows := []row{} // never null in --json: "checkpoints" is [] on an empty store (client contract)
	for i := len(ids) - 1; i >= 0; i-- {
		m, err := store.Load(storeDir, ids[i])
		if err != nil {
			continue
		}
		exc := m.Exceptions
		if exc == nil {
			exc = []store.Exception{} // never null (client contract)
		}
		rows = append(rows, row{m.ID, m.TimeNS, status.Of(m).String(), m.Source, m.Name, m.SettleTimedOut, m.Missed, exc})
	}

	if *jsonFlag {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"checkpoints": rows})
	}
	if len(rows) == 0 {
		fmt.Println("no checkpoints yet")
		return nil
	}
	for _, r := range rows {
		when := time.Unix(0, r.TimeNS).Format("2006-01-02 15:04:05")
		note := ""
		if r.SettleTimedOut {
			note = "  [settle timed out]"
		}
		if r.Missed > 0 {
			note += fmt.Sprintf("  [%d file(s) uncaptured]", r.Missed)
		}
		src := r.Source
		if src == "" {
			src = "(unlabeled)"
		}
		if r.Name != "" {
			src += fmt.Sprintf("  name:%q", r.Name)
		}
		fmt.Printf("#%-3d %s  %-28s  %s%s\n", r.ID, when, r.Badge, src, note)
		for _, ex := range r.Exceptions {
			fmt.Printf("      ! %s (%s)\n", ex.Path, ex.Reason)
		}
	}
	return nil
}

func cmdPrune(args []string) error {
	fs := flag.NewFlagSet("prune", flag.ExitOnError)
	rootFlag := fs.String("root", "", "protected root (default: current directory)")
	storeFlag := fs.String("store", "", "store directory (default: derived from root path)")
	keepDays := fs.Int("keep-days", 7, "keep unnamed checkpoints newer than this many days (>= 0)")
	dryRun := fs.Bool("dry-run", false, "report what would be deleted; delete nothing")
	yesFlag := fs.Bool("yes", false, "skip the confirmation (noninteractive)")
	fs.Parse(args)
	if fs.NArg() != 0 {
		return fmt.Errorf("prune: unexpected argument %q (this command takes only flags)", fs.Arg(0))
	}
	// A negative retention window is meaningless and reads as "keep nothing";
	// refuse rather than silently treating it as some cutoff.
	if *keepDays < 0 {
		return fmt.Errorf("prune: --keep-days must be >= 0 (got %d)", *keepDays)
	}
	root := *rootFlag
	if root == "" {
		if wd, err := os.Getwd(); err == nil {
			root = wd
		}
	}
	root, err := resolveDir(root)
	if err != nil {
		return err
	}
	storeDir, err := resolveStore(*storeFlag, root)
	if err != nil {
		return err
	}
	if err := requireStoreFor(storeDir, root); err != nil {
		return err
	}
	// Exclusive-access gate: a live daemon holds the versionlog, cuts manifests,
	// and reuses prior refs incrementally, so pruning under it would fork the
	// log and race the GC against in-flight cuts.
	if _, err := daemon.RequestStatus(daemon.SocketPath(storeDir)); err == nil {
		return fmt.Errorf("prune: stop the daemon first, because prune requires exclusive store access")
	}
	oc, err := objstore.Open(storeDir)
	if err != nil {
		return err
	}
	if op, interrupted := oplog.CheckInterrupted(storeDir); interrupted {
		fmt.Printf("note: %s\n", oplog.Describe(op))
	}
	// Plan with a DryRun pass first: the confirmation and the journal must show
	// what will actually be deleted, and the journal must exist BEFORE the first
	// mutation. The same NowNS is reused so the real pass deletes exactly the
	// plan (nothing else can move: the daemon is stopped).
	now := time.Now().UnixNano()
	plan, err := store.Prune(storeDir, oc, store.PruneOpts{KeepDays: *keepDays, NowNS: now, DryRun: true})
	if err != nil {
		return err
	}
	if len(plan.RemovedManifests) == 0 && plan.RemovedObjects == 0 && plan.ExpiredVersions == 0 {
		if len(plan.KeptNamed) > 0 {
			fmt.Printf("nothing to prune (%d named checkpoint(s) old enough are kept: %s)\n",
				len(plan.KeptNamed), idList(plan.KeptNamed))
		} else {
			fmt.Println("nothing to prune")
		}
		return nil
	}
	if *dryRun {
		fmt.Printf("dry run: nothing deleted. A real prune would remove:\n")
		printPrunePlan(plan, *keepDays)
		return nil
	}
	if err := confirmPrune(plan, *keepDays, *yesFlag); err != nil {
		return err
	}
	// Journal-before-mutation (coarse actions: the manifests by id, one GC
	// summary). Rerunning prune after an interruption completes the pass:
	// removed manifests stop being candidates and GC is idempotent.
	var acts []oplog.Action
	for _, id := range plan.RemovedManifests {
		acts = append(acts, oplog.Action{Do: "delete-manifest",
			Path: filepath.Join(storeDir, "manifests", fmt.Sprintf("%d.json", id))})
	}
	acts = append(acts, oplog.Action{Do: "gc", Path: storeDir})
	if err := oplog.Begin(storeDir, oplog.Op{Kind: "prune", Root: root, Actions: acts}); err != nil {
		return err
	}
	rep, err := store.Prune(storeDir, oc, store.PruneOpts{KeepDays: *keepDays, NowNS: now})
	if err != nil {
		fmt.Fprintf(os.Stderr, "the operation journal is kept (%s); rerunning prune completes it (GC is idempotent)\n",
			filepath.Join(storeDir, "operation.json"))
		return err
	}
	if err := oplog.Done(storeDir); err != nil {
		return err
	}
	fmt.Printf("removed %d checkpoint(s)%s\n", len(rep.RemovedManifests), idListSuffix(rep.RemovedManifests))
	if len(rep.KeptNamed) > 0 {
		fmt.Printf("kept %d named checkpoint(s) old enough to prune: %s\n", len(rep.KeptNamed), idList(rep.KeptNamed))
	}
	fmt.Printf("removed %d object(s), %d bytes reclaimed\n", rep.RemovedObjects, rep.RemovedBytes)
	fmt.Printf("%d recovered-file entries expired\n", rep.ExpiredVersions)
	return nil
}

// printPrunePlan renders the plan's real counts + bytes (shared by --dry-run
// and the confirmation).
func printPrunePlan(rep store.PruneReport, keepDays int) {
	fmt.Printf("  %d checkpoint(s) older than %d day(s)%s\n",
		len(rep.RemovedManifests), keepDays, idListSuffix(rep.RemovedManifests))
	fmt.Printf("  %d unreferenced object(s), %d bytes\n", rep.RemovedObjects, rep.RemovedBytes)
	fmt.Printf("  %d recovered-file entr%s would expire\n", rep.ExpiredVersions, plural(rep.ExpiredVersions, "y", "ies"))
	if len(rep.KeptNamed) > 0 {
		fmt.Printf("  (named checkpoints kept despite age: %s)\n", idList(rep.KeptNamed))
	}
}

// confirmPrune shows the real counts and bytes a prune will delete and requires
// an explicit yes. Noninteractive runs must pass --yes; a non-terminal stdin
// without it is a refusal, never a silent proceed (the include-extra pattern).
func confirmPrune(rep store.PruneReport, keepDays int, yes bool) error {
	fmt.Printf("prune will permanently delete from the store:\n")
	printPrunePlan(rep, keepDays)
	if yes {
		return nil
	}
	if fi, err := os.Stdin.Stat(); err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return fmt.Errorf("refusing to prune without confirmation: rerun with --yes (or preview with --dry-run)")
	}
	fmt.Printf("delete these? [y/N] ")
	var answer string
	if _, err := fmt.Fscanln(os.Stdin, &answer); err != nil {
		return fmt.Errorf("no confirmation received: rerun with --yes for noninteractive use")
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return nil
	}
	return fmt.Errorf("aborted: nothing pruned")
}

// idList renders manifest ids compactly (first 10 + count).
func idList(ids []int) string {
	var parts []string
	show := len(ids)
	if show > 10 {
		show = 10
	}
	for _, id := range ids[:show] {
		parts = append(parts, fmt.Sprintf("#%d", id))
	}
	s := strings.Join(parts, " ")
	if len(ids) > show {
		s += fmt.Sprintf(" … and %d more", len(ids)-show)
	}
	return s
}

// idListSuffix is idList as a ": #a #b" suffix, empty for an empty list.
func idListSuffix(ids []int) string {
	if len(ids) == 0 {
		return ""
	}
	return ": " + idList(ids)
}

// cmdTrampoline stops itself, then execs the real command in place (same pid).
// The parent `run` registers this pid as an agent root while it is stopped and
// SIGCONTs it, so every descendant, however short-lived, is born with the
// birth-parent scanner already active.
func cmdTrampoline(args []string) error {
	if len(args) < 2 || args[0] != "--" {
		return fmt.Errorf("__trampoline: internal use only")
	}
	argv := args[1:]
	path, err := exec.LookPath(argv[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %s: command not found\n", prog(), argv[0])
		os.Exit(127)
	}
	syscall.Kill(os.Getpid(), syscall.SIGSTOP) // parent SIGCONTs after registering
	return syscall.Exec(path, argv, os.Environ())
}

// waitStopped polls /proc until pid reaches the stopped state (the trampoline's
// self-SIGSTOP has landed).
func waitStopped(pid int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			return err
		}
		s := string(b)
		if i := strings.LastIndexByte(s, ')'); i >= 0 && len(s) > i+2 {
			if st := s[i+2]; st == 'T' || st == 't' {
				return nil
			}
		}
		time.Sleep(time.Millisecond)
	}
	return fmt.Errorf("pid %d not stopped within %s", pid, timeout)
}

// agoOrNone renders a checkpoint timestamp as a worst-case-loss age ("how much
// could you lose right now"), or "none yet".
func agoOrNone(ns int64) string {
	if ns <= 0 {
		return "none yet"
	}
	d := time.Since(time.Unix(0, ns)).Round(time.Second)
	if d < 0 {
		d = 0
	}
	return fmt.Sprintf("%s ago", d)
}

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

// checkSocketPath refuses a store whose socket path would exceed the kernel's
// sockaddr_un limit. The failure is otherwise a bare "bind: invalid argument"
// arriving after a 15-second readiness wait, which tells the user nothing.
func checkSocketPath(storeDir string) error {
	const sunPathMax = 108 // sizeof(sockaddr_un.sun_path), including the NUL
	sock := daemon.SocketPath(storeDir)
	if len(sock)+1 > sunPathMax {
		return fmt.Errorf("store path is too long for a Unix socket: %s would need %d bytes and the "+
			"kernel allows %d.\n  use a shorter --store (e.g. --store %s)",
			sock, len(sock)+1, sunPathMax, filepath.Join("/tmp", "ckpt-"+filepath.Base(storeDir)))
	}
	return nil
}

// cmdSelftest exercises the product's real guarantees against a throwaway
// workspace on the user's own machine, through this very binary. `doctor` says
// whether the environment looks right; selftest proves whether recovery
// actually works here, which is the only claim that matters on a machine we
// have never seen. --json emits the report a bug report should attach.
func cmdSelftest(args []string) error {
	fs := flag.NewFlagSet("selftest", flag.ExitOnError)
	jsonFlag := fs.Bool("json", false, "emit the machine-readable report (what a bug report should attach)")
	keepFlag := fs.Bool("keep", false, "keep the scratch directory for inspection")
	workFlag := fs.String("work", "", "scratch directory (default: the current directory, so the test runs on the filesystem your projects actually live on)")
	fs.Parse(args)
	if fs.NArg() != 0 {
		return fmt.Errorf("selftest: unexpected argument %q (this command takes only flags)", fs.Arg(0))
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	// The scratch directory decides WHICH FILESYSTEM gets tested, and that is
	// the whole point on an unfamiliar machine: /tmp is frequently tmpfs or
	// overlayfs while the user's projects live on ext4, and the guarantees
	// differ between them. Default to the current directory (where their code
	// is), and say so; fall back to /tmp only when that will not work, naming
	// the reason.
	base := *workFlag
	note := ""
	if base == "" {
		if wd, werr := os.Getwd(); werr == nil {
			base = wd
		} else {
			base = "/tmp"
		}
	}
	work, err := os.MkdirTemp(base, ".checkpoint-selftest-")
	if err != nil {
		work, err = os.MkdirTemp("/tmp", "ckself")
		if err != nil {
			return err
		}
		note = fmt.Sprintf("scratch fell back to /tmp (%s is not writable): the filesystem under test is /tmp's, not %s's", base, base)
	}
	// A long scratch path breaks the daemon's Unix socket; fall back rather
	// than fail, and be explicit that the tested filesystem changed.
	if err := checkSocketPath(filepath.Join(work, "stores", "primary")); err != nil {
		os.RemoveAll(work)
		work, err = os.MkdirTemp("/tmp", "ckself")
		if err != nil {
			return err
		}
		note = "scratch fell back to /tmp (the original path was too long for a Unix socket): the filesystem under test is /tmp's"
	}
	if note != "" {
		fmt.Fprintf(os.Stderr, "%s: note: %s\n", prog(), note)
	}
	if !*keepFlag {
		defer os.RemoveAll(work)
	}
	rep := selftest.Run(self, work)
	if *jsonFlag {
		b, err := rep.JSON()
		if err != nil {
			return err
		}
		fmt.Println(string(b))
	} else {
		fmt.Print(rep.Text())
		if *keepFlag {
			fmt.Printf("\nscratch kept at %s\n", work)
		}
	}
	if !rep.OK() {
		return fmt.Errorf("selftest: at least one guarantee FAILED on this machine (details above)")
	}
	return nil
}

// cmdDoctor answers the question a stranger machine actually poses: will this
// work here, and if not, what do I do about it? Every failing check carries a
// remedy, and the exit status is usable from a setup script.
func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	rootFlag := fs.String("root", "", "project to check (default: current directory)")
	storeFlag := fs.String("store", "", "store directory (default: derived from root path)")
	fs.Parse(args)
	if fs.NArg() != 0 {
		return fmt.Errorf("doctor: unexpected argument %q (this command takes only flags)", fs.Arg(0))
	}
	root := *rootFlag
	if root == "" {
		if wd, err := os.Getwd(); err == nil {
			root = wd
		}
	}
	root, err := resolveDir(root)
	if err != nil {
		return err
	}
	// Resolve the store WITHOUT letting the in-tree guard abort: a bad store
	// location is one of the things doctor exists to REPORT, not to die on.
	storeDir := *storeFlag
	if storeDir == "" {
		if d, derr := resolveStore("", root); derr == nil {
			storeDir = d
		} else {
			storeDir = filepath.Join(root, ".checkpoint-store")
		}
	} else if abs, aerr := filepath.Abs(storeDir); aerr == nil {
		storeDir = abs
	}
	rep := doctor.Run(root, storeDir)
	fmt.Print(rep.Text())
	if !rep.Healthy() {
		return fmt.Errorf("this machine cannot run checkpoint as configured (see the remedies above)")
	}
	return nil
}

// cmdUI runs the TUI, a pure client of this binary's own --json output. The
// TUI can render nothing the JSON contract doesn't carry, which keeps the
// contract honest: anything the UI needs, a script can get too.
func cmdUI(args []string) error {
	fs := flag.NewFlagSet("ui", flag.ExitOnError)
	rootFlag := fs.String("root", "", "protected root (default: current directory)")
	storeFlag := fs.String("store", "", "store directory (default: derived from root path)")
	fs.Parse(args)
	if fs.NArg() != 0 {
		return fmt.Errorf("ui: unexpected argument %q (this command takes only flags)", fs.Arg(0))
	}
	root := *rootFlag
	if root == "" {
		if wd, err := os.Getwd(); err == nil {
			root = wd
		}
	}
	root, err := resolveDir(root)
	if err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	return tui.Run(tui.NewCLIClient(self, root, *storeFlag))
}

// cmdProtect establishes standing protection: it starts the daemon DETACHED
// (own session, logs to <store>/daemon.log, pid recorded in <store>/daemon.pid)
// and confirms protection before returning. --stop tears it down. The daemon
// subcommand stays for foreground/supervised use; protect is the everyday form.
func cmdProtect(args []string) error {
	fs := flag.NewFlagSet("protect", flag.ExitOnError)
	storeFlag := fs.String("store", "", "store directory (default: derived from root path)")
	protectFlag := fs.String("protect", "", "comma-separated additional folders to protect (absolute paths)")
	stopFlag := fs.Bool("stop", false, "stop the standing daemon for this root")
	fs.Parse(args)
	root := ""
	if fs.NArg() > 1 {
		return fmt.Errorf("protect: expected at most one <root>")
	}
	if fs.NArg() == 1 {
		root = fs.Arg(0)
	}
	if root == "" {
		if wd, err := os.Getwd(); err == nil {
			root = wd
		}
	}
	root, err := resolveDir(root)
	if err != nil {
		return err
	}
	storeDir, err := resolveStore(*storeFlag, root)
	if err != nil {
		return err
	}
	if err := checkSocketPath(storeDir); err != nil {
		return err
	}
	sock := daemon.SocketPath(storeDir)
	pidFile := filepath.Join(storeDir, "daemon.pid")

	if *stopFlag {
		b, err := os.ReadFile(pidFile)
		if err != nil {
			if _, serr := daemon.RequestStatus(sock); serr != nil {
				fmt.Println("not protected (no standing daemon)")
				return nil
			}
			return fmt.Errorf("protect --stop: a daemon answers on %s but %s is missing; it was started in the foreground, so stop it there", sock, pidFile)
		}
		var pid int
		if _, err := fmt.Sscanf(strings.TrimSpace(string(b)), "%d", &pid); err != nil || pid <= 0 {
			return fmt.Errorf("protect --stop: %s is corrupt; stop the daemon manually", pidFile)
		}
		if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := daemon.RequestStatus(sock); err != nil {
				os.Remove(pidFile)
				fmt.Printf("protection stopped for %s\n", root)
				return nil
			}
			time.Sleep(50 * time.Millisecond)
		}
		return fmt.Errorf("protect --stop: daemon (pid %d) did not exit within 10s", pid)
	}

	if st, err := daemon.RequestStatus(sock); err == nil {
		fmt.Printf("already protected (daemon running since %s; %d checkpoint(s))\n",
			time.Unix(0, st.SinceUnixNS).Format("2006-01-02 15:04:05"), st.Checkpoints)
		return nil
	}
	// Statically invalid configurations must fail NOW, not after a 15s readiness
	// wait on a daemon that already exited: nesting is decidable from the paths
	// alone, and the foreground `daemon` rejects it immediately.
	var protectExtras []string
	for _, p := range strings.Split(*protectFlag, ",") {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		abs, err := resolveDir(p)
		if err != nil {
			return err
		}
		if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
			return fmt.Errorf("protect: protected folder %s is not a directory", abs)
		}
		protectExtras = append(protectExtras, abs)
	}
	allRoots := append([]string{root}, protectExtras...)
	for i, a := range allRoots {
		for j, b := range allRoots {
			if i != j && (a == b || strings.HasPrefix(a, b+string(filepath.Separator))) {
				return fmt.Errorf("protect: protected folders must not nest: %s is under %s", a, b)
			}
		}
	}
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	logF, err := os.OpenFile(filepath.Join(storeDir, "daemon.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer logF.Close()
	dArgs := []string{"daemon", "--store", storeDir}
	if len(protectExtras) > 0 {
		dArgs = append(dArgs, "--protect", strings.Join(protectExtras, ","))
	}
	dArgs = append(dArgs, root)
	c := exec.Command(self, dArgs...)
	c.Stdout, c.Stderr = logF, logF
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // survives this shell/session
	if err := c.Start(); err != nil {
		return err
	}
	if err := os.WriteFile(pidFile, []byte(fmt.Sprintf("%d\n", c.Process.Pid)), 0o600); err != nil {
		return err
	}
	go c.Wait() // reap if it dies before we detach
	// Confirm protection (or fail with the daemon's own words) before returning.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if st, err := daemon.RequestStatus(sock); err == nil {
			state := "protected"
			if st.SettingUp {
				state = "setting up (first scan running)"
			} else if !st.BaselineComplete {
				state = "limited (baseline incomplete; auto-rescan running)"
			}
			fmt.Printf("protection started for %s: %s [log: %s]\n", root, state, filepath.Join(storeDir, "daemon.log"))
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	b, _ := os.ReadFile(filepath.Join(storeDir, "daemon.log"))
	tail := string(b)
	if len(tail) > 500 {
		tail = tail[len(tail)-500:]
	}
	os.Remove(pidFile)
	return fmt.Errorf("daemon did not become ready within 15s; log tail:\n%s", tail)
}

// latestTurn returns the newest DURABLE checkpoint that represents real work,
// skipping checkpoint's own bookkeeping cuts. Baseline selection then walks
// down from it the same way, so a partially-undone turn stays addressable
// until every selected path has been handled.
func latestTurn(storeDir string) (*store.Manifest, bool, error) {
	ids, err := store.ValidIDs(storeDir)
	if err != nil {
		return nil, false, err
	}
	for i := len(ids) - 1; i >= 0; i-- {
		m, err := store.Load(storeDir, ids[i])
		if err != nil || m.Coverage != store.DURABLE {
			continue
		}
		if isBookkeeping(m.Source) {
			continue
		}
		return m, true, nil
	}
	return nil, false, nil
}

// manifestBelow returns the newest non-bookkeeping DURABLE checkpoint with an
// id below id, which is the pre-turn state for the turn at id.
func manifestBelow(storeDir string, id int) (*store.Manifest, bool) {
	ids, err := store.ValidIDs(storeDir)
	if err != nil {
		return nil, false
	}
	for i := len(ids) - 1; i >= 0; i-- {
		if ids[i] >= id {
			continue
		}
		m, err := store.Load(storeDir, ids[i])
		if err != nil || m.Coverage != store.DURABLE || isBookkeeping(m.Source) {
			continue
		}
		return m, true
	}
	return nil, false
}

// isBookkeeping reports whether a checkpoint records what checkpoint itself was
// about to do, rather than work someone did.
func isBookkeeping(source string) bool {
	return source == "pre-undo" || source == "pre-restore"
}

// reportUnattributableDeletions surfaces files that exist in the target
// checkpoint but are gone from disk, when this filesystem cannot attribute
// deletions. Undo legitimately cannot restore them (it never reverts what it
// cannot prove the agent did), but the user must not be left thinking nothing
// is missing: restore-by-path can bring them back.
func reportUnattributableDeletions(storeDir, root string, m *store.Manifest) {
	if st, err := daemon.RequestStatus(daemon.SocketPath(storeDir)); err == nil && st.FeedActive {
		return // deletions ARE attributed here; undo already handled them
	}
	var missing []string
	for rel, e := range m.Entries {
		if e.Kind == store.KindDir {
			continue
		}
		if _, err := os.Lstat(filepath.Join(root, rel)); os.IsNotExist(err) {
			missing = append(missing, rel)
		}
	}
	if len(missing) == 0 {
		return
	}
	sort.Strings(missing)
	shown := missing
	if len(shown) > 10 {
		shown = shown[:10]
	}
	fmt.Printf("\nnote: %d file(s) in checkpoint %d are missing from the workspace. This\n"+
		"filesystem cannot attribute deletions (no change feed), so undo cannot tell whether\n"+
		"the agent or you deleted them, and it never reverts what it cannot prove:\n", len(missing), m.ID)
	for _, rel := range shown {
		fmt.Printf("  - %q\n", rel)
	}
	if len(missing) > len(shown) {
		fmt.Printf("  … and %d more\n", len(missing)-len(shown))
	}
	fmt.Printf("  bring one back:  %s restore --only %q %d %s\n", prog(), shown[0], m.ID, root)
	fmt.Printf("  bring all back:  %s restore %d %s   (restores the whole checkpoint)\n", prog(), m.ID, root)
}

// requireStoreFor binds a store to the workspace it protects. A store stamped
// for another project must not be written to or read as this project's history:
// mixing two projects into one history makes "restore checkpoint N" restore
// someone else's tree. Mutating and workspace-semantic commands call
// this; `restore` deliberately does NOT, because restoring a checkpoint into a
// fresh directory is a legitimate, non-destructive use of a foreign store.
func requireStoreFor(storeDir, workspace string) error {
	meta, err := store.EnsureMeta(storeDir, workspace)
	if err != nil {
		return err
	}
	return store.CheckWorkspaceIdentity(meta, workspace)
}

// warnStoreFor is the read-only counterpart: a mismatch is surfaced but does
// not block diagnostics (history/status must stay usable for figuring out what
// a store actually holds).
func warnStoreFor(storeDir, workspace string) {
	meta, err := store.LoadMeta(storeDir)
	if err != nil {
		return
	}
	if err := store.CheckWorkspaceIdentity(meta, workspace); err != nil {
		fmt.Fprintf(os.Stderr, "%s: WARNING: %v\n", prog(), err)
	}
}

// flagWasSet reports whether the user actually passed a flag, as opposed to it
// carrying its zero default. Go's flag package cannot distinguish `--only ”`
// from an absent --only by value alone, and that distinction is load-bearing:
// an explicitly empty selector must refuse, never widen into a full destructive
// operation.
func flagWasSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

// parseCheckpointID accepts a COMPLETE unsigned decimal id and nothing else.
// fmt.Sscanf("%d") stops at the first non-digit, so "0junk", "1.0", "1/2" and
// "0x1" would silently parse as a numeric prefix and restore the WRONG
// checkpoint destructively. A typo must fail, not guess.
func parseCheckpointID(s string) (int, error) {
	n, err := strconv.ParseUint(s, 10, 31)
	if err != nil {
		return 0, fmt.Errorf("bad checkpoint id %q: expected a whole number like 0, 1, 2", s)
	}
	return int(n), nil
}

// resolveDir turns a user-supplied directory argument into an absolute path
// with symlinks resolved. A symlinked path is an alias for its target
// everywhere in this CLI: leaving it unresolved lets a symlinked project root
// snapshot NOTHING while reporting "Fully recoverable", and lets a store that
// resolves inside the project slip past the in-tree guard.
// A path that does not exist yet keeps its (cleaned, absolute) form, because
// restore targets are allowed not to exist.
func resolveDir(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return abs, nil //nolint:nilerr // absent path: nothing to resolve
	}
	return resolved, nil
}

// parseOnly turns a --only value into the selected relative paths. It is the
// single gate for both `undo --only` and `restore --only`, because the failure
// modes are shared and destructive:
//
//   - An EMPTY selector (`--only ”`, trivially produced by an unset shell or
//     CI variable) must NEVER silently widen into "operate on everything": a
//     flag that says "limit this" turning into a full destructive operation is
//     the worst possible reading of the user's intent.
//   - A path outside the workspace must be refused, not silently ignored:
//     `--only ../outside` reporting "nothing to revert" reads as "that file was
//     clean", which is a lie about a path we never considered.
//
// present tells the caller whether --only was given at all (absent = whole
// operation, which is a different thing from an empty selector).
func parseOnly(flagVal string, given bool, root string) (only []string, present bool, err error) {
	if !given {
		return nil, false, nil
	}
	for _, raw := range strings.Split(flagVal, ",") {
		p := strings.TrimSpace(raw)
		if p == "" {
			continue
		}
		if filepath.IsAbs(p) {
			rel, relErr := filepath.Rel(root, p)
			if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return nil, true, fmt.Errorf("--only %s is outside the workspace %s", p, root)
			}
			p = rel
		}
		clean := filepath.Clean(p)
		if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return nil, true, fmt.Errorf("--only %s escapes the workspace %s (paths are workspace-relative)", raw, root)
		}
		only = append(only, clean)
	}
	if len(only) == 0 {
		return nil, true, fmt.Errorf("--only was given but names no paths; refusing to widen an " +
			"explicitly limited operation into a full one (omit --only to operate on everything)")
	}
	return only, true, nil
}

// prefixAll joins each rel path onto root, for reporting extra-root undo results
// unambiguously alongside workspace-relative ones.
func prefixAll(root string, rels []string) []string {
	out := make([]string, len(rels))
	for i, r := range rels {
		out[i] = filepath.Join(root, r)
	}
	return out
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	rootFlag := fs.String("root", "", "protected root (default: current directory)")
	storeFlag := fs.String("store", "", "store directory (default: derived from root path)")
	jsonFlag := fs.Bool("json", false, "emit JSON")
	fs.Parse(args)
	if fs.NArg() != 0 {
		return fmt.Errorf("status: unexpected argument %q (this command takes only flags)", fs.Arg(0))
	}
	root := *rootFlag
	if root == "" {
		if wd, err := os.Getwd(); err == nil {
			root = wd
		}
	}
	root, err := resolveDir(root)
	if err != nil {
		return err
	}
	storeDir, err := resolveStore(*storeFlag, root)
	if err != nil {
		return err
	}
	warnStoreFor(storeDir, root)
	st, err := daemon.RequestStatus(daemon.SocketPath(storeDir))
	if err != nil {
		// Not protected: no daemon running for this project. Honest, not an error.
		ids, _ := store.ValidIDs(storeDir) // a torn manifest is not a checkpoint
		lastNS := int64(0)
		if m, ok, _ := store.Latest(storeDir); ok {
			lastNS = m.TimeNS
		}
		if *jsonFlag {
			// List fields are ALWAYS arrays, even with no daemon: a client
			// doing status.outside.length must never hit null/undefined.
			return json.NewEncoder(os.Stdout).Encode(daemon.Status{
				Protected: false, Root: root, Checkpoints: len(ids), LastCkptNS: lastNS,
				ProtectedDirs: []string{root}, Outside: []string{},
			})
		}
		fmt.Printf("Protection: Not protected (no daemon running for this project)\n")
		fmt.Printf("Root: %s\nStore: %s\nCheckpoints on record: %d\n", root, storeDir, len(ids))
		if u, uerr := store.MeasureUsage(storeDir); uerr == nil {
			fmt.Printf("Storage: %s\n", u.Human())
		}
		fmt.Printf("Last complete checkpoint: %s\n", agoOrNone(lastNS))
		return nil
	}
	if *jsonFlag {
		return json.NewEncoder(os.Stdout).Encode(st)
	}
	state := "Protected"
	if st.Limited {
		state = "Limited protection"
	}
	if st.SettingUp {
		state = "Setting up (first scan still running)"
		if st.SetupScanned > 0 {
			state = fmt.Sprintf("Setting up (first scan running, %d files scanned so far)", st.SetupScanned)
		}
	}
	fmt.Printf("Protection: %s\n", state)
	fmt.Printf("Root: %s\n", st.Root)
	for _, d := range st.ProtectedDirs {
		if d != st.Root {
			fmt.Printf("Also protecting: %s\n", d)
		}
	}
	fmt.Printf("Store: %s\n", storeDir)
	if u, uerr := store.MeasureUsage(storeDir); uerr == nil {
		fmt.Printf("Storage: %s\n", u.Human())
	}
	fmt.Printf("Checkpoints: %d\n", st.Checkpoints)
	fmt.Printf("Last complete checkpoint: %s\n", agoOrNone(st.LastCkptNS))
	if st.BaselineComplete {
		fmt.Println("Complete baseline: yes")
	} else {
		fmt.Println("Complete baseline: NO")
	}
	if st.FeedActive {
		fmt.Println("Change feed: active (delete attribution + change-scaled checkpoints)")
	} else {
		fmt.Println("Change feed: unavailable on this filesystem; delete attribution is unavailable and checkpoints use full scans")
	}
	fmt.Printf("Active agent sessions: %d\n", st.AgentSessions)
	fmt.Printf("Protecting since: %s\n", time.Unix(0, st.SinceUnixNS).Format("2006-01-02 15:04:05"))
	if st.Limited {
		fmt.Println("Why limited:")
		if !st.BaselineComplete {
			fmt.Println("  ! first scan incomplete (event overflow during setup): there is no complete baseline yet, and a clean rescan runs automatically until one succeeds")
		}
		if st.Overflowed {
			fmt.Println("  ! a burst overflowed the watch queue, so some changes since the last checkpoint may be uncaptured (unbounded)")
		}
		if st.Missed > 0 {
			fmt.Printf("  ! %d file(s) since the last checkpoint could not be captured\n", st.Missed)
		}
		if st.OutsideCount > 0 {
			fmt.Printf("  ! unprotected changes: the agent wrote %d time(s) outside the protected folders (not captured, not restorable):\n", st.OutsideCount)
			for _, p := range st.Outside {
				fmt.Printf("      %s\n", p)
			}
			if st.OutsideCount > len(st.Outside) {
				fmt.Printf("      … more paths beyond the %d listed\n", len(st.Outside))
			}
			fmt.Println("      (add folders with: daemon --protect DIR,DIR)")
		}
	}
	// Current exceptions: what the LATEST checkpoint could not cover, by name.
	if m, ok, _ := store.Latest(storeDir); ok && len(m.Exceptions) > 0 {
		fmt.Printf("Current exceptions (checkpoint %d):\n", m.ID)
		for _, ex := range m.Exceptions {
			fmt.Printf("  ! %s (%s)\n", ex.Path, ex.Reason)
		}
	}
	return nil
}

func cmdRegisterAgent(args []string, register bool) error {
	name := "register-agent"
	if !register {
		name = "unregister-agent"
	}
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	rootFlag := fs.String("root", "", "protected root (default: current directory)")
	storeFlag := fs.String("store", "", "store directory (default: derived from root path)")
	pidFlag := fs.Int("pid", 0, "the agent process pid to (un)register as an agent root")
	fs.Parse(args)
	if fs.NArg() != 0 {
		return fmt.Errorf("%s: unexpected argument %q (this command takes only flags)", name, fs.Arg(0))
	}
	if *pidFlag <= 0 {
		return fmt.Errorf("%s: --pid is required", name)
	}
	root := *rootFlag
	if root == "" {
		if wd, err := os.Getwd(); err == nil {
			root = wd
		}
	}
	root, err := resolveDir(root)
	if err != nil {
		return err
	}
	storeDir, err := resolveStore(*storeFlag, root)
	if err != nil {
		return err
	}
	// A registration whose stable identity cannot be read is worthless and
	// actively harmful: it inflates agent_sessions, keeps the provenance
	// scanner spinning, and (with a start-time of 0) would credit whatever
	// process later inherits that pid. Refuse it.
	start, ok := provenance.StartTime(*pidFlag)
	if !ok {
		return fmt.Errorf("%s: no live process with pid %d (its identity cannot be read, so "+
			"writes could never be attributed to it)", name, *pidFlag)
	}
	sock := daemon.SocketPath(storeDir)
	if register {
		if err := daemon.RegisterAgentRoot(sock, *pidFlag, start); err != nil {
			return err
		}
		fmt.Printf("registered agent root pid %d\n", *pidFlag)
	} else {
		if err := daemon.UnregisterAgentRoot(sock, *pidFlag, start); err != nil {
			return err
		}
		fmt.Printf("unregistered agent root pid %d\n", *pidFlag)
	}
	return nil
}

// resolveStore returns the store directory: the explicit flag if set, otherwise
// a per-project area under $XDG_DATA_HOME/checkpoint keyed by the project's
// absolute path (base name + short path hash), so distinct projects never share
// a store and the same project always resolves to the same one.
// Every command resolves its store here, so the out-of-tree invariant is
// enforced once, at the choke point: an explicitly configured --store that
// lives inside the protected/target folder (or contains it) is REFUSED. An
// in-tree store would also be captured as project content, and it lets
// `rm -rf project` destroy the recovery data along with the project, which is
// the exact failure the whole design exists to prevent.
func resolveStore(flagVal, projectPath string) (string, error) {
	if flagVal != "" {
		abs, err := filepath.Abs(flagVal)
		if err != nil {
			return "", err
		}
		project, err := filepath.Abs(projectPath)
		if err != nil {
			return "", err
		}
		if err := store.CheckStoreLocation(abs, project); err != nil {
			return "", err
		}
		return abs, nil
	}
	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dataHome = filepath.Join(home, ".local", "share")
	}
	sum := sha256.Sum256([]byte(projectPath))
	key := filepath.Base(projectPath) + "-" + hex.EncodeToString(sum[:])[:12]
	def := filepath.Join(dataHome, "checkpoint", key)
	// The default can only land in-tree if the project contains $XDG_DATA_HOME
	// (or $HOME); refuse that too rather than silently protecting nothing.
	project, err := filepath.Abs(projectPath)
	if err != nil {
		return "", err
	}
	if err := store.CheckStoreLocation(def, project); err != nil {
		return "", err
	}
	return def, nil
}

// plural picks a singular/plural suffix for counted nouns in user output.
// reviewNote phrases why a path needs review. An unknown writer must never be
// reported as the user: checkpoint refuses to touch the path either way, but
// it only claims the user changed something when it actually attributed the
// write to them.
func reviewNote(other undo.Other) string {
	switch other {
	case undo.OtherHuman:
		return "you also changed it"
	case undo.OtherBoth:
		return "you and an unidentified process also changed it"
	default:
		return "an unidentified process also changed it"
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

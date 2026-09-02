package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/manyapn/checkpoint-public/internal/daemon"
	"github.com/manyapn/checkpoint-public/internal/objstore"
	"github.com/manyapn/checkpoint-public/internal/oplog"
	"github.com/manyapn/checkpoint-public/internal/store"
)

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

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

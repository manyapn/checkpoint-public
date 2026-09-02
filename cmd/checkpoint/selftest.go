package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/manyapn/checkpoint-public/internal/selftest"
)

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

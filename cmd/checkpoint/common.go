package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/manyapn/checkpoint-public/internal/daemon"
	"github.com/manyapn/checkpoint-public/internal/snapshot"
)

// target is what every command needs: the protected folder, its store, and
// the daemon socket in that store.
type target struct {
	root, storeDir, sock string
}

func flags(name string) (*flag.FlagSet, *string, *string) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	root := fs.String("root", "", "protected folder (default: current directory)")
	store := fs.String("store", "", "store directory (default: ~/.local/share/checkpoint/<project>)")
	return fs, root, store
}

func resolve(rootFlag, storeFlag string) (target, error) {
	root := rootFlag
	if root == "" {
		root, _ = os.Getwd()
	}
	root, err := realDir(root)
	if err != nil {
		return target{}, err
	}
	storeDir := storeFlag
	if storeDir == "" {
		storeDir = defaultStore(root)
	} else if storeDir, err = filepath.Abs(storeDir); err != nil {
		return target{}, err
	}
	if err := snapshot.CheckStoreLocation(storeDir, root); err != nil {
		return target{}, err
	}
	return target{root, storeDir, daemon.SocketPath(storeDir)}, nil
}

// realDir resolves symlinks so a symlinked project is treated as its target
// everywhere. A path that does not exist yet is kept as given.
func realDir(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}
	return abs, nil
}

func defaultStore(root string) string {
	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		home, _ := os.UserHomeDir()
		dataHome = filepath.Join(home, ".local", "share")
	}
	sum := sha256.Sum256([]byte(root))
	return filepath.Join(dataHome, "checkpoint", filepath.Base(root)+"-"+hex.EncodeToString(sum[:6]))
}

func noArgs(fs *flag.FlagSet) error {
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	return nil
}

// parseOnly turns "a,b" into clean relative paths. An explicitly empty
// selector is refused rather than widened into "everything".
func parseOnly(value string, root string) ([]string, error) {
	var only []string
	for _, p := range strings.Split(value, ",") {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		if filepath.IsAbs(p) {
			p, _ = filepath.Rel(root, p)
		}
		p = filepath.Clean(p)
		if p == "." || p == ".." || strings.HasPrefix(p, "../") {
			return nil, fmt.Errorf("--only %s is outside the protected folder", p)
		}
		only = append(only, p)
	}
	if len(only) == 0 {
		return nil, fmt.Errorf("--only names no paths; omit it to operate on everything")
	}
	return only, nil
}

func parseID(s string) (int, error) {
	id, err := strconv.ParseUint(s, 10, 31)
	if err != nil {
		return 0, fmt.Errorf("bad checkpoint id %q", s)
	}
	return int(id), nil
}

func confirm(prompt string, yes bool) error {
	if yes {
		return nil
	}
	if fi, err := os.Stdin.Stat(); err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return fmt.Errorf("refusing without confirmation: rerun with --yes")
	}
	fmt.Printf("%s [y/N] ", prompt)
	var answer string
	fmt.Fscanln(os.Stdin, &answer)
	if a := strings.ToLower(answer); a != "y" && a != "yes" {
		return fmt.Errorf("aborted")
	}
	return nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

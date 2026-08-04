package store

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// TestFoldNeverCapturesSecrets: the credential denylist has to be enforced on
// BOTH manifest-building paths. Enforced only in scanTree, a .env written after
// the baseline is read into the object store on every change-feed filesystem
// (the recommended setup), so the gap would hit the users who configured things
// correctly. The randomized property test below cannot catch this: it never
// generates a credential-named file.
func TestFoldNeverCapturesSecrets(t *testing.T) {
	root := t.TempDir()
	oc := mustObj(t, t.TempDir())
	writeFile(t, filepath.Join(root, "main.go"), "package main\n", 0o644)
	base, err := Snapshot(root, oc, nil, 0, 1, DURABLE, 0)
	if err != nil {
		t.Fatal(err)
	}
	const marker = "SECRET-FOLD-MARKER"
	writeFile(t, filepath.Join(root, ".env"), "KEY="+marker+"\n", 0o600)
	writeFile(t, filepath.Join(root, "main.go"), "package main // edited\n", 0o644)

	dirty := map[string][]string{root: {
		filepath.Join(root, ".env"), filepath.Join(root, "main.go"),
	}}
	m, err := SnapshotFold(root, oc, base, 1, 2, DURABLE, 0, dirty)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Entries[".env"]; ok {
		t.Fatal(".env must never become a manifest entry on the fold path")
	}
	var named bool
	for _, ex := range m.Exceptions {
		if ex.Path == ".env" {
			named = true
		}
	}
	if !named {
		t.Fatal(".env was skipped but not reported; silent non-protection is the forbidden failure mode")
	}
	refs, err := oc.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range refs {
		if b, err := oc.Get(ref); err == nil && strings.Contains(string(b), marker) {
			t.Fatalf("credential content reached the object store via the fold path (%s)", ref)
		}
	}
}

// TestFoldMatchesFullScan pins SnapshotFold's correctness contract: given a
// COMPLETE change-set (every path created, written, deleted, or renamed since
// the base, including both names of a rename), the fold equals a full Snapshot.
// Runs a deterministic randomized op sequence over files and directories.
func TestFoldMatchesFullScan(t *testing.T) {
	for _, seed := range []int64{1, 42, 7} {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			root := t.TempDir()
			oc := mustObj(t, t.TempDir())
			rng := rand.New(rand.NewSource(seed))

			// Base tree.
			var files []string
			for i := 0; i < 8; i++ {
				rel := fmt.Sprintf("d%d/f%d.txt", i%3, i)
				writeFile(t, filepath.Join(root, rel), fmt.Sprintf("base %d\n", i), 0o644)
				files = append(files, rel)
			}
			base, err := Snapshot(root, oc, nil, 0, 1, DURABLE, 0)
			if err != nil {
				t.Fatal(err)
			}

			// Random ops; record the COMPLETE change-set as the feed would.
			dirty := map[string]bool{}
			touch := func(rel string) { dirty[filepath.Join(root, rel)] = true }
			for op := 0; op < 40; op++ {
				switch rng.Intn(5) {
				case 0: // create (possibly in a new dir)
					rel := fmt.Sprintf("n%d/new%d.txt", rng.Intn(3), op)
					writeFile(t, filepath.Join(root, rel), fmt.Sprintf("new %d\n", op), 0o644)
					touch(filepath.Dir(rel)) // mkdir event
					touch(rel)
					files = append(files, rel)
				case 1: // modify
					if len(files) == 0 {
						continue
					}
					rel := files[rng.Intn(len(files))]
					writeFile(t, filepath.Join(root, rel), fmt.Sprintf("mod %d\n", op), 0o644)
					touch(rel)
				case 2: // delete a file
					if len(files) == 0 {
						continue
					}
					i := rng.Intn(len(files))
					rel := files[i]
					if err := os.Remove(filepath.Join(root, rel)); err == nil {
						touch(rel)
						files = append(files[:i], files[i+1:]...)
					}
				case 3: // rename a file (sibling)
					if len(files) == 0 {
						continue
					}
					i := rng.Intn(len(files))
					rel := files[i]
					dst := filepath.Join(filepath.Dir(rel), fmt.Sprintf("mv%d.txt", op))
					if err := os.Rename(filepath.Join(root, rel), filepath.Join(root, dst)); err == nil {
						touch(rel) // rename-from
						touch(dst) // rename-to
						files[i] = dst
					}
				case 4: // rename a whole directory
					ents, _ := os.ReadDir(root)
					var dirs []string
					for _, e := range ents {
						if e.IsDir() {
							dirs = append(dirs, e.Name())
						}
					}
					if len(dirs) == 0 {
						continue
					}
					src := dirs[rng.Intn(len(dirs))]
					dst := fmt.Sprintf("moved%d", op)
					if err := os.Rename(filepath.Join(root, src), filepath.Join(root, dst)); err == nil {
						touch(src)
						touch(dst)
						for j, f := range files {
							if rel, ok := hasPrefixDir(f, src); ok {
								files[j] = filepath.Join(dst, rel)
							}
						}
					}
				}
			}

			var dirtyList []string
			for p := range dirty {
				dirtyList = append(dirtyList, p)
			}
			sort.Strings(dirtyList)

			fold, err := SnapshotFold(root, oc, base, 1, 2, DURABLE, 0, map[string][]string{root: dirtyList})
			if err != nil {
				t.Fatal(err)
			}
			scan, err := Snapshot(root, oc, base, 1, 2, DURABLE, 0)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(fold.Entries, scan.Entries) {
				t.Fatalf("fold != full scan:\nfold: %v\nscan: %v\ndirty: %v",
					keysOf(fold.Entries), keysOf(scan.Entries), dirtyList)
			}
		})
	}
}

func hasPrefixDir(rel, dir string) (string, bool) {
	pfx := dir + "/"
	if len(rel) > len(pfx) && rel[:len(pfx)] == pfx {
		return rel[len(pfx):], true
	}
	return "", false
}

func keysOf(m map[string]Entry) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

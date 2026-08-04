// Package objstore is checkpoint's content store: file contents ("versions")
// stored as git loose objects: content-addressed, zlib-compressed, deduplicated.
//
// It writes byte-for-byte git-compatible blob objects (`blob <len>\0<content>`,
// keyed by sha1, at objects/<xx>/<rest>, zlib-deflated), so an object our Put
// produced is readable by a real `git cat-file` and reclaimable by `git gc`.
// The format was chosen over a bespoke content-addressed store so that stored
// bytes stay inspectable and reclaimable with tools people already have, and so
// no effort goes into inventing an encoding. We still own the write path (no
// `git` subprocess) to stay dependency-free and fast.
//
// Scope is deliberately narrow: canonical blob encoding, sha1 hashing, zlib
// compression, atomic writes, idempotent existing-object handling, plus the
// List/Delete/Size primitives store.Prune's GC needs (the reachability decision
// lives there, not here). No trees/commits/refs/packs, no delta compression.
package objstore

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
)

// Store is a git loose-object directory. dir is the object root; blobs live at
// dir/objects/<xx>/<38 hex>.
type Store struct{ dir string }

// Open creates (if needed) the object directory rooted at dir. Pass a project's
// out-of-tree store area for normal use; pass a repo's `.git` to interoperate
// with a real git.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "objects"), 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

// blobHeader returns the canonical git object framing for a blob of length n:
// "blob <n>\0". The sha1 of header+content is the object id; zlib(header+content)
// is the stored bytes.
func blobHeader(n int) []byte {
	return []byte("blob " + strconv.Itoa(n) + "\x00")
}

func (s *Store) path(ref string) string {
	return filepath.Join(s.dir, "objects", ref[:2], ref[2:])
}

// Put stores content as a git blob and returns its ref (40-hex sha1). isNew is
// false when the object already existed (dedup hit: nothing written). Writing an
// object that already exists is a no-op, so Put is idempotent and safe to retry.
func (s *Store) Put(content []byte) (ref string, isNew bool, err error) {
	header := blobHeader(len(content))
	h := sha1.New()
	h.Write(header)
	h.Write(content)
	ref = hex.EncodeToString(h.Sum(nil))

	dst := s.path(ref)
	if _, statErr := os.Stat(dst); statErr == nil {
		return ref, false, nil // dedup hit: object already durable
	}

	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err = zw.Write(header); err != nil {
		return "", false, err
	}
	if _, err = zw.Write(content); err != nil {
		return "", false, err
	}
	if err = zw.Close(); err != nil {
		return "", false, err
	}

	if err = os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", false, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), "tmp-")
	if err != nil {
		return "", false, err
	}
	tmpName := tmp.Name()
	if _, err = tmp.Write(buf.Bytes()); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", false, err
	}
	if err = tmp.Close(); err != nil {
		os.Remove(tmpName)
		return "", false, err
	}
	if err = os.Rename(tmpName, dst); err != nil { // visible == complete
		os.Remove(tmpName)
		return "", false, err
	}
	return ref, true, nil
}

// Get returns the content for ref, verifying the object's framing and that the
// recomputed sha1 matches ref. A corrupt, truncated, or wrong object is an
// error, never silently wrong bytes.
func (s *Store) Get(ref string) ([]byte, error) {
	if !validRef(ref) {
		return nil, fmt.Errorf("objstore: invalid ref %q", ref)
	}
	f, err := os.Open(s.path(ref))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	zr, err := zlib.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("objstore: %s: %w", ref, err)
	}
	defer zr.Close()
	raw, err := io.ReadAll(zr)
	if err != nil {
		return nil, fmt.Errorf("objstore: %s: %w", ref, err)
	}
	nul := bytes.IndexByte(raw, 0)
	if nul < 0 {
		return nil, fmt.Errorf("objstore: %s: no object header", ref)
	}
	header, content := raw[:nul], raw[nul+1:]
	// header is "blob <len>"; validate type and length.
	var n int
	if _, err := fmt.Sscanf(string(header), "blob %d", &n); err != nil || n != len(content) {
		return nil, fmt.Errorf("objstore: %s: bad object header %q", ref, header)
	}
	h := sha1.New()
	h.Write(raw)
	if hex.EncodeToString(h.Sum(nil)) != ref {
		return nil, fmt.Errorf("objstore: %s: content hash mismatch", ref)
	}
	return content, nil
}

// List returns every stored ref, sorted. Non-object files (a tmp- left by a
// crash mid-Put) are excluded by the same ref validation Get applies. An empty
// or absent object directory is an empty list, not an error.
func (s *Store) List() ([]string, error) {
	root := filepath.Join(s.dir, "objects")
	fans, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var refs []string
	for _, fan := range fans {
		if !fan.IsDir() || len(fan.Name()) != 2 {
			continue
		}
		ents, err := os.ReadDir(filepath.Join(root, fan.Name()))
		if err != nil {
			return nil, err
		}
		for _, e := range ents {
			ref := fan.Name() + e.Name()
			if validRef(ref) {
				refs = append(refs, ref)
			}
		}
	}
	sort.Strings(refs)
	return refs, nil
}

// Delete removes ref's object (GC path: the caller has proven nothing
// references it). The ref is validated like Get; removing a missing ref is an
// error, because a GC that deletes what it never listed is deleting blind.
func (s *Store) Delete(ref string) error {
	if !validRef(ref) {
		return fmt.Errorf("objstore: invalid ref %q", ref)
	}
	return os.Remove(s.path(ref))
}

// Size returns ref's on-disk (compressed) byte size, so a prune can report the
// bytes it reclaims honestly.
func (s *Store) Size(ref string) (int64, error) {
	if !validRef(ref) {
		return 0, fmt.Errorf("objstore: invalid ref %q", ref)
	}
	fi, err := os.Stat(s.path(ref))
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// Has reports whether ref is present.
func (s *Store) Has(ref string) bool {
	if !validRef(ref) {
		return false
	}
	_, err := os.Stat(s.path(ref))
	return err == nil
}

func validRef(ref string) bool {
	if len(ref) != 40 {
		return false
	}
	for _, c := range ref {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

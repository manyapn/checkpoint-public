// Package objects stores file contents by hash, in git's loose-object format
// so `git cat-file -p <ref>` can read what we wrote.
package objects

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

type Store struct{ dir string }

func Open(storeDir string) (*Store, error) {
	dir := filepath.Join(storeDir, "objects")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

func (s *Store) path(ref string) string {
	return filepath.Join(s.dir, ref[:2], ref[2:])
}

// Put returns the ref for content, writing it only if not already present.
func (s *Store) Put(content []byte) (string, error) {
	framed := append([]byte("blob "+strconv.Itoa(len(content))+"\x00"), content...)
	sum := sha1.Sum(framed)
	ref := hex.EncodeToString(sum[:])
	if s.Has(ref) {
		return ref, nil
	}
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	zw.Write(framed)
	if err := zw.Close(); err != nil {
		return "", err
	}
	dst := s.path(ref)
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), "tmp-")
	if err != nil {
		return "", err
	}
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	// rename is atomic: a reader never sees a half-written object
	if err := os.Rename(tmp.Name(), dst); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return ref, nil
}

// Get returns the content for ref, or an error if the stored bytes do not
// hash back to ref.
func (s *Store) Get(ref string) ([]byte, error) {
	if !ValidRef(ref) {
		return nil, fmt.Errorf("objects: invalid ref %q", ref)
	}
	f, err := os.Open(s.path(ref))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	zr, err := zlib.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("objects: %s: %w", ref, err)
	}
	defer zr.Close()
	framed, err := io.ReadAll(zr)
	if err != nil {
		return nil, fmt.Errorf("objects: %s: %w", ref, err)
	}
	nul := bytes.IndexByte(framed, 0)
	sum := sha1.Sum(framed)
	if nul < 0 || hex.EncodeToString(sum[:]) != ref {
		return nil, fmt.Errorf("objects: %s: corrupt object", ref)
	}
	return framed[nul+1:], nil
}

func (s *Store) Has(ref string) bool {
	if !ValidRef(ref) {
		return false
	}
	_, err := os.Stat(s.path(ref))
	return err == nil
}

func (s *Store) Delete(ref string) error {
	if !ValidRef(ref) {
		return fmt.Errorf("objects: invalid ref %q", ref)
	}
	return os.Remove(s.path(ref))
}

func (s *Store) Size(ref string) int64 {
	fi, err := os.Stat(s.path(ref))
	if err != nil {
		return 0
	}
	return fi.Size()
}

// List returns every stored ref, sorted. Leftover tmp- files are not refs.
func (s *Store) List() ([]string, error) {
	fans, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var refs []string
	for _, fan := range fans {
		if !fan.IsDir() || len(fan.Name()) != 2 {
			continue
		}
		ents, err := os.ReadDir(filepath.Join(s.dir, fan.Name()))
		if err != nil {
			return nil, err
		}
		for _, e := range ents {
			if ref := fan.Name() + e.Name(); ValidRef(ref) {
				refs = append(refs, ref)
			}
		}
	}
	sort.Strings(refs)
	return refs, nil
}

func ValidRef(ref string) bool {
	if len(ref) != 40 {
		return false
	}
	_, err := hex.DecodeString(ref)
	return err == nil
}

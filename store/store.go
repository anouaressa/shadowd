// Package store implements the content-addressable "object store" that
// backs shadowd. Every chunk is saved once under a path derived from its
// hash (git-style: first two hex chars as a subdirectory, to avoid one huge
// flat directory). Saving a chunk that already exists on disk is a no-op —
// that check is the entire deduplication mechanism.
package store

import (
	"fmt"
	"os"
	"path/filepath"

	"shadowd/chunker"
)

type Store struct {
	Root string // e.g. /var/lib/shadowd/objects
}

func New(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("creating store root: %w", err)
	}
	return &Store{Root: root}, nil
}

func (s *Store) objectPath(hash string) string {
	return filepath.Join(s.Root, hash[:2], hash[2:])
}

// Has reports whether a chunk with this hash is already stored.
func (s *Store) Has(hash string) bool {
	_, err := os.Stat(s.objectPath(hash))
	return err == nil
}

// Save writes a chunk to disk if (and only if) it isn't already present.
// Returns whether a new object was actually written, so callers can report
// how much new data a snapshot introduced.
func (s *Store) Save(c chunker.Chunk) (wrote bool, err error) {
	dest := s.objectPath(c.Hash)
	if _, err := os.Stat(dest); err == nil {
		return false, nil // already have it — dedup hit
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return false, err
	}

	// Write to a temp file in the same directory then rename, so a crash
	// mid-write never leaves a corrupt object under its real hash name.
	tmp, err := os.CreateTemp(filepath.Dir(dest), "tmp-*")
	if err != nil {
		return false, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename below succeeds

	if _, err := tmp.Write(c.Data); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(tmpName, dest); err != nil {
		return false, err
	}
	return true, nil
}

// Load reads a chunk's raw bytes back by hash.
func (s *Store) Load(hash string) ([]byte, error) {
	return os.ReadFile(s.objectPath(hash))
}

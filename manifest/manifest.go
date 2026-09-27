// Package manifest tracks *which chunks* make up each snapshot of each
// tracked file. The actual chunk bytes live in the store; this is just the
// small, cheap index that says "file X at time T = chunks [a, b, c]".
//
// It's a single JSON file rather than a database, on purpose: a homelab
// tracking a modest number of files takes on the order of tens of KB here
// even with thousands of versions, so a database would add a dependency
// for no real benefit. If this ever needs to track many thousands of files
// with very frequent snapshots, swap this file for SQLite without changing
// the interface used elsewhere.
package manifest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type Version struct {
	Timestamp int64    `json:"timestamp"` // unix seconds
	Chunks    []string `json:"chunks"`    // ordered chunk hashes; concatenation reconstructs the file
	Size      int64    `json:"size"`      // original file size, for display/listing
	NewBytes  int64    `json:"new_bytes"` // bytes actually newly written to the store by this snapshot
}

type Manifest struct {
	mu       sync.Mutex
	path     string
	Versions map[string][]Version `json:"versions"` // key: absolute source file path
}

// Load reads the manifest from disk, or returns an empty one if it doesn't
// exist yet (first run).
func Load(path string) (*Manifest, error) {
	m := &Manifest{path: path, Versions: map[string][]Version{}}

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return m, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading manifest: %w", err)
	}
	if err := json.Unmarshal(data, m); err != nil {
		return nil, fmt.Errorf("parsing manifest: %w", err)
	}
	return m, nil
}

// save writes the manifest atomically (temp file + rename), same reasoning
// as the object store: never leave a half-written manifest behind.
func (m *Manifest) save() error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(m.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "manifest-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, m.path)
}

// AddVersion records a new snapshot for path and persists the manifest.
func (m *Manifest) AddVersion(path string, v Version) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.Versions[path] = append(m.Versions[path], v)
	return m.save()
}

// List returns every recorded version of path, oldest first.
func (m *Manifest) List(path string) []Version {
	m.mu.Lock()
	defer m.mu.Unlock()

	vs := append([]Version(nil), m.Versions[path]...)
	// Stable sort matters here: two versions can share the same
	// second-resolution timestamp (fast schedules, rapid manual snapshots),
	// and a non-stable sort could then reorder them unpredictably between
	// calls — which would make "row N" in the UI point at a different
	// version each time. Stable sort keeps ties in append (chronological)
	// order.
	sort.SliceStable(vs, func(i, j int) bool { return vs[i].Timestamp < vs[j].Timestamp })
	return vs
}

// AtIndex returns the version at position idx in List's ordering (0 =
// oldest). Unlike At, this is unambiguous even when two versions share the
// same second-resolution timestamp — which happens easily under a fast
// schedule or repeated manual snapshots — so callers that already know
// which row a user picked (e.g. the web UI) should use this instead of
// converting the row back into a timestamp and calling At.
func (m *Manifest) AtIndex(path string, idx int) (Version, bool) {
	vs := m.List(path)
	if idx < 0 || idx >= len(vs) {
		return Version{}, false
	}
	return vs[idx], true
}

// At returns the most recent version at or before t. If t is the zero
// value, it returns the latest version overall.
func (m *Manifest) At(path string, t time.Time) (Version, bool) {
	vs := m.List(path) // already sorted oldest first
	if len(vs) == 0 {
		return Version{}, false
	}
	if t.IsZero() {
		return vs[len(vs)-1], true
	}

	var best Version
	found := false
	for _, v := range vs {
		if v.Timestamp <= t.Unix() {
			best = v
			found = true
		}
	}
	return best, found
}

// TrackedPaths returns every source path with at least one recorded version.
func (m *Manifest) TrackedPaths() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	paths := make([]string, 0, len(m.Versions))
	for p := range m.Versions {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}

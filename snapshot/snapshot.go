// Package snapshot implements the two user-facing operations: taking a
// snapshot of a file (chunk it, save new chunks, record the version) and
// restoring a file from a recorded version (look up its chunk list,
// concatenate them back into a file).
package snapshot

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"shadowd/chunker"
	"shadowd/manifest"
	"shadowd/store"
)

type Engine struct {
	Store    *store.Store
	Manifest *manifest.Manifest
}

// Take snapshots one file: read it, split it into content-defined chunks,
// save any chunk not already in the store, and record the resulting
// version in the manifest. Returns the recorded version.
//
// If the file's content is identical to its most recent snapshot (same
// chunk list), no new version is recorded — this keeps a cron job that
// fires hourly from filling the manifest with hundreds of identical
// entries for a file nobody touched.
func (e *Engine) Take(path string) (manifest.Version, bool, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return manifest.Version{}, false, err
	}

	info, err := os.Stat(absPath)
	if err != nil {
		return manifest.Version{}, false, fmt.Errorf("opening %s: %w", absPath, err)
	}
	if info.IsDir() {
		return e.takeDir(absPath)
	}
	return e.takeFile(absPath)
}

func (e *Engine) takeFile(path string) (manifest.Version, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return manifest.Version{}, false, fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return manifest.Version{}, false, err
	}

	chunks, err := chunker.Split(f)
	if err != nil {
		return manifest.Version{}, false, fmt.Errorf("chunking %s: %w", path, err)
	}

	hashes := make([]string, len(chunks))
	var newBytes int64
	for i, c := range chunks {
		hashes[i] = c.Hash
		wrote, err := e.Store.Save(c)
		if err != nil {
			return manifest.Version{}, false, fmt.Errorf("saving chunk: %w", err)
		}
		if wrote {
			newBytes += int64(len(c.Data))
		}
	}

	if last, ok := e.Manifest.At(path, time.Time{}); ok && sameChunks(last.Chunks, hashes) {
		return last, false, nil // unchanged since last snapshot — nothing recorded
	}

	v := manifest.Version{
		Timestamp: time.Now().Unix(),
		Chunks:    hashes,
		Size:      info.Size(),
		NewBytes:  newBytes,
	}
	if err := e.Manifest.AddVersion(path, v); err != nil {
		return manifest.Version{}, false, fmt.Errorf("recording version: %w", err)
	}
	return v, true, nil
}

func (e *Engine) takeDir(dir string) (manifest.Version, bool, error) {
	var last manifest.Version
	changedAny := false
	var fileCount int

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		fileCount++

		v, changed, err := e.takeFile(path)
		if err != nil {
			return err
		}
		last = v
		if changed {
			changedAny = true
		}
		return nil
	})
	if err != nil {
		return manifest.Version{}, false, err
	}
	if fileCount == 0 {
		return manifest.Version{}, false, nil
	}
	return last, changedAny, nil
}

func sameChunks(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Restore reconstructs path as it existed at time t (or the latest version,
// if t is the zero value) and writes it to outPath.
//
// Prefer RestoreIndex when the caller already knows exactly which recorded
// version it wants (e.g. a specific row a user picked in a UI): two
// versions can share the same second-resolution timestamp, which makes
// "restore by time" ambiguous between them. This method exists for the CLI,
// where a human-supplied time is the natural input and picking the latest
// match among ties is a reasonable, documented default.
func (e *Engine) Restore(path string, t time.Time, outPath string) (manifest.Version, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return manifest.Version{}, err
	}

	v, ok := e.Manifest.At(absPath, t)
	if !ok {
		return manifest.Version{}, fmt.Errorf("no snapshot of %s found%s", absPath, atSuffix(t))
	}
	return v, e.writeVersion(v, outPath)
}

// RestoreIndex reconstructs the version at the given index (as returned by
// Manifest.List / AtIndex, 0 = oldest) and writes it to outPath. This is
// unambiguous even when multiple versions share a timestamp.
func (e *Engine) RestoreIndex(path string, idx int, outPath string) (manifest.Version, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return manifest.Version{}, err
	}

	v, ok := e.Manifest.AtIndex(absPath, idx)
	if !ok {
		return manifest.Version{}, fmt.Errorf("no version at index %d for %s", idx, absPath)
	}
	return v, e.writeVersion(v, outPath)
}

// RollbackIndex restores the file to the version at the given index by
// overwriting the live file at path with that historical content.
func (e *Engine) RollbackIndex(path string, idx int) (manifest.Version, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return manifest.Version{}, err
	}

	v, ok := e.Manifest.AtIndex(absPath, idx)
	if !ok {
		return manifest.Version{}, fmt.Errorf("no version at index %d for %s", idx, absPath)
	}
	return v, e.writeVersion(v, absPath)
}

func (e *Engine) writeVersion(v manifest.Version, outPath string) error {
	out, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("creating %s: %w", outPath, err)
	}
	defer out.Close()

	for _, hash := range v.Chunks {
		data, err := e.Store.Load(hash)
		if err != nil {
			return fmt.Errorf("loading chunk %s: %w", hash[:12], err)
		}
		if _, err := out.Write(data); err != nil {
			return err
		}
	}
	return nil
}

func atSuffix(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return " at or before " + t.Format(time.RFC3339)
}

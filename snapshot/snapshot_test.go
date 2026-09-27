package snapshot

import (
	"os"
	"path/filepath"
	"testing"

	"shadowd/manifest"
	"shadowd/store"
)

func TestRollbackIndexRestoresLiveFile(t *testing.T) {
	tempDir := t.TempDir()
	path := filepath.Join(tempDir, "tracked.txt")

	if err := os.WriteFile(path, []byte("version 1\n"), 0o644); err != nil {
		t.Fatalf("write initial file: %v", err)
	}

	eng := &Engine{
		Store:    mustStore(t, filepath.Join(tempDir, "store")),
		Manifest: mustManifest(t, filepath.Join(tempDir, "manifest.json")),
	}

	if _, _, err := eng.Take(path); err != nil {
		t.Fatalf("take first snapshot: %v", err)
	}

	if err := os.WriteFile(path, []byte("version 2\n"), 0o644); err != nil {
		t.Fatalf("update file: %v", err)
	}

	if _, _, err := eng.Take(path); err != nil {
		t.Fatalf("take second snapshot: %v", err)
	}

	if _, err := eng.RollbackIndex(path, 0); err != nil {
		t.Fatalf("rollback to first version: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read rolled back file: %v", err)
	}
	if got, want := string(data), "version 1\n"; got != want {
		t.Fatalf("rolled back content = %q, want %q", got, want)
	}
}

func TestTakeSnapshotForDirectoryTracksFiles(t *testing.T) {
	tempDir := t.TempDir()
	root := filepath.Join(tempDir, "project")
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatalf("create dir structure: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "b.txt"), []byte("beta\n"), 0o644); err != nil {
		t.Fatalf("write b.txt: %v", err)
	}

	eng := &Engine{
		Store:    mustStore(t, filepath.Join(tempDir, "store")),
		Manifest: mustManifest(t, filepath.Join(tempDir, "manifest.json")),
	}

	if _, _, err := eng.Take(root); err != nil {
		t.Fatalf("take directory snapshot: %v", err)
	}

	tracked := eng.Manifest.TrackedPaths()
	if len(tracked) != 2 {
		t.Fatalf("tracked file count = %d, want 2", len(tracked))
	}
}

func mustStore(t *testing.T, root string) *store.Store {
	t.Helper()
	st, err := store.New(root)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	return st
}

func mustManifest(t *testing.T, path string) *manifest.Manifest {
	t.Helper()
	m, err := manifest.Load(path)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	return m
}

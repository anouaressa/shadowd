// Package webui exposes shadowd's tracked files and version history over
// HTTP, and serves a small embedded single-page dashboard for browsing and
// restoring versions from a browser. It never mutates snapshot history —
// the only "write" endpoint it exposes is a manual snapshot trigger, which
// goes through the exact same Engine.Take used by the cron scheduler.
package webui

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"shadowd/config"
	"shadowd/snapshot"
)

//go:embed static/index.html
var staticFS embed.FS

// fileSummary is what the file list view shows: one row per tracked file.
type fileSummary struct {
	Path         string `json:"path"`
	VersionCount int    `json:"versionCount"`
	LatestSize   int64  `json:"latestSize"`
	LatestTime   int64  `json:"latestTime"`
}

// versionInfo is one row in a file's version history.
type versionInfo struct {
	Index     int   `json:"index"`
	Timestamp int64 `json:"timestamp"`
	Size      int64 `json:"size"`
	NewBytes  int64 `json:"newBytes"`
}

// NewHandler builds the full HTTP handler: embedded frontend at "/" and the
// JSON API under "/api/". cfg is only used to know which paths are
// snapshot-able via the manual "snapshot now" button — the version history
// itself always comes from the manifest, which may know about paths no
// longer present in the current config (e.g. removed from config.json but
// still tracked historically).
func NewHandler(eng *snapshot.Engine, cfg *config.Config) http.Handler {
	mux := http.NewServeMux()

	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		log.Fatalf("webui: embedding static assets: %v", err) // build-time bug, not a runtime condition
	}
	mux.Handle("GET /", http.FileServer(http.FS(sub)))

	mux.HandleFunc("GET /api/files", func(w http.ResponseWriter, r *http.Request) {
		paths := eng.Manifest.TrackedPaths()
		summaries := make([]fileSummary, 0, len(paths))
		for _, p := range paths {
			versions := eng.Manifest.List(p)
			if len(versions) == 0 {
				continue
			}
			latest := versions[len(versions)-1]
			summaries = append(summaries, fileSummary{
				Path:         p,
				VersionCount: len(versions),
				LatestSize:   latest.Size,
				LatestTime:   latest.Timestamp,
			})
		}
		writeJSON(w, summaries)
	})

	mux.HandleFunc("GET /api/versions", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Query().Get("path")
		if path == "" {
			http.Error(w, "missing path parameter", http.StatusBadRequest)
			return
		}
		versions := eng.Manifest.List(path)
		out := make([]versionInfo, len(versions))
		for i, v := range versions {
			out[i] = versionInfo{Index: i, Timestamp: v.Timestamp, Size: v.Size, NewBytes: v.NewBytes}
		}
		writeJSON(w, out)
	})

	// GET /api/restore?path=...&index=N — reconstructs version N of path
	// and streams it back as a file download. Restoring here never
	// overwrites the live file on disk; it only ever writes to a temp file
	// that's streamed to the browser and then removed, so browsing history
	// from the UI can't accidentally clobber the current version of a file.
	mux.HandleFunc("GET /api/restore", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Query().Get("path")
		idxParam := r.URL.Query().Get("index")
		if path == "" || idxParam == "" {
			http.Error(w, "missing path or index parameter", http.StatusBadRequest)
			return
		}

		versions := eng.Manifest.List(path)
		idx, err := parseIndex(idxParam, len(versions))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		tmp, err := os.CreateTemp("", "shadowd-restore-*")
		if err != nil {
			http.Error(w, "internal error creating temp file", http.StatusInternalServerError)
			return
		}
		tmpPath := tmp.Name()
		tmp.Close()
		defer os.Remove(tmpPath)

		v, err := eng.RestoreIndex(path, idx, tmpPath)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		filename := fmt.Sprintf("%s.%s", baseName(path), time.Unix(v.Timestamp, 0).Format("20060102-150405"))
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
		http.ServeFile(w, r, tmpPath)
	})

	// POST /api/rollback?path=...&index=N — overwrites the live file at path
	// with the content from version N, effectively "rolling back" the file.
	mux.HandleFunc("POST /api/rollback", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Query().Get("path")
		idxParam := r.URL.Query().Get("index")
		if path == "" || idxParam == "" {
			http.Error(w, "missing path or index parameter", http.StatusBadRequest)
			return
		}

		versions := eng.Manifest.List(path)
		idx, err := parseIndex(idxParam, len(versions))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		v, err := eng.RollbackIndex(path, idx)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"path": path, "index": idx, "timestamp": v.Timestamp, "size": v.Size})
	})

	// POST /api/snapshot?path=... — take a snapshot of one configured path
	// right now, so the UI can offer a "snapshot now" button instead of
	// only ever showing whatever the cron schedule has produced so far.
	mux.HandleFunc("POST /api/snapshot", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Query().Get("path")
		if path == "" {
			http.Error(w, "missing path parameter", http.StatusBadRequest)
			return
		}
		if !isConfigured(cfg, path) {
			http.Error(w, "path is not in the configured paths list", http.StatusForbidden)
			return
		}
		v, changed, err := eng.Take(path)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"changed": changed, "timestamp": v.Timestamp, "size": v.Size, "newBytes": v.NewBytes})
	})

	// GET /api/config — tells the frontend which paths are actively
	// configured for snapshotting, so it can offer "snapshot now" only
	// where that's actually possible, plus show any configured path that
	// has no snapshot yet (versionCount 0) rather than only ever showing
	// paths the manifest already knows about.
	mux.HandleFunc("GET /api/config", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"paths": cfg.Paths, "schedule": cfg.Schedule})
	})

	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("webui: encoding response: %v", err)
	}
}

func parseIndex(s string, n int) (int, error) {
	var idx int
	if _, err := fmt.Sscanf(s, "%d", &idx); err != nil {
		return 0, fmt.Errorf("invalid index %q", s)
	}
	if idx < 0 || idx >= n {
		return 0, fmt.Errorf("index %d out of range (0..%d)", idx, n-1)
	}
	return idx, nil
}

// isConfigured mirrors snapshot.Engine's own path normalization (filepath.Abs)
// so /api/snapshot accepts either the path exactly as written in config.json
// or its resolved absolute form — the manifest always stores absolute paths,
// and the frontend always sends what /api/files gave it, which is absolute.
func isConfigured(cfg *config.Config, path string) bool {
	for _, p := range cfg.Paths {
		if p == path {
			return true
		}
		if abs, err := filepath.Abs(p); err == nil {
			if abs == path {
				return true
			}
			if dir, err := os.Stat(abs); err == nil && dir.IsDir() {
				if rel, err := filepath.Rel(abs, path); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
					return true
				}
			}
		}
	}
	return false
}

func baseName(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[i+1:]
		}
	}
	return path
}

// Package config loads shadowd's JSON config file. JSON rather than YAML
// is a deliberate choice: it needs zero third-party dependencies (Go's
// stdlib encoding/json is enough), so the whole project builds with only
// the standard library.
package config

import (
	"encoding/json"
	"fmt"
	"os"
)

type Config struct {
	// StoreDir holds the deduplicated chunk objects.
	StoreDir string `json:"store_dir"`
	// ManifestPath is the JSON file recording version history.
	ManifestPath string `json:"manifest_path"`
	// Schedule is a 5-field cron expression, or "@every <duration>".
	Schedule string `json:"schedule"`
	// Paths are the files to snapshot on every scheduled run. (Keep this
	// to individual files, not whole trees, for a first pass — see the
	// README for how to extend it to directories.)
	Paths []string `json:"paths"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}
	if c.StoreDir == "" || c.ManifestPath == "" || c.Schedule == "" {
		return nil, fmt.Errorf("config must set store_dir, manifest_path and schedule")
	}
	return &c, nil
}

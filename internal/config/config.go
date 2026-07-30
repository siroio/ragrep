// Package config reads the optional .ragrep/config.json workspace settings
// file. It uses only encoding/json from the standard library -- no new
// dependency for a handful of fields.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Defaults used when config.json is absent, or when it doesn't set a field.
const (
	DefaultDB = ".ragrep/index.db"
)

// Config is the shape of .ragrep/config.json. All paths are interpreted
// relative to the workspace root (the directory containing .ragrep/).
type Config struct {
	DB         string              `json:"db"`
	Converters map[string][]string `json:"converters"`
}

// Load reads .ragrep/config.json from root. A missing file is not an error:
// it yields the documented defaults (db=".ragrep/index.db"). A
// present-but-unparsable file is a clear error rather than falling back
// silently.
func Load(root string) (Config, error) {
	cfg := Config{DB: DefaultDB}

	path := filepath.Join(root, ".ragrep", "config.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return Config{}, err
	}

	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	if cfg.DB == "" {
		cfg.DB = DefaultDB
	}
	return cfg, nil
}

// ConverterFor returns the registered converter argv for an extension
// (".pdf"), or nil. Lookup is case-insensitive on the extension.
func (c Config) ConverterFor(ext string) []string {
	return c.Converters[strings.ToLower(ext)]
}

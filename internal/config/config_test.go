package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T, root, body string) {
	t.Helper()
	dir := filepath.Join(root, ".ragrep")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// With no .ragrep/config.json present, Load must fall back to the documented
// defaults: db=".ragrep/index.db", code_db=".ragrep/code.db", no servers.
func TestLoadDefaultsWhenMissing(t *testing.T) {
	root := t.TempDir()

	cfg, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DB != DefaultDB {
		t.Fatalf("DB=%q, want %q", cfg.DB, DefaultDB)
	}
	if cfg.CodeDB != DefaultCodeDB {
		t.Fatalf("CodeDB=%q, want %q", cfg.CodeDB, DefaultCodeDB)
	}
	if len(cfg.Servers) != 0 {
		t.Fatalf("Servers=%v, want empty", cfg.Servers)
	}
}

// A present config.json overrides the defaults with its own values.
func TestLoadReadsCustomValues(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root, `{
		"db": "custom/idx.db",
		"code_db": "custom/code.db",
		"servers": {"go": "gopls"}
	}`)

	cfg, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DB != "custom/idx.db" {
		t.Fatalf("DB=%q, want custom/idx.db", cfg.DB)
	}
	if cfg.CodeDB != "custom/code.db" {
		t.Fatalf("CodeDB=%q, want custom/code.db", cfg.CodeDB)
	}
	if cfg.Servers["go"] != "gopls" {
		t.Fatalf("Servers[go]=%q, want gopls", cfg.Servers["go"])
	}
}

// A config.json that only sets some fields must still fall back to defaults
// for the fields it omits (e.g. only "servers" set, no "db"/"code_db").
func TestLoadPartialFillsDefaults(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root, `{"servers": {"go": "gopls"}}`)

	cfg, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DB != DefaultDB {
		t.Fatalf("DB=%q, want default %q", cfg.DB, DefaultDB)
	}
	if cfg.CodeDB != DefaultCodeDB {
		t.Fatalf("CodeDB=%q, want default %q", cfg.CodeDB, DefaultCodeDB)
	}
}

// Malformed JSON must be a clear, non-nil error -- not a silently zeroed
// Config.
func TestLoadBadJSON(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root, `{not valid json`)

	if _, err := Load(root); err == nil {
		t.Fatal("Load: want error for malformed JSON, got nil")
	}
}

// ServerCommand returns the registered executable for a known language, and
// a clear error for one that isn't in servers -- this task only needs the
// lookup API; actually launching the server is a later task.
func TestServerCommand(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root, `{"servers": {"go": "gopls"}}`)
	cfg, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	cmd, err := cfg.ServerCommand("go")
	if err != nil || cmd != "gopls" {
		t.Fatalf("ServerCommand(go)=%q err=%v, want gopls,nil", cmd, err)
	}

	if _, err := cfg.ServerCommand("python"); err == nil {
		t.Fatal("ServerCommand(python): want error for unregistered language, got nil")
	}
}

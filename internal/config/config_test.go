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
// defaults: db=".ragrep/index.db".
func TestLoadDefaultsWhenMissing(t *testing.T) {
	root := t.TempDir()

	cfg, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DB != DefaultDB {
		t.Fatalf("DB=%q, want %q", cfg.DB, DefaultDB)
	}
}

// A present config.json overrides the defaults with its own values.
func TestLoadReadsCustomValues(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root, `{"db": "custom/idx.db"}`)

	cfg, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DB != "custom/idx.db" {
		t.Fatalf("DB=%q, want custom/idx.db", cfg.DB)
	}
}

// A config.json that only sets some fields must still fall back to defaults
// for the fields it omits (e.g. only "converters" set, no "db").
func TestLoadPartialFillsDefaults(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root, `{"converters": {".pdf": ["pdftotext", "{input}", "-"]}}`)

	cfg, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DB != DefaultDB {
		t.Fatalf("DB=%q, want default %q", cfg.DB, DefaultDB)
	}
}

// Named document profiles retain their configured path and description so
// callers can choose a corpus without resolving paths inside the loader.
func TestLoadReadsDocumentProfiles(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root, `{
		"default_profile": "game",
		"profiles": {
			"game": {
				"path": ".ragrep/game.db",
				"description": "Game design documents"
			}
		}
	}`)

	cfg, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DefaultProfile != "game" {
		t.Fatalf("DefaultProfile=%q, want game", cfg.DefaultProfile)
	}
	profile, ok := cfg.Profiles["game"]
	if !ok {
		t.Fatal("Profiles does not contain game")
	}
	if profile.Path != ".ragrep/game.db" {
		t.Fatalf("profile.Path=%q, want .ragrep/game.db", profile.Path)
	}
	if profile.Description != "Game design documents" {
		t.Fatalf("profile.Description=%q, want Game design documents", profile.Description)
	}
}

func TestLoadRejectsEmptyProfileName(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root, `{"profiles": {"": {"path": ".ragrep/game.db"}}}`)

	if _, err := Load(root); err == nil {
		t.Fatal("Load: want error for empty profile name, got nil")
	}
}

func TestLoadRejectsEmptyProfilePath(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root, `{"profiles": {"game": {"path": ""}}}`)

	if _, err := Load(root); err == nil {
		t.Fatal("Load: want error for empty profile path, got nil")
	}
}

func TestLoadRejectsUnknownDefaultProfile(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root, `{
		"default_profile": "research",
		"profiles": {"game": {"path": ".ragrep/game.db"}}
	}`)

	if _, err := Load(root); err == nil {
		t.Fatal("Load: want error for unknown default profile, got nil")
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

// ConverterFor returns the registered argv for a known extension, lowering
// the given extension before lookup (the map keys themselves are stored
// as-is), and nil for one that isn't registered.
func TestConverterFor(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root, `{"converters": {".pdf": ["pdftotext", "{input}", "-"]}}`)

	cfg, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	want := []string{"pdftotext", "{input}", "-"}
	got := cfg.ConverterFor(".pdf")
	if len(got) != len(want) {
		t.Fatalf("ConverterFor(.pdf)=%v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ConverterFor(.pdf)=%v, want %v", got, want)
		}
	}

	if got := cfg.ConverterFor(".PDF"); len(got) != len(want) {
		t.Fatalf("ConverterFor(.PDF)=%v, want case-insensitive match %v", got, want)
	}

	if got := cfg.ConverterFor(".docx"); got != nil {
		t.Fatalf("ConverterFor(.docx)=%v, want nil", got)
	}
}

//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCodeWorkspacePathsCanonicalizesMissingWindowsDatabaseKey(t *testing.T) {
	root := testWorkspaceRoot(t)
	configPath := filepath.Join(root, ".ragrep", "config.json")
	if err := os.WriteFile(configPath, []byte(`{"code_db":"Missing/Code.DB"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	configured := filepath.Join(root, "Missing", "Code.DB")
	alias := strings.ToUpper(filepath.ToSlash(configured))
	_, first, isDefault, err := codeWorkspacePaths(root, configured)
	if err != nil {
		t.Fatal(err)
	}
	_, second, aliasIsDefault, err := codeWorkspacePaths(root, alias)
	if err != nil {
		t.Fatal(err)
	}
	if !isDefault || !aliasIsDefault {
		t.Fatalf("default identity: configured=%v alias=%v", isDefault, aliasIsDefault)
	}
	if first != second {
		t.Fatalf("database keys differ: %q != %q", first, second)
	}
	firstKey := codeWorkspaceKey{root: root, db: first}
	secondKey := codeWorkspaceKey{root: root, db: second}
	if firstKey != secondKey {
		t.Fatal("codeWorkspaceKey does not use canonical database identity")
	}
}

func TestWorkspaceRegistryReusesWindowsDatabaseAliases(t *testing.T) {
	t.Run("default then custom casing", func(t *testing.T) {
		root := testWorkspaceRoot(t)
		r, err := newWorkspaceRegistry(filepath.Join(t.TempDir(), "workspaces.json"), time.Hour, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		opens := countWorkspaceOpeners(r)
		first, err := r.ResolveCode(root, "")
		if err != nil {
			t.Fatal(err)
		}
		alias := strings.ToUpper(filepath.ToSlash(filepath.Join(root, ".ragrep", "code.db")))
		second, err := r.ResolveCode(root, alias)
		if err != nil {
			t.Fatal(err)
		}
		if first != second || len(r.entries) != 1 || len(r.codeEntries) != 0 || opens.Load() != 1 {
			t.Fatalf("same state=%v default entries=%d custom entries=%d opens=%d", first == second, len(r.entries), len(r.codeEntries), opens.Load())
		}
	})

	t.Run("custom casing then default", func(t *testing.T) {
		root := testWorkspaceRoot(t)
		r, err := newWorkspaceRegistry(filepath.Join(t.TempDir(), "workspaces.json"), time.Hour, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		opens := countWorkspaceOpeners(r)
		alias := strings.ToUpper(filepath.ToSlash(filepath.Join(root, ".ragrep", "code.db")))
		first, err := r.ResolveCode(root, alias)
		if err != nil {
			t.Fatal(err)
		}
		release, err := r.AcquireCode(root, "")
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		second, err := r.ResolveCode(root, "")
		if err != nil {
			t.Fatal(err)
		}
		if first != second || len(r.entries)+len(r.codeEntries) != 1 || opens.Load() != 1 {
			t.Fatalf("same state=%v default entries=%d custom entries=%d opens=%d", first == second, len(r.entries), len(r.codeEntries), opens.Load())
		}
		entry := r.entries[root]
		if entry == nil {
			for _, entry = range r.codeEntries {
				break
			}
		}
		if entry == nil {
			t.Fatal("canonical database entry is missing")
		}
		if entry.leases != 1 {
			t.Fatalf("leases=%d, want 1", entry.leases)
		}
	})
}

//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
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

func TestClaimDaemonDiscoveryUsesUnoccupiedTarget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.json")
	discovery := daemonDiscovery{Endpoint: "http://127.0.0.1:7377", Token: "secret", PID: 1}
	if err := writeDaemonDiscovery(path, discovery); err != nil {
		t.Fatal(err)
	}
	claim, err := claimDaemonDiscovery(path)
	if err != nil {
		t.Fatal(err)
	}
	claimDir := filepath.Dir(claim)
	if claimDir == filepath.Dir(path) {
		t.Fatalf("claim target %q shares discovery directory; Windows rename replacement remains possible", claim)
	}
	finishDaemonDiscoveryClaim(claim, path, discovery)
	if _, err := os.Stat(claimDir); !os.IsNotExist(err) {
		t.Fatalf("claim directory remains after cleanup: %v", err)
	}
}

func TestStoppedNewerDaemonRemovesDiscoveryClaimedByOlderCleanup(t *testing.T) {
	for _, afterRead := range []bool{false, true} {
		name := "after old claim removal"
		if afterRead {
			name = "after old link"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "daemon.json")
			newer := daemonDiscovery{Endpoint: "http://127.0.0.1:7378", Token: "newer", PID: 2}
			if err := writeDaemonDiscovery(path, newer); err != nil {
				t.Fatal(err)
			}
			claim, err := claimDaemonDiscovery(path)
			if err != nil {
				t.Fatal(err)
			}
			claimDir := filepath.Dir(claim)
			linkOld := func() {
				if err := os.Link(claim, path); err != nil {
					t.Fatal(err)
				}
			}
			var listed, read func()
			if afterRead {
				read = linkOld
			} else {
				listed = func() {
					linkOld()
					if err := os.Remove(claim); err != nil {
						t.Fatal(err)
					}
				}
			}

			cleanupDaemonDiscovery(path, newer, listed, read)
			_ = os.Remove(claimDir)

			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("stale newer discovery restored: %v", err)
			}
			if _, err := os.Stat(claimDir); !os.IsNotExist(err) {
				t.Fatalf("newer discovery claim remains: %v", err)
			}
		})
	}
}

func TestMissingDiscoveryDoesNotLoopOnEmptyClaimDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".daemon-cleanup-stale"), 0o700); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		cleanupDaemonDiscovery(filepath.Join(dir, "daemon.json"), daemonDiscovery{Token: "stopped"}, nil, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cleanup looped on an empty claim directory")
	}
}

func TestUndeletableOwnedClaimDoesNotLoop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.json")
	owned := daemonDiscovery{Endpoint: "http://127.0.0.1:7377", Token: "stopped", PID: 1}
	if err := writeDaemonDiscovery(path, owned); err != nil {
		t.Fatal(err)
	}
	claim, err := claimDaemonDiscovery(path)
	if err != nil {
		t.Fatal(err)
	}
	claimPath, err := windows.UTF16PtrFromString(claim)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(claimPath, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	closeHandle := func() {
		if !closed {
			_ = windows.CloseHandle(handle)
			closed = true
		}
	}
	defer closeHandle()
	done := make(chan struct{})
	go func() {
		cleanupDaemonDiscovery(path, owned, nil, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		closeHandle()
		<-done
		t.Fatal("cleanup looped on an undeletable owned claim")
	}
	closeHandle()
	_ = removeDaemonDiscoveryClaim(claim)
}

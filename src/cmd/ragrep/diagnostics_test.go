package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func captureRunOutput(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var code int
	var stderr string
	stdout := captureStdout(t, func() {
		stderr = captureStderr(t, func() { code = run(args) })
	})
	return code, stdout + stderr
}

func TestVersionCommandReportsBuildFields(t *testing.T) {
	oldVersion, oldCommit, oldBuildDate := version, commit, buildDate
	version, commit, buildDate = "1.2.3", "abc123", "2026-09-09T00:00:00Z"
	t.Cleanup(func() { version, commit, buildDate = oldVersion, oldCommit, oldBuildDate })

	out := captureStdout(t, func() {
		if code := run([]string{"version"}); code != 0 {
			t.Fatalf("version exit=%d, want 0", code)
		}
	})
	for _, want := range []string{"1.2.3", "abc123", "2026-09-09T00:00:00Z"} {
		if !strings.Contains(out, want) {
			t.Fatalf("version output %q does not contain %q", out, want)
		}
	}
}

func TestDoctorMissingWorkspaceAssetsAndDBHasNoSideEffects(t *testing.T) {
	root := t.TempDir()
	cacheRoot := t.TempDir()
	t.Setenv("LOCALAPPDATA", cacheRoot)
	t.Setenv("XDG_CACHE_HOME", cacheRoot)
	t.Setenv("HOME", cacheRoot)
	t.Setenv("RAGREP_DB", "")
	if err := os.Mkdir(filepath.Join(root, ".ragrep"), 0o755); err != nil {
		t.Fatal(err)
	}
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })

	before, err := os.ReadDir(filepath.Join(root, ".ragrep"))
	if err != nil {
		t.Fatal(err)
	}
	exit, out := captureRunOutput(t, "doctor")
	if exit != 1 {
		t.Fatalf("doctor exit=%d, want 1 for missing DB/assets", exit)
	}
	if !strings.Contains(out, "ragrep init") {
		t.Fatalf("doctor output=%q, want init recovery", out)
	}
	if _, err := os.Stat(filepath.Join(root, ".ragrep", "index.db")); !os.IsNotExist(err) {
		t.Fatalf("doctor created index.db: stat error=%v", err)
	}
	after, err := os.ReadDir(filepath.Join(root, ".ragrep"))
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("doctor changed workspace metadata: before=%v after=%v", before, after)
	}
	if _, err := os.Stat(doctorCacheDir()); !os.IsNotExist(err) {
		t.Fatalf("doctor created cache state: stat error=%v", err)
	}
}

func TestDoctorDoesNotExposeMalformedConfigSecrets(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".ragrep"), 0o755); err != nil {
		t.Fatal(err)
	}
	const secret = "doctor-secret-sentinel"
	if err := os.WriteFile(filepath.Join(root, ".ragrep", "config.json"), []byte(`{"servers":{"go":"`+secret+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })

	_, out := captureRunOutput(t, "doctor")
	if strings.Contains(out, secret) {
		t.Fatalf("doctor exposed config secret: %q", out)
	}
}

func TestDoctorChecksConfiguredServerWithoutPrintingCommand(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".ragrep"), 0o755); err != nil {
		t.Fatal(err)
	}
	const secret = "doctor-command-secret"
	if err := os.WriteFile(filepath.Join(root, ".ragrep", "config.json"), []byte(`{"servers":{"go":"`+secret+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })

	_, out := captureRunOutput(t, "doctor", "--db", filepath.Join(root, ".ragrep", "missing.db"))
	if strings.Contains(out, secret) || !strings.Contains(out, "language server go") {
		t.Fatalf("doctor output=%q, want server status without command", out)
	}
}

func TestDoctorReportsIncompatibleIndexWithoutRawError(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".ragrep"), 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(root, ".ragrep", "index.db")
	if err := os.WriteFile(dbPath, []byte("not a ragrep database"), 0o600); err != nil {
		t.Fatal(err)
	}
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })

	_, out := captureRunOutput(t, "doctor")
	if strings.Contains(out, "doctor-secret-sentinel") || !strings.Contains(out, "incompatible") {
		t.Fatalf("doctor output=%q, want sanitized incompatible status", out)
	}
}

func TestSafeDaemonEndpointAcceptsLoopbackOnly(t *testing.T) {
	for _, endpoint := range []string{"http://127.0.0.1:7377", "http://[::1]:7377"} {
		if !safeDaemonEndpoint(endpoint) {
			t.Fatalf("safeDaemonEndpoint(%q)=false, want true", endpoint)
		}
	}
	for _, endpoint := range []string{"http://example.invalid:7377", "http://127.0.0.1:0", "http://127.0.0.1:7377/path", "http://127.0.0.1:7377?token=secret"} {
		if safeDaemonEndpoint(endpoint) {
			t.Fatalf("safeDaemonEndpoint(%q)=true, want false", endpoint)
		}
	}
}

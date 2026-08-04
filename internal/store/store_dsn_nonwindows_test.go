//go:build !windows

package store

import "testing"

func TestStoreDSNMatchesExistingNonWindowsValue(t *testing.T) {
	const path = "/tmp/ragrep/index.db"
	const want = "file:/tmp/ragrep/index.db?_pragma=busy_timeout(5000)"

	if got := storeDSN(path); got != want {
		t.Fatalf("storeDSN(%q) = %q, want %q", path, got, want)
	}
}

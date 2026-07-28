package sample

import "testing"

// TestGreet is the fixture's one reference to Greet from a _test.go file --
// exercises `code expand --relation tests` (a references result whose path
// ends in _test.go is classified "tests", see codeindex.ReferenceRelations).
func TestGreet(t *testing.T) {
	g := NewGreeter("world")
	if got := g.Greet(); got != "hello, world" {
		t.Fatalf("Greet() = %q, want %q", got, "hello, world")
	}
}

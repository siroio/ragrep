// Package sample is a tiny fixture for the gopls integration test
// (cmd/ragrep's code_gopls_test.go): it just needs enough shape -- a
// package-level function, a struct, a method on that struct, and a caller
// of that method -- for textDocument/documentSymbol to return more than one
// symbol and for callHierarchy/references relations to have something real
// to find. sample_test.go (same package) supplies the "tests" relation
// fixture.
package sample

// Greeter holds a name to greet.
type Greeter struct {
	Name string
}

// Greet returns a greeting for g's Name.
func (g Greeter) Greet() string {
	return "hello, " + g.Name
}

// NewGreeter constructs a Greeter for name.
func NewGreeter(name string) Greeter {
	return Greeter{Name: name}
}

// SayHello calls Greet -- the fixture's one real caller, for exercising
// `code expand --relation callers` / `--relation callees`. It calls Greet
// TWICE, deliberately: a symbol referenced twice from the same enclosing
// symbol used to crash `code expand --relation references|tests` with a
// symbol_edges UNIQUE constraint violation (duplicate resolved relations
// weren't deduped before being persisted) -- see
// codeindex.DedupResolvedRelations.
func SayHello(g Greeter) string {
	msg := g.Greet()
	return msg + " " + g.Greet()
}

// Package sample is a tiny fixture for the gopls integration test
// (cmd/ragrep's code_gopls_test.go): it just needs enough shape -- a
// package-level function, a struct, and a method on that struct -- for
// textDocument/documentSymbol to return more than one symbol.
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

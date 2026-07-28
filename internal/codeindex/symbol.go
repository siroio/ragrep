// Package codeindex converts LSP document symbols into code-index records:
// language-agnostic Symbol values with stable keys, extracted source bodies,
// and canonical embedding text. It has no dependency on the store or CLI —
// only internal/lsp, whose UTF-16-based Position/Range it consumes and
// converts to byte offsets in the source file.
package codeindex

// Position is a zero-based line/character location. Character counts
// UTF-16 code units from the start of the line, matching LSP's
// Position (see internal/lsp.Position) — codeindex defines its own type
// rather than aliasing lsp.Position so this package's public API carries no
// dependency on internal/lsp.
type Position struct {
	Line      int
	Character int
}

// Range is a half-open [Start, End) span of Positions.
type Range struct {
	Start Position
	End   Position
}

// Symbol is one code-index record: a single named declaration (function,
// method, type, field, ...) with everything needed to store, search, and
// re-resolve it. A struct/class and each of its methods are always separate
// Symbol values — Body never includes a child symbol's body (see extract.go
// and render.go).
type Symbol struct {
	Key           string
	Language      string
	Kind          string
	Name          string
	QualifiedName string
	Signature     string
	Documentation string
	Container     string
	Path          string
	Range         Range
	Body          string
	BodyHash      string
	EmbeddingText string
}

// Relation is a directed edge between two symbols (e.g. "calls", "extends"),
// keyed by the symbols' stable Keys.
type Relation struct {
	FromKey string
	ToKey   string
	Kind    string
	Source  string
}

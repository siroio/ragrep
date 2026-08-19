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
	Line      int `json:"line"`
	Character int `json:"character"`
}

// Range is a half-open [Start, End) span of Positions.
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// Symbol is one code-index record: a single named declaration (function,
// method, type, field, ...) with everything needed to store, search, and
// re-resolve it. A struct/class and each of its methods are always separate
// Symbol values — Body never includes a child symbol's body (see extract.go
// and render.go).
//
// JSON tags are snake_case, matching codestore.SymbolHit's own convention --
// Symbol is serialized straight into `code pack`'s JSON output (see
// coderetrieval.ContextPack.Symbols). EmbeddingText is deliberately excluded
// (json:"-"): it's internal render output, always recomputable from the
// other fields via RenderEmbeddingText, not something a pack consumer needs.
type Symbol struct {
	Key           string `json:"key"`
	Language      string `json:"language"`
	Kind          string `json:"kind"`
	Name          string `json:"name"`
	QualifiedName string `json:"qualified_name"`
	Signature     string `json:"signature"`
	Documentation string `json:"documentation,omitempty"`
	Container     string `json:"container,omitempty"`
	Path          string `json:"path"`
	Range         Range  `json:"range"`
	Body          string `json:"body,omitempty"`
	BodyHash      string `json:"body_hash,omitempty"`
	EmbeddingText string `json:"-"`
}

// Relation is a directed edge between two symbols (e.g. "calls", "extends"),
// keyed by the symbols' stable Keys. JSON tags are snake_case, matching
// Symbol's own convention -- Relation is serialized straight into `code
// pack`'s JSON output (see coderetrieval.ContextPack.Relations).
type Relation struct {
	FromKey string `json:"from_key"`
	ToKey   string `json:"to_key"`
	Kind    string `json:"kind"`
	Source  string `json:"source"`

	// ToPath and ToPosition preserve the target location for a relation
	// whose target didn't resolve to an indexed symbol -- ToKey is left ""
	// rather than fabricated (see relations.go's toRelation). Both are the
	// zero value when ToKey is set. Neither is persisted by
	// codestore.ReplaceRelations (symbol_edges has no columns for them);
	// they exist only so a caller (cmd/ragrep's `code expand`) can still
	// describe an unresolved reference instead of silently dropping it.
	ToPath     string   `json:"to_path,omitempty"`
	ToPosition Position `json:"to_position,omitempty"`
}

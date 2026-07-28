package codeindex

import "strings"

// Loc is an LSP location translated into codeindex's own coordinate system:
// Path is a display path (workspace-relative and slash-separated when the
// location is inside the workspace, same convention as Symbol.Path; an
// absolute/foreign path otherwise -- see cmd/ragrep's pathFromURI) and
// Position is the 0-based, UTF-16-character Position Symbol.Range also uses.
// Callers (cmd/ragrep) build these from lsp.Location / lsp.CallHierarchyItem
// before calling into this package: relations.go has no dependency on
// internal/lsp or the filesystem, only on already-normalized coordinates, so
// it can be tested without a real language server.
type Loc struct {
	Path     string
	Position Position
}

// Resolver looks up the stable key of the indexed symbol enclosing (path,
// line), if any. The codestore-backed implementation is
// (*codestore.Store).SymbolAt; character isn't needed since containment is
// resolved by line range alone.
type Resolver func(path string, line int) (key string, ok bool)

// toRelation builds one Relation from fromKey to whatever resolve finds
// enclosing loc: a real ToKey when resolve finds an indexed symbol there, or
// -- per the "never fabricate a key" rule -- an empty ToKey with loc's own
// path/position preserved as the fallback description otherwise (an
// external dependency, or simply a location that hasn't been indexed).
func toRelation(fromKey string, loc Loc, kind, source string, resolve Resolver) Relation {
	if key, ok := resolve(loc.Path, loc.Position.Line); ok {
		return Relation{FromKey: fromKey, ToKey: key, Kind: kind, Source: source}
	}
	return Relation{FromKey: fromKey, Kind: kind, Source: source, ToPath: loc.Path, ToPosition: loc.Position}
}

// DefinitionRelations converts textDocument/definition locations into
// "definition" relations.
func DefinitionRelations(fromKey string, locs []Loc, source string, resolve Resolver) []Relation {
	out := make([]Relation, 0, len(locs))
	for _, loc := range locs {
		out = append(out, toRelation(fromKey, loc, "definition", source, resolve))
	}
	return out
}

// ReferenceRelations converts textDocument/references locations into
// "references" relations -- except a location whose Path ends in _test.go,
// which becomes a "tests" relation instead: a reference found in a _test.go
// file reads as "this test exercises fromKey", which is a more useful
// classification than a generic reference.
func ReferenceRelations(fromKey string, locs []Loc, source string, resolve Resolver) []Relation {
	out := make([]Relation, 0, len(locs))
	for _, loc := range locs {
		kind := "references"
		if strings.HasSuffix(loc.Path, "_test.go") {
			kind = "tests"
		}
		out = append(out, toRelation(fromKey, loc, kind, source, resolve))
	}
	return out
}

// CallerRelations converts callHierarchy/incomingCalls results -- already
// reduced by the caller to each call's "from" location, one hop only, no
// further traversal -- into "callers" relations.
//
// Deliberately does NOT reclassify a _test.go caller as "tests" the way
// ReferenceRelations does: the spec's tests-relation wording is specific to
// references ("a reference found in a _test.go file"), not to callers/
// callees in general -- a test file calling a function is already fully
// described by "callers", and folding it into "tests" too would blur two
// different questions ("what exercises this" vs. "what calls this"). Don't
// "fix" this to match ReferenceRelations; it's intentional.
func CallerRelations(fromKey string, callers []Loc, source string, resolve Resolver) []Relation {
	out := make([]Relation, 0, len(callers))
	for _, loc := range callers {
		out = append(out, toRelation(fromKey, loc, "callers", source, resolve))
	}
	return out
}

// DedupResolvedRelations filters relations down to the ones a caller (e.g.
// cmd/ragrep's `code expand`) should actually persist via
// codestore.ReplaceRelations: unresolved relations (ToKey == "") are dropped
// entirely -- symbol_edges has no columns for ToPath/ToPosition, see
// Relation's own doc comment -- and exact duplicates (same FromKey, ToKey,
// Kind, and Source) collapse to one, first occurrence wins. This matters
// because symbol_edges has a UNIQUE(from_key, to_key, kind, source)
// constraint but an LSP query returns one location per reference/call site:
// a symbol referenced or called twice from the same enclosing symbol
// produces two Relations that resolve to the identical edge, which would
// otherwise fail that constraint. Order among survivors is preserved.
func DedupResolvedRelations(relations []Relation) []Relation {
	out := make([]Relation, 0, len(relations))
	seen := make(map[Relation]bool, len(relations))
	for _, r := range relations {
		if r.ToKey == "" || seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	return out
}

// CalleeRelations converts callHierarchy/outgoingCalls results -- already
// reduced by the caller to each call's "to" location, one hop only, no
// further traversal -- into "callees" relations.
//
// Same deliberate asymmetry as CallerRelations: a callee in a _test.go file
// (e.g. a helper only test code calls) stays "callees", never reclassified
// to "tests" -- see CallerRelations' doc comment.
func CalleeRelations(fromKey string, callees []Loc, source string, resolve Resolver) []Relation {
	out := make([]Relation, 0, len(callees))
	for _, loc := range callees {
		out = append(out, toRelation(fromKey, loc, "callees", source, resolve))
	}
	return out
}

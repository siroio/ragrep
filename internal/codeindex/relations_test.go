package codeindex

import (
	"fmt"
	"reflect"
	"testing"
)

// resolverFromMap builds a Resolver backed by a fixed path->key table,
// keyed "path:line" -- enough for these table-driven tests without needing
// a real codestore.Store.
func resolverFromMap(m map[string]string) Resolver {
	return func(path string, line int) (string, bool) {
		key, ok := m[locKey(path, line)]
		return key, ok
	}
}

func locKey(path string, line int) string {
	return fmt.Sprintf("%s:%d", path, line)
}

func TestDefinitionRelations(t *testing.T) {
	resolve := resolverFromMap(map[string]string{
		"pkg/impl.go:5": "impl-key",
	})

	tests := []struct {
		name string
		locs []Loc
		want []Relation
	}{
		{
			name: "resolved",
			locs: []Loc{{Path: "pkg/impl.go", Position: Position{Line: 5}}},
			want: []Relation{{FromKey: "from-key", ToKey: "impl-key", Kind: "definition", Source: "gopls"}},
		},
		{
			name: "unresolved: never fabricate a key",
			locs: []Loc{{Path: "pkg/external.go", Position: Position{Line: 9, Character: 2}}},
			want: []Relation{{
				FromKey: "from-key", Kind: "definition", Source: "gopls",
				ToPath: "pkg/external.go", ToPosition: Position{Line: 9, Character: 2},
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DefinitionRelations("from-key", tt.locs, "gopls", resolve)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("DefinitionRelations() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

// A reference located in a _test.go file must be classified "tests" instead
// of "references"; every other reference stays "references". Both branches
// use the same resolver so a single table covers the resolved/unresolved
// axis too.
func TestReferenceRelationsClassifiesTestFiles(t *testing.T) {
	resolve := resolverFromMap(map[string]string{
		"pkg/impl.go:3":      "impl-key",
		"pkg/impl_test.go:7": "test-key",
	})

	locs := []Loc{
		{Path: "pkg/impl.go", Position: Position{Line: 3}},       // resolved, non-test
		{Path: "pkg/impl_test.go", Position: Position{Line: 7}},  // resolved, test file
		{Path: "pkg/other_test.go", Position: Position{Line: 1}}, // unresolved, test file
		{Path: "pkg/unindexed.go", Position: Position{Line: 4}},  // unresolved, non-test
	}

	got := ReferenceRelations("from-key", locs, "gopls", resolve)
	want := []Relation{
		{FromKey: "from-key", ToKey: "impl-key", Kind: "references", Source: "gopls"},
		{FromKey: "from-key", ToKey: "test-key", Kind: "tests", Source: "gopls"},
		{FromKey: "from-key", Kind: "tests", Source: "gopls", ToPath: "pkg/other_test.go", ToPosition: Position{Line: 1}},
		{FromKey: "from-key", Kind: "references", Source: "gopls", ToPath: "pkg/unindexed.go", ToPosition: Position{Line: 4}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ReferenceRelations() = %#v, want %#v", got, want)
	}
}

// DB内にない外部依存シンボルを、存在する内部シンボルとして偽装しない: a
// location the resolver can't place must produce ToKey == "" with the
// location's own path/position preserved, never a made-up key.
func TestRelationsNeverFabricateUnresolvedKey(t *testing.T) {
	neverResolves := Resolver(func(string, int) (string, bool) { return "", false })

	loc := Loc{Path: "vendor/external/dep.go", Position: Position{Line: 42, Character: 8}}
	for _, tc := range []struct {
		name string
		got  []Relation
	}{
		{"definition", DefinitionRelations("k", []Loc{loc}, "gopls", neverResolves)},
		{"references", ReferenceRelations("k", []Loc{loc}, "gopls", neverResolves)},
		{"callers", CallerRelations("k", []Loc{loc}, "gopls", neverResolves)},
		{"callees", CalleeRelations("k", []Loc{loc}, "gopls", neverResolves)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if len(tc.got) != 1 {
				t.Fatalf("got %d relations, want 1", len(tc.got))
			}
			r := tc.got[0]
			if r.ToKey != "" {
				t.Fatalf("ToKey = %q, want empty (must never fabricate a key for an unresolved location)", r.ToKey)
			}
			if r.ToPath != loc.Path || r.ToPosition != loc.Position {
				t.Fatalf("ToPath/ToPosition = %q/%v, want %q/%v", r.ToPath, r.ToPosition, loc.Path, loc.Position)
			}
		})
	}
}

// CallerRelations/CalleeRelations map each incoming/outgoing call location
// 1:1 to a Relation -- exactly one hop, no recursive expansion (the LSP
// call-hierarchy shape itself has no nested "from.from"/"to.to", so this is
// mostly a shape/count assertion).
func TestCallerAndCalleeRelationsAreOneHop(t *testing.T) {
	resolve := resolverFromMap(map[string]string{
		"pkg/caller.go:1": "caller-key",
		"pkg/callee.go:2": "callee-key",
	})

	callers := CallerRelations("target-key", []Loc{{Path: "pkg/caller.go", Position: Position{Line: 1}}}, "gopls", resolve)
	want := []Relation{{FromKey: "target-key", ToKey: "caller-key", Kind: "callers", Source: "gopls"}}
	if !reflect.DeepEqual(callers, want) {
		t.Fatalf("CallerRelations() = %#v, want %#v", callers, want)
	}

	callees := CalleeRelations("target-key", []Loc{{Path: "pkg/callee.go", Position: Position{Line: 2}}}, "gopls", resolve)
	wantCallees := []Relation{{FromKey: "target-key", ToKey: "callee-key", Kind: "callees", Source: "gopls"}}
	if !reflect.DeepEqual(callees, wantCallees) {
		t.Fatalf("CalleeRelations() = %#v, want %#v", callees, wantCallees)
	}
}

// DedupResolvedRelations must (a) drop every unresolved relation (ToKey ==
// "") -- symbol_edges has no columns for ToPath/ToPosition, so these must
// never reach codestore.ReplaceRelations -- and (b) collapse duplicate
// resolved relations (same FromKey/ToKey/Kind/Source) down to one, first
// occurrence wins: an LSP query naturally returns one location per
// reference/call site, so a symbol referenced twice from the same enclosing
// symbol produces two Relations that would otherwise collide on
// symbol_edges' UNIQUE(from_key, to_key, kind, source) constraint.
func TestDedupResolvedRelationsFiltersUnresolvedAndDedups(t *testing.T) {
	in := []Relation{
		{FromKey: "f", ToKey: "a", Kind: "references", Source: "gopls"},
		{FromKey: "f", ToKey: "a", Kind: "references", Source: "gopls"}, // exact duplicate
		{FromKey: "f", Kind: "references", Source: "gopls", ToPath: "ext.go", ToPosition: Position{Line: 1}}, // unresolved
		{FromKey: "f", ToKey: "b", Kind: "references", Source: "gopls"},
		{FromKey: "f", ToKey: "a", Kind: "callers", Source: "gopls"}, // same ToKey, different kind: not a duplicate
	}
	want := []Relation{
		{FromKey: "f", ToKey: "a", Kind: "references", Source: "gopls"},
		{FromKey: "f", ToKey: "b", Kind: "references", Source: "gopls"},
		{FromKey: "f", ToKey: "a", Kind: "callers", Source: "gopls"},
	}
	got := DedupResolvedRelations(in)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DedupResolvedRelations() = %#v, want %#v", got, want)
	}
}

func TestRelationHelpersPreserveInputOrder(t *testing.T) {
	resolve := resolverFromMap(map[string]string{
		"a.go:1": "k1",
		"a.go:2": "k2",
		"a.go:3": "k3",
	})
	locs := []Loc{
		{Path: "a.go", Position: Position{Line: 1}},
		{Path: "a.go", Position: Position{Line: 2}},
		{Path: "a.go", Position: Position{Line: 3}},
	}
	got := ReferenceRelations("from", locs, "gopls", resolve)
	if len(got) != 3 || got[0].ToKey != "k1" || got[1].ToKey != "k2" || got[2].ToKey != "k3" {
		t.Fatalf("ReferenceRelations() order = %#v, want k1,k2,k3 in order", got)
	}
}

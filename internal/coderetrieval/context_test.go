package coderetrieval

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/siroio/ragrep/internal/codeindex"
	"github.com/siroio/ragrep/internal/codestore"
)

func hit(key, path string, start, end int) codestore.SymbolHit {
	return codestore.SymbolHit{
		Key:           key,
		Kind:          "function",
		Name:          key,
		QualifiedName: key,
		Signature:     "func " + key + "()",
		Path:          path,
		StartLine:     start,
		EndLine:       end,
	}
}

func TestDedupCandidates_OverlappingRangesMerge(t *testing.T) {
	hits := []codestore.SymbolHit{
		hit("a", "x.go", 10, 20),
		hit("b", "x.go", 15, 25), // overlaps a
		hit("c", "x.go", 40, 50), // disjoint
		hit("d", "y.go", 10, 20), // different file, same lines as a: not a dup
	}

	out := DedupCandidates(hits)

	if len(out) != 3 {
		t.Fatalf("len(out) = %d, want 3 (a+b merged, c, d): %+v", len(out), out)
	}
	if out[0].Key != "a" {
		t.Fatalf("out[0].Key = %q, want %q (first-seen kept as representative)", out[0].Key, "a")
	}
	if out[0].StartLine != 10 || out[0].EndLine != 25 {
		t.Fatalf("merged range = [%d,%d], want [10,25] (union of a and b)", out[0].StartLine, out[0].EndLine)
	}
}

func TestDedupCandidates_NoOverlapKeepsAll(t *testing.T) {
	hits := []codestore.SymbolHit{
		hit("a", "x.go", 1, 5),
		hit("b", "x.go", 10, 15),
	}
	out := DedupCandidates(hits)
	if len(out) != 2 {
		t.Fatalf("len(out) = %d, want 2", len(out))
	}
}

// TestDedupCandidates_BridgingOrderCascades covers the controller-flagged
// bug: a group formed by widening (a merged with c) must itself be
// re-checked against every other existing group (b), not just left as-is --
// otherwise a later item that bridges two earlier, disjoint groups leaves
// the output with two entries that still overlap each other.
func TestDedupCandidates_BridgingOrderCascades(t *testing.T) {
	hits := []codestore.SymbolHit{
		hit("a", "x.go", 0, 5),   // group 1
		hit("b", "x.go", 20, 25), // group 2, disjoint from a
		hit("c", "x.go", 4, 22),  // bridges a and b: overlaps both
	}

	out := DedupCandidates(hits)

	if len(out) != 1 {
		t.Fatalf("len(out) = %d, want 1 (c must bridge a and b into one group): %+v", len(out), out)
	}
	if out[0].Key != "a" {
		t.Fatalf("out[0].Key = %q, want %q (first-seen representative)", out[0].Key, "a")
	}
	if out[0].StartLine != 0 || out[0].EndLine != 25 {
		t.Fatalf("merged range = [%d,%d], want [0,25] (union of a, b, c)", out[0].StartLine, out[0].EndLine)
	}
}

func TestBuildContextPack_SelectedSymbolsDedupOverlappingRanges(t *testing.T) {
	// "outer" contains "inner" entirely (a struct and one of its own
	// methods, say); "partial" merely overlaps "outer"'s tail. All three
	// are in the same file and must collapse into a single packed body.
	symTable := map[string]codeindex.Symbol{
		"outer": {
			Key: "outer", QualifiedName: "Outer", Path: "x.go", Body: "type Outer struct{ ... }",
			Range: codeindex.Range{Start: codeindex.Position{Line: 1}, End: codeindex.Position{Line: 20}},
		},
		"inner": {
			Key: "inner", QualifiedName: "Outer.Method", Path: "x.go", Body: "func (o *Outer) Method() {}",
			Range: codeindex.Range{Start: codeindex.Position{Line: 5}, End: codeindex.Position{Line: 10}},
		},
		"partial": {
			Key: "partial", QualifiedName: "Adjacent", Path: "x.go", Body: "func Adjacent() {}",
			Range: codeindex.Range{Start: codeindex.Position{Line: 18}, End: codeindex.Position{Line: 30}},
		},
	}

	opts := AssembleOptions{
		Budget:       100_000,
		SelectedKeys: []string{"outer", "inner", "partial"},
		GetSymbol:    func(k string) (codeindex.Symbol, error) { return symTable[k], nil },
		GetRelations: func(k string) ([]codeindex.Relation, error) { return nil, nil },
	}

	pack, err := BuildContextPack(nil, opts)
	if err != nil {
		t.Fatalf("BuildContextPack: %v", err)
	}
	if len(pack.Symbols) != 1 {
		t.Fatalf("len(pack.Symbols) = %d, want 1 (nested + overlapping selections must collapse): %+v", len(pack.Symbols), pack.Symbols)
	}
	got := pack.Symbols[0]
	if got.Key != "outer" {
		t.Fatalf("pack.Symbols[0].Key = %q, want %q (first-selected representative)", got.Key, "outer")
	}
	if got.Range.Start.Line != 1 || got.Range.End.Line != 30 {
		t.Fatalf("merged range = [%d,%d], want [1,30] (union of outer, inner, partial)", got.Range.Start.Line, got.Range.End.Line)
	}
}

func TestBuildContextPack_MetadataOnlyStaysWithinBudget(t *testing.T) {
	hits := []codestore.SymbolHit{
		hit("a", "x.go", 1, 5),
		hit("b", "y.go", 1, 5),
		hit("c", "z.go", 1, 5),
	}
	opts := AssembleOptions{Budget: 100_000}

	pack, err := BuildContextPack(hits, opts)
	if err != nil {
		t.Fatalf("BuildContextPack: %v", err)
	}
	if pack.Truncated {
		t.Fatalf("pack.Truncated = true, want false: skipped=%v usedChars=%d", pack.Skipped, pack.UsedChars)
	}
	if len(pack.Candidates) != 3 {
		t.Fatalf("len(pack.Candidates) = %d, want 3", len(pack.Candidates))
	}
	if len(pack.Skipped) != 0 {
		t.Fatalf("pack.Skipped = %v, want empty", pack.Skipped)
	}
	if pack.UsedChars <= 0 || pack.UsedChars > pack.Budget {
		t.Fatalf("pack.UsedChars = %d, want in (0, %d]", pack.UsedChars, pack.Budget)
	}
}

func TestBuildContextPack_StagedSymbolBodiesAndRelations(t *testing.T) {
	hits := []codestore.SymbolHit{hit("a", "x.go", 1, 5), hit("b", "x.go", 10, 15)}

	symTable := map[string]codeindex.Symbol{
		"a": {
			Key: "a", QualifiedName: "A", Path: "x.go", Body: "func A() {}",
			Range: codeindex.Range{Start: codeindex.Position{Line: 1}, End: codeindex.Position{Line: 5}},
		},
		"b": {
			Key: "b", QualifiedName: "B", Path: "x.go", Body: "func B() {}",
			Range: codeindex.Range{Start: codeindex.Position{Line: 10}, End: codeindex.Position{Line: 15}},
		},
	}
	relTable := map[string][]codeindex.Relation{
		"a": {{FromKey: "a", ToKey: "b", Kind: "calls", Source: "gopls"}},
		"b": nil,
	}

	opts := AssembleOptions{
		Budget:       100_000,
		SelectedKeys: []string{"a", "b"},
		GetSymbol:    func(k string) (codeindex.Symbol, error) { return symTable[k], nil },
		GetRelations: func(k string) ([]codeindex.Relation, error) { return relTable[k], nil },
	}

	pack, err := BuildContextPack(hits, opts)
	if err != nil {
		t.Fatalf("BuildContextPack: %v", err)
	}
	if pack.Truncated {
		t.Fatalf("pack.Truncated = true, want false")
	}
	if len(pack.Symbols) != 2 {
		t.Fatalf("len(pack.Symbols) = %d, want 2", len(pack.Symbols))
	}
	if len(pack.Relations) != 1 || pack.Relations[0].ToKey != "b" {
		t.Fatalf("pack.Relations = %+v, want one relation a->b", pack.Relations)
	}
}

func TestBuildContextPack_BudgetExhaustionTruncates(t *testing.T) {
	hits := []codestore.SymbolHit{hit("a", "x.go", 1, 5), hit("b", "y.go", 1, 5)}
	symTable := map[string]codeindex.Symbol{
		"a": {Key: "a", QualifiedName: "A", Path: "x.go", Body: "func A() { /* a very long body that costs a lot of budget characters to include */ }"},
		"b": {Key: "b", QualifiedName: "B", Path: "y.go", Body: "func B() { /* another very long body that costs a lot of budget characters too */ }"},
	}

	// Budget large enough for both candidates' metadata but not both bodies.
	metaOnly, err := BuildContextPack(hits, AssembleOptions{Budget: 100_000})
	if err != nil {
		t.Fatalf("BuildContextPack (measure): %v", err)
	}
	budget := metaOnly.UsedChars + 50 // room for candidates + a sliver, not two bodies

	opts := AssembleOptions{
		Budget:       budget,
		SelectedKeys: []string{"a", "b"},
		GetSymbol:    func(k string) (codeindex.Symbol, error) { return symTable[k], nil },
		GetRelations: func(k string) ([]codeindex.Relation, error) { return nil, nil },
	}

	pack, err := BuildContextPack(hits, opts)
	if err != nil {
		t.Fatalf("BuildContextPack: %v", err)
	}
	if !pack.Truncated {
		t.Fatalf("pack.Truncated = false, want true: usedChars=%d budget=%d", pack.UsedChars, budget)
	}
	if len(pack.Skipped) == 0 {
		t.Fatalf("pack.Skipped is empty, want at least one skipped item")
	}
	if pack.UsedChars > pack.Budget {
		t.Fatalf("pack.UsedChars = %d exceeds budget %d: truncation must never silently overflow", pack.UsedChars, pack.Budget)
	}
	if len(pack.Candidates) != 2 {
		t.Fatalf("len(pack.Candidates) = %d, want 2 (metadata is highest priority, must not be starved)", len(pack.Candidates))
	}
}

func TestBuildContextPack_GetSymbolErrorPropagates(t *testing.T) {
	wantErr := errors.New("boom")
	opts := AssembleOptions{
		Budget:       1000,
		SelectedKeys: []string{"missing"},
		GetSymbol:    func(k string) (codeindex.Symbol, error) { return codeindex.Symbol{}, wantErr },
	}
	_, err := BuildContextPack(nil, opts)
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want wrapping %v", err, wantErr)
	}
}

func TestContextPack_JSONRoundTrip(t *testing.T) {
	hits := []codestore.SymbolHit{hit("a", "x.go", 1, 5)}
	symTable := map[string]codeindex.Symbol{
		"a": {Key: "a", QualifiedName: "A", Path: "x.go", Body: "func A() {}"},
	}
	opts := AssembleOptions{
		Budget:       100_000,
		SelectedKeys: []string{"a"},
		GetSymbol:    func(k string) (codeindex.Symbol, error) { return symTable[k], nil },
		GetRelations: func(k string) ([]codeindex.Relation, error) { return nil, nil },
	}
	pack, err := BuildContextPack(hits, opts)
	if err != nil {
		t.Fatalf("BuildContextPack: %v", err)
	}

	b, err := json.Marshal(pack)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var out ContextPack
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if out.UsedChars != pack.UsedChars || out.Truncated != pack.Truncated || len(out.Candidates) != len(pack.Candidates) {
		t.Fatalf("round trip mismatch: got %+v, want %+v", out, pack)
	}
}

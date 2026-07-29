package codestore

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/siroio/ragrep/internal/codeindex"
)

// fakeEmbedder returns pre-registered vectors keyed by the exact text
// embed() is called with (i.e. codeindex.RenderEmbeddingText's output), and
// counts calls so tests can assert the re-embed cache rule. Calling embed
// with unregistered text is a test bug, not a runtime condition, so it
// fails the test immediately via t.Fatalf rather than returning an error.
type fakeEmbedder struct {
	t       *testing.T
	vectors map[string][]float32
	calls   int
}

func newFakeEmbedder(t *testing.T) *fakeEmbedder {
	return &fakeEmbedder{t: t, vectors: map[string][]float32{}}
}

func (f *fakeEmbedder) register(sym codeindex.Symbol, v []float32) {
	f.vectors[codeindex.RenderEmbeddingText(sym)] = v
}

func (f *fakeEmbedder) embed(text string) ([]float32, error) {
	f.calls++
	v, ok := f.vectors[text]
	if !ok {
		f.t.Fatalf("fakeEmbedder: no registered vector for embedding text %q", text)
	}
	return v, nil
}

func openTestStore(t *testing.T, dim int) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "code.db")
	s, err := Open(path, "test-model", dim)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// sym builds a minimal, valid codeindex.Symbol for tests. Names/identifiers
// used across these tests are always >= 3 chars: fts5's trigram tokenizer
// only indexes tokens of length >= 3.
func sym(key, name, qualifiedName, documentation, body string) codeindex.Symbol {
	bodyHash := fmt.Sprintf("bodyhash:%s", body)
	s := codeindex.Symbol{
		Key:           key,
		Language:      "go",
		Kind:          "function",
		Name:          name,
		QualifiedName: qualifiedName,
		Signature:     "func " + name + "()",
		Documentation: documentation,
		Container:     "",
		Path:          "pkg/file.go",
		Range: codeindex.Range{
			Start: codeindex.Position{Line: 1, Character: 0},
			End:   codeindex.Position{Line: 3, Character: 1},
		},
		Body:     body,
		BodyHash: bodyHash,
	}
	// Mirrors what codeindex.Extract does: EmbeddingText is derived from the
	// other fields, never set independently by a caller.
	s.EmbeddingText = codeindex.RenderEmbeddingText(s)
	return s
}

func TestUpsertSymbolsSkipsReindexWhenFileHashUnchanged(t *testing.T) {
	s := openTestStore(t, 3)
	fe := newFakeEmbedder(t)

	s1 := sym("k1", "ParseConfig", "ParseConfig", "", "func ParseConfig() {}")
	fe.register(s1, []float32{1, 0, 0})

	changed, err := s.UpsertSymbols("pkg/file.go", "filehash-1", []codeindex.Symbol{s1}, 0, fe.embed)
	if err != nil {
		t.Fatalf("first UpsertSymbols: %v", err)
	}
	if !changed {
		t.Fatal("first UpsertSymbols: changed = false, want true")
	}
	if fe.calls != 1 {
		t.Fatalf("embed calls after first upsert = %d, want 1", fe.calls)
	}

	// Same file hash, even if the caller (hypothetically) re-parsed the same
	// content into an equal symbol list: must skip without touching embed.
	changed, err = s.UpsertSymbols("pkg/file.go", "filehash-1", []codeindex.Symbol{s1}, 0, fe.embed)
	if err != nil {
		t.Fatalf("second UpsertSymbols: %v", err)
	}
	if changed {
		t.Fatal("second UpsertSymbols with unchanged file_hash: changed = true, want false")
	}
	if fe.calls != 1 {
		t.Fatalf("embed calls after second upsert = %d, want still 1 (embed must not be called)", fe.calls)
	}
}

func TestUpsertSymbolsRemovesOrphanRowsAcrossAllIndexes(t *testing.T) {
	s := openTestStore(t, 3)
	fe := newFakeEmbedder(t)

	k1 := sym("k1", "ParseConfig", "ParseConfig", "", "func ParseConfig() {}")
	k2 := sym("k2", "LoadSettings", "LoadSettings", "", "func LoadSettings() {}")
	fe.register(k1, []float32{1, 0, 0})
	fe.register(k2, []float32{0, 1, 0})

	if _, err := s.UpsertSymbols("pkg/file.go", "filehash-1", []codeindex.Symbol{k1, k2}, 0, fe.embed); err != nil {
		t.Fatalf("initial upsert: %v", err)
	}

	var k1ID int64
	if err := s.db.QueryRow(`SELECT id FROM symbols WHERE key='k1'`).Scan(&k1ID); err != nil {
		t.Fatalf("lookup k1 id: %v", err)
	}

	if err := s.ReplaceRelations(1, "k1", []string{"calls"}, []codeindex.Relation{
		{FromKey: "k1", ToKey: "k2", Kind: "calls", Source: "static"},
	}); err != nil {
		t.Fatalf("ReplaceRelations: %v", err)
	}

	// Re-index the file with k1 removed (only k2 remains).
	changed, err := s.UpsertSymbols("pkg/file.go", "filehash-2", []codeindex.Symbol{k2}, 0, fe.embed)
	if err != nil {
		t.Fatalf("re-upsert dropping k1: %v", err)
	}
	if !changed {
		t.Fatal("re-upsert dropping k1: changed = false, want true")
	}

	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM symbols WHERE key='k1'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("symbols row for k1 still present after removal")
	}

	if err := s.db.QueryRow(`SELECT COUNT(*) FROM symbol_vec WHERE rowid=?`, k1ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("symbol_vec row for k1 (rowid=%d) still present after removal", k1ID)
	}

	if err := s.db.QueryRow(`SELECT COUNT(*) FROM symbol_edges WHERE from_key='k1'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("symbol_edges row from k1 still present after removal")
	}

	// FTS: k1's old rowid must no longer be found by a query that used to
	// match its name.
	hits, err := s.SearchSymbolsText("ParseConfig", 10)
	if err != nil {
		t.Fatalf("SearchSymbolsText: %v", err)
	}
	for _, h := range hits {
		if h.Key == "k1" {
			t.Errorf("SearchSymbolsText still returns removed symbol k1")
		}
	}

	// k2 (the surviving symbol) must still be intact.
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM symbols WHERE key='k2'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("symbols row for surviving k2 missing")
	}
}

func TestUpsertSymbolsReEmbedsOnlyWhenDocOrBodyChanges(t *testing.T) {
	s := openTestStore(t, 3)
	fe := newFakeEmbedder(t)

	k1 := sym("k1", "ParseConfig", "ParseConfig", "", "func ParseConfig() {}")
	fe.register(k1, []float32{1, 0, 0})
	if _, err := s.UpsertSymbols("pkg/file.go", "filehash-1", []codeindex.Symbol{k1}, 0, fe.embed); err != nil {
		t.Fatalf("initial upsert: %v", err)
	}
	if fe.calls != 1 {
		t.Fatalf("calls after initial insert = %d, want 1", fe.calls)
	}

	// Re-upsert with a new file hash, but the symbol content (doc + body)
	// unchanged: must not re-embed.
	changed, err := s.UpsertSymbols("pkg/file.go", "filehash-2", []codeindex.Symbol{k1}, 0, fe.embed)
	if err != nil {
		t.Fatalf("no-op content re-upsert: %v", err)
	}
	if !changed {
		t.Fatal("re-upsert with new file_hash: changed = false, want true")
	}
	if fe.calls != 1 {
		t.Fatalf("calls after content-unchanged re-upsert = %d, want still 1", fe.calls)
	}

	// Now change only Documentation: must re-embed.
	k1Doc := k1
	k1Doc.Documentation = "ParseConfig parses the config file."
	k1Doc.EmbeddingText = codeindex.RenderEmbeddingText(k1Doc)
	fe.register(k1Doc, []float32{0, 1, 0})
	if _, err := s.UpsertSymbols("pkg/file.go", "filehash-3", []codeindex.Symbol{k1Doc}, 0, fe.embed); err != nil {
		t.Fatalf("doc-changed re-upsert: %v", err)
	}
	if fe.calls != 2 {
		t.Fatalf("calls after doc-changed re-upsert = %d, want 2", fe.calls)
	}

	// Now change only Body (Documentation held fixed at k1Doc's value):
	// must also re-embed. This is the BodyHash-changed branch, distinct
	// from the Documentation-changed branch exercised above.
	k1Body := k1Doc
	k1Body.Body = "func ParseConfig() { return nil }"
	k1Body.BodyHash = fmt.Sprintf("bodyhash:%s", k1Body.Body)
	k1Body.EmbeddingText = codeindex.RenderEmbeddingText(k1Body)
	fe.register(k1Body, []float32{0, 0, 1})
	if _, err := s.UpsertSymbols("pkg/file.go", "filehash-4", []codeindex.Symbol{k1Body}, 0, fe.embed); err != nil {
		t.Fatalf("body-changed re-upsert: %v", err)
	}
	if fe.calls != 3 {
		t.Fatalf("calls after body-changed re-upsert = %d, want 3", fe.calls)
	}
}

// TestUpsertSymbolsResyncsFTSWhenIndexedColumnChanges exercises updateSymbol's
// FTS resync (the fts5 external-content 'delete' of the old indexed values
// followed by an insert of the new ones) on its own, independent of the
// re-embed cache rule: Documentation and Body are held fixed (so embed is
// NOT called on the second upsert — asserted below) while Name/QualifiedName/
// Signature change. If the FTS resync were missing or used stale rowid/old
// values incorrectly, the old name would still be findable and/or the new
// name would not be.
//
// Note: the store trusts the caller-supplied Symbol.Key; it never recomputes
// it. In production codeindex.Extract derives Key from language+path+kind+
// qualified_name+signature, so a real qualified_name/signature change also
// changes Key (handled by the insert/delete paths, covered by
// TestUpsertSymbolsRemovesOrphanRowsAcrossAllIndexes). This test targets the
// update-in-place path specifically, so it deliberately keeps Key constant
// while changing the other indexed columns.
func TestUpsertSymbolsResyncsFTSWhenIndexedColumnChanges(t *testing.T) {
	s := openTestStore(t, 3)
	fe := newFakeEmbedder(t)

	fixedBody := "func Symbol() {}" // held constant so BodyHash never changes
	k1 := sym("k1", "AlphaWidget", "AlphaWidget", "", fixedBody)
	fe.register(k1, []float32{1, 0, 0})
	if _, err := s.UpsertSymbols("pkg/file.go", "filehash-1", []codeindex.Symbol{k1}, 0, fe.embed); err != nil {
		t.Fatalf("initial upsert: %v", err)
	}

	hits, err := s.SearchSymbolsText("AlphaWidget", 10)
	if err != nil {
		t.Fatalf("SearchSymbolsText(old name) before rename: %v", err)
	}
	if len(hits) != 1 || hits[0].Key != "k1" {
		t.Fatalf("SearchSymbolsText(AlphaWidget) before rename = %+v, want single hit for k1", hits)
	}

	k1Renamed := sym("k1", "BetaGadget", "BetaGadget", "", fixedBody)
	if _, err := s.UpsertSymbols("pkg/file.go", "filehash-2", []codeindex.Symbol{k1Renamed}, 0, fe.embed); err != nil {
		t.Fatalf("rename re-upsert: %v", err)
	}
	if fe.calls != 1 {
		t.Fatalf("embed calls after rename-only re-upsert = %d, want still 1 (doc/body unchanged)", fe.calls)
	}

	hits, err = s.SearchSymbolsText("AlphaWidget", 10)
	if err != nil {
		t.Fatalf("SearchSymbolsText(old name) after rename: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("SearchSymbolsText(AlphaWidget) after rename = %+v, want no hits (stale FTS entry)", hits)
	}

	hits, err = s.SearchSymbolsText("BetaGadget", 10)
	if err != nil {
		t.Fatalf("SearchSymbolsText(new name) after rename: %v", err)
	}
	if len(hits) != 1 || hits[0].Key != "k1" {
		t.Fatalf("SearchSymbolsText(BetaGadget) after rename = %+v, want single hit for k1", hits)
	}
}

// TestSearchSymbolsHybridExactMatchBeatsSemanticSimilarity constructs a case
// where the exact-name match would actually LOSE under plain RRF fusion (no
// exact-match pin), so the assertion is falsifiable rather than
// coincidentally true. See task-5-report.md's fix-round-1 section for the
// experiment confirming this: with the pin block temporarily bypassed, this
// test fails (a decoy ranks first, not "exact-key").
//
// "exact" has a lean FTS document (its own name, once, in each column) and
// an embedding placed maximally far from the query vector, so it gets only
// one weak contribution (FTS rank 1) to its RRF score and a near-worst
// vector rank.
//
// Each "decoy" is a lexically-noisy near-duplicate: its Documentation
// repeats the query substring many times, which drives up its FTS bm25
// score enough to outrank "exact"'s single occurrence — and its embedding
// is placed exactly at the query vector, giving it the best possible vector
// rank too. A decoy that wins on both signals accumulates a higher fused
// RRF score than "exact", which only wins outright on one signal (FTS) and
// is worst-ranked on the other (vector).
func TestSearchSymbolsHybridExactMatchBeatsSemanticSimilarity(t *testing.T) {
	s := openTestStore(t, 3)
	fe := newFakeEmbedder(t)

	queryVector := []float32{1, 0, 0}

	exact := sym("exact-key", "ParseConfig", "ParseConfig", "", "func ParseConfig() {}")
	fe.register(exact, []float32{0, 0, 1}) // far from queryVector

	symbols := []codeindex.Symbol{exact}
	const numDecoys = 4
	for i := 0; i < numDecoys; i++ {
		name := fmt.Sprintf("ParseConfigDecoy%d", i)
		decoy := sym(name, name, name,
			strings.Repeat("ParseConfig ", 8), // heavy repetition inflates bm25 past "exact"'s
			"func "+name+"() {}")
		fe.register(decoy, queryVector) // sits exactly at the query vector
		symbols = append(symbols, decoy)
	}

	if _, err := s.UpsertSymbols("pkg/file.go", "filehash-1", symbols, 0, fe.embed); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	hits, err := s.SearchSymbolsHybrid("ParseConfig", queryVector, 10)
	if err != nil {
		t.Fatalf("SearchSymbolsHybrid: %v", err)
	}
	if len(hits) < 2 {
		t.Fatalf("got %d hits, want >= 2", len(hits))
	}
	if hits[0].Key != "exact-key" {
		t.Errorf("hits[0].Key = %q, want exact-key (exact identifier match must be pinned above decoys that outrank it under plain RRF)", hits[0].Key)
	}
	if !hits[0].ExactMatch {
		t.Errorf("hits[0].ExactMatch = false, want true")
	}
}

func TestSearchSymbolsVectorRecallsSemanticMatchWithoutLexicalOverlap(t *testing.T) {
	s := openTestStore(t, 3)
	fe := newFakeEmbedder(t)

	// near has zero lexical relation to the query text we'll use ("totally
	// unrelated tokens") but an embedding close to the query vector.
	near := sym("near-key", "ComputeTotal", "ComputeTotal", "", "func ComputeTotal() {}")
	fe.register(near, []float32{1, 0, 0})

	far := sym("far-key", "RenderWidget", "RenderWidget", "", "func RenderWidget() {}")
	fe.register(far, []float32{0, 0, 1})

	if _, err := s.UpsertSymbols("pkg/file.go", "filehash-1", []codeindex.Symbol{near, far}, 0, fe.embed); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	hits, err := s.SearchSymbolsVector([]float32{1, 0, 0}, 1)
	if err != nil {
		t.Fatalf("SearchSymbolsVector: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("got %d hits, want 1", len(hits))
	}
	if hits[0].Key != "near-key" {
		t.Errorf("hits[0].Key = %q, want near-key", hits[0].Key)
	}
	if hits[0].VecRank != 1 {
		t.Errorf("hits[0].VecRank = %d, want 1", hits[0].VecRank)
	}
}

func TestSearchResultsCarryNoBody(t *testing.T) {
	// Structural guarantee: SymbolHit must not have a Body field at all, so
	// no code path can ever leak full source into a search result.
	if _, ok := reflect.TypeOf(SymbolHit{}).FieldByName("Body"); ok {
		t.Fatal("SymbolHit must not have a Body field")
	}

	s := openTestStore(t, 3)
	fe := newFakeEmbedder(t)
	bigBody := "func ParseConfig() {\n\t// a lot of source code here\n\treturn nil\n}"
	k1 := sym("k1", "ParseConfig", "ParseConfig", "", bigBody)
	fe.register(k1, []float32{1, 0, 0})
	if _, err := s.UpsertSymbols("pkg/file.go", "filehash-1", []codeindex.Symbol{k1}, 0, fe.embed); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	hits, err := s.SearchSymbolsText("ParseConfig", 10)
	if err != nil {
		t.Fatalf("SearchSymbolsText: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("got %d hits, want 1", len(hits))
	}
	if hits[0].Signature == bigBody || hits[0].Path == bigBody {
		t.Fatal("a SymbolHit field leaked the full body text")
	}

	got, err := s.GetSymbol("k1")
	if err != nil {
		t.Fatalf("GetSymbol: %v", err)
	}
	if got.Body != bigBody {
		t.Errorf("GetSymbol.Body = %q, want %q", got.Body, bigBody)
	}
}

func TestGetSymbolReturnsErrNotFound(t *testing.T) {
	s := openTestStore(t, 3)
	_, err := s.GetSymbol("does-not-exist")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetSymbol on missing key: err=%v, want ErrNotFound", err)
	}
}

// ReplaceRelations must scope its delete to (fromKey, kind), not just
// fromKey: this is what makes repeated `code expand` calls for the same
// symbol but different relations accumulate a relation graph instead of
// each call wiping out every other kind the symbol had saved. Mirrors the
// coordinator-requested regression scenario directly: a references expand
// (saving both "references" and "tests" -- they share one LSP query, see
// codeindex.ReferenceRelations) followed by a callers expand must leave the
// references/tests edges intact; a later references expand that now finds
// nothing must still clear the old references/tests edges (kinds are
// passed explicitly, not inferred from an empty batch) without touching
// callers, or another symbol's edges.
func TestReplaceRelationsScopesDeleteToFromKeyAndKinds(t *testing.T) {
	s := openTestStore(t, 3)
	fe := newFakeEmbedder(t)

	k1 := sym("k1", "Target", "Target", "", "func Target() {}")
	k2 := sym("k2", "Other", "Other", "", "func Other() {}")
	k3 := sym("k3", "Third", "Third", "", "func Third() {}")
	fe.register(k1, []float32{1, 0, 0})
	fe.register(k2, []float32{0, 1, 0})
	fe.register(k3, []float32{0, 0, 1})
	if _, err := s.UpsertSymbols("pkg/file.go", "filehash-1", []codeindex.Symbol{k1, k2, k3}, 0, fe.embed); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// A references expand: saves both "references" and "tests" as one
	// group.
	if err := s.ReplaceRelations(1, "k1", []string{"references", "tests"}, []codeindex.Relation{
		{FromKey: "k1", ToKey: "k2", Kind: "references", Source: "gopls"},
		{FromKey: "k1", ToKey: "k3", Kind: "tests", Source: "gopls"},
	}); err != nil {
		t.Fatalf("references expand: %v", err)
	}
	// An unrelated symbol's edge, to prove from_key scoping still holds
	// throughout.
	if err := s.ReplaceRelations(1, "k2", []string{"references", "tests"}, []codeindex.Relation{
		{FromKey: "k2", ToKey: "k1", Kind: "references", Source: "gopls"},
	}); err != nil {
		t.Fatalf("k2 references expand: %v", err)
	}

	// A callers expand for the same symbol (k1): must not touch k1's
	// references/tests edges saved above, since "callers" is a disjoint
	// kind group.
	if err := s.ReplaceRelations(2, "k1", []string{"callers"}, []codeindex.Relation{
		{FromKey: "k1", ToKey: "k3", Kind: "callers", Source: "gopls"},
	}); err != nil {
		t.Fatalf("callers expand: %v", err)
	}

	assertEdgeCount := func(fromKey, kind string, want int) {
		t.Helper()
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM symbol_edges WHERE from_key=? AND kind=?`, fromKey, kind).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != want {
			t.Errorf("symbol_edges(from_key=%q, kind=%q) count = %d, want %d", fromKey, kind, n, want)
		}
	}

	assertEdgeCount("k1", "references", 1)
	assertEdgeCount("k1", "tests", 1)
	assertEdgeCount("k1", "callers", 1)
	assertEdgeCount("k2", "references", 1) // untouched by k1's callers expand

	// A later references expand for k1 that now finds nothing (e.g. the
	// reference was deleted from source) must still clear the stale
	// references/tests edges -- kinds is passed explicitly, so an empty
	// relations slice still deletes -- and must leave k1's callers edge and
	// k2's edge alone.
	if err := s.ReplaceRelations(3, "k1", []string{"references", "tests"}, nil); err != nil {
		t.Fatalf("empty references re-expand: %v", err)
	}
	assertEdgeCount("k1", "references", 0)
	assertEdgeCount("k1", "tests", 0)
	assertEdgeCount("k1", "callers", 1)
	assertEdgeCount("k2", "references", 1)
}

// --- RecordIndexRun: the sole write path for index_runs (replaces the
// raw-SQL insert cmd/ragrep used to do itself) ---

func TestRecordIndexRunInsertsRowAndReturnsIncrementingID(t *testing.T) {
	s := openTestStore(t, 3)

	when := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	id1, err := s.RecordIndexRun("pkg", "rev1", "go", "gopls", "v1.0.0", "test-model", when)
	if err != nil {
		t.Fatalf("RecordIndexRun: %v", err)
	}
	if id1 <= 0 {
		t.Fatalf("first RecordIndexRun id = %d, want > 0", id1)
	}

	id2, err := s.RecordIndexRun("pkg2", "rev2", "go", "gopls", "v1.0.0", "test-model", when)
	if err != nil {
		t.Fatalf("second RecordIndexRun: %v", err)
	}
	if id2 <= id1 {
		t.Fatalf("second RecordIndexRun id = %d, want > first id %d", id2, id1)
	}

	var scope, revision, language, serverName, serverVersion, modelID string
	if err := s.db.QueryRow(`
		SELECT scope, revision, language, server_name, server_version, model_id
		FROM index_runs WHERE id=?`, id1).Scan(&scope, &revision, &language, &serverName, &serverVersion, &modelID); err != nil {
		t.Fatalf("querying recorded run: %v", err)
	}
	if scope != "pkg" || revision != "rev1" || language != "go" || serverName != "gopls" || serverVersion != "v1.0.0" || modelID != "test-model" {
		t.Fatalf("recorded run = (%q,%q,%q,%q,%q,%q), want (pkg,rev1,go,gopls,v1.0.0,test-model)",
			scope, revision, language, serverName, serverVersion, modelID)
	}
}

// --- UpsertSymbols(..., runID, ...): symbols.index_run_id must carry the
// real run that produced the row, on both insert and update ---

func TestUpsertSymbolsStampsIndexRunID(t *testing.T) {
	s := openTestStore(t, 3)
	fe := newFakeEmbedder(t)

	k1 := sym("k1", "ParseConfig", "ParseConfig", "", "func ParseConfig() {}")
	fe.register(k1, []float32{1, 0, 0})

	runID, err := s.RecordIndexRun("pkg", "rev1", "go", "gopls", "v1", "test-model", time.Now())
	if err != nil {
		t.Fatalf("RecordIndexRun: %v", err)
	}

	if _, err := s.UpsertSymbols("pkg/file.go", "filehash-1", []codeindex.Symbol{k1}, runID, fe.embed); err != nil {
		t.Fatalf("insert upsert: %v", err)
	}
	var gotRunID int64
	if err := s.db.QueryRow(`SELECT index_run_id FROM symbols WHERE key='k1'`).Scan(&gotRunID); err != nil {
		t.Fatal(err)
	}
	if gotRunID != runID {
		t.Fatalf("index_run_id after insert = %d, want %d", gotRunID, runID)
	}

	// A later run updating the same symbol (content changed, so the update
	// path runs) must re-stamp index_run_id to the new run.
	runID2, err := s.RecordIndexRun("pkg", "rev2", "go", "gopls", "v1", "test-model", time.Now())
	if err != nil {
		t.Fatalf("second RecordIndexRun: %v", err)
	}
	k1Doc := k1
	k1Doc.Documentation = "now documented"
	k1Doc.EmbeddingText = codeindex.RenderEmbeddingText(k1Doc)
	fe.register(k1Doc, []float32{0, 1, 0})
	if _, err := s.UpsertSymbols("pkg/file.go", "filehash-2", []codeindex.Symbol{k1Doc}, runID2, fe.embed); err != nil {
		t.Fatalf("update upsert: %v", err)
	}
	if err := s.db.QueryRow(`SELECT index_run_id FROM symbols WHERE key='k1'`).Scan(&gotRunID); err != nil {
		t.Fatal(err)
	}
	if gotRunID != runID2 {
		t.Fatalf("index_run_id after update = %d, want %d", gotRunID, runID2)
	}
}

// --- SymbolAt: (path, line) -> enclosing indexed symbol's key, the
// resolution primitive relations.go's Resolver needs ---

func TestSymbolAtResolvesEnclosingSymbol(t *testing.T) {
	s := openTestStore(t, 3)
	fe := newFakeEmbedder(t)

	k1 := sym("k1", "ParseConfig", "ParseConfig", "", "func ParseConfig() {}")
	k1.Range = codeindex.Range{Start: codeindex.Position{Line: 10}, End: codeindex.Position{Line: 20}}
	fe.register(k1, []float32{1, 0, 0})
	if _, err := s.UpsertSymbols("pkg/file.go", "filehash-1", []codeindex.Symbol{k1}, 0, fe.embed); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	key, ok, err := s.SymbolAt("pkg/file.go", 15)
	if err != nil {
		t.Fatalf("SymbolAt(inside range): %v", err)
	}
	if !ok || key != "k1" {
		t.Fatalf("SymbolAt(pkg/file.go, 15) = (%q, %v), want (k1, true)", key, ok)
	}

	// Boundary lines are inclusive.
	if _, ok, err := s.SymbolAt("pkg/file.go", 10); err != nil || !ok {
		t.Fatalf("SymbolAt(start line): ok=%v err=%v, want ok=true", ok, err)
	}
	if _, ok, err := s.SymbolAt("pkg/file.go", 20); err != nil || !ok {
		t.Fatalf("SymbolAt(end line): ok=%v err=%v, want ok=true", ok, err)
	}

	if _, ok, err := s.SymbolAt("pkg/file.go", 25); err != nil || ok {
		t.Fatalf("SymbolAt(outside range): ok=%v err=%v, want ok=false", ok, err)
	}
	if _, ok, err := s.SymbolAt("pkg/other.go", 15); err != nil || ok {
		t.Fatalf("SymbolAt(wrong path): ok=%v err=%v, want ok=false", ok, err)
	}
}

// --- ListPaths / DeleteSymbolsForPath: the store-side primitives `code
// index`'s default pruning pass needs to remove symbols for files no longer
// discovered under an indexed root ---

func TestListPathsAndDeleteSymbolsForPath(t *testing.T) {
	s := openTestStore(t, 3)
	fe := newFakeEmbedder(t)

	a := sym("k1", "ParseConfig", "ParseConfig", "", "func ParseConfig() {}")
	a.Path = "pkg/a.go"
	a.EmbeddingText = codeindex.RenderEmbeddingText(a)
	fe.register(a, []float32{1, 0, 0})
	if _, err := s.UpsertSymbols(a.Path, "hash-a", []codeindex.Symbol{a}, 0, fe.embed); err != nil {
		t.Fatalf("upsert a: %v", err)
	}

	b := sym("k2", "RunServer", "RunServer", "", "func RunServer() {}")
	b.Path = "pkg/b.go"
	b.EmbeddingText = codeindex.RenderEmbeddingText(b)
	fe.register(b, []float32{0, 1, 0})
	if _, err := s.UpsertSymbols(b.Path, "hash-b", []codeindex.Symbol{b}, 0, fe.embed); err != nil {
		t.Fatalf("upsert b: %v", err)
	}

	paths, err := s.ListPaths()
	if err != nil {
		t.Fatalf("ListPaths: %v", err)
	}
	sort.Strings(paths)
	if !reflect.DeepEqual(paths, []string{"pkg/a.go", "pkg/b.go"}) {
		t.Fatalf("ListPaths() = %v, want [pkg/a.go pkg/b.go]", paths)
	}

	var k1ID int64
	if err := s.db.QueryRow(`SELECT id FROM symbols WHERE key='k1'`).Scan(&k1ID); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteSymbolsForPath("pkg/a.go"); err != nil {
		t.Fatalf("DeleteSymbolsForPath: %v", err)
	}

	if _, err := s.GetSymbol("k1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetSymbol(k1) after DeleteSymbolsForPath: err=%v, want ErrNotFound", err)
	}
	if _, err := s.GetSymbol("k2"); err != nil {
		t.Fatalf("GetSymbol(k2), a different path, must be untouched: %v", err)
	}

	paths2, err := s.ListPaths()
	if err != nil {
		t.Fatal(err)
	}
	if len(paths2) != 1 || paths2[0] != "pkg/b.go" {
		t.Fatalf("ListPaths() after delete = %v, want [pkg/b.go]", paths2)
	}

	// FTS and vec rows must also be gone, not just the symbols row.
	hits, err := s.SearchSymbolsText("ParseConfig", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("FTS hit for a pruned symbol still present: %v", hits)
	}
	var vecCount int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM symbol_vec WHERE rowid=?`, k1ID).Scan(&vecCount); err != nil {
		t.Fatal(err)
	}
	if vecCount != 0 {
		t.Fatalf("symbol_vec row for pruned symbol still present: count=%d", vecCount)
	}

	// A path with no stored symbols is a no-op, not an error.
	if err := s.DeleteSymbolsForPath("pkg/never-indexed.go"); err != nil {
		t.Fatalf("DeleteSymbolsForPath on an unindexed path: %v", err)
	}
}

// --- LatestIndexRun: the read counterpart to RecordIndexRun, for a caller
// (cmd/ragrep's `code pack`) that needs the index's current identity
// without tracking a specific run id itself ---

func TestLatestIndexRunReturnsErrNotFoundWhenEmpty(t *testing.T) {
	s := openTestStore(t, 3)
	_, err := s.LatestIndexRun()
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("LatestIndexRun on empty index_runs: err=%v, want ErrNotFound", err)
	}
}

func TestLatestIndexRunReturnsMostRecentRow(t *testing.T) {
	s := openTestStore(t, 3)

	when := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	if _, err := s.RecordIndexRun("index:pkg1", "rev1", "go", "gopls", "v1.0.0", "test-model", when); err != nil {
		t.Fatalf("first RecordIndexRun: %v", err)
	}
	id2, err := s.RecordIndexRun("index:pkg2", "rev2", "go", "gopls", "v2.0.0", "test-model", when.Add(time.Hour))
	if err != nil {
		t.Fatalf("second RecordIndexRun: %v", err)
	}

	run, err := s.LatestIndexRun()
	if err != nil {
		t.Fatalf("LatestIndexRun: %v", err)
	}
	if run.ID != id2 || run.Scope != "index:pkg2" || run.Revision != "rev2" || run.ServerVersion != "v2.0.0" {
		t.Fatalf("LatestIndexRun = %+v, want the most recently recorded run (id=%d, scope=index:pkg2, rev2, v2.0.0)", run, id2)
	}
}

// A `code expand` call records its own index_runs row (see cmd/ragrep's
// cmdCodeExpand) with scope "expand:...", not "index:...", so it must never
// win LatestIndexRun even though it has a higher id than the last real
// index run -- otherwise a manifest could advertise a revision at which
// symbols were never actually (re)indexed.
func TestLatestIndexRunIgnoresNonIndexScopedRuns(t *testing.T) {
	s := openTestStore(t, 3)

	when := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	indexID, err := s.RecordIndexRun("index:pkg1", "rev1", "go", "gopls", "v1.0.0", "test-model", when)
	if err != nil {
		t.Fatalf("index-scoped RecordIndexRun: %v", err)
	}
	if _, err := s.RecordIndexRun("expand:references:somekey", "rev1", "go", "gopls", "v1.0.0", "test-model", when.Add(time.Hour)); err != nil {
		t.Fatalf("expand-scoped RecordIndexRun: %v", err)
	}

	run, err := s.LatestIndexRun()
	if err != nil {
		t.Fatalf("LatestIndexRun: %v", err)
	}
	if run.ID != indexID || run.Scope != "index:pkg1" {
		t.Fatalf("LatestIndexRun = %+v, want the index-scoped run (id=%d, scope=index:pkg1), not the later expand-scoped one", run, indexID)
	}
}

// --- FindByQualifiedName: coderetrieval.SymbolFinder's store-backed
// implementation, for re-resolving a manifest entry whose stable key no
// longer exists ---

func TestFindByQualifiedNameMatchesOnQualifiedNameAndPath(t *testing.T) {
	s := openTestStore(t, 3)
	fe := newFakeEmbedder(t)

	k1 := sym("k1", "ParseConfig", "ParseConfig", "", "func ParseConfig() {}")
	fe.register(k1, []float32{1, 0, 0})
	if _, err := s.UpsertSymbols("pkg/file.go", "filehash-1", []codeindex.Symbol{k1}, 0, fe.embed); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	matches, err := s.FindByQualifiedName("ParseConfig", "pkg/file.go")
	if err != nil {
		t.Fatalf("FindByQualifiedName: %v", err)
	}
	if len(matches) != 1 || matches[0].Key != "k1" {
		t.Fatalf("FindByQualifiedName(ParseConfig, pkg/file.go) = %+v, want single match k1", matches)
	}

	// Same qualified name, different path: must not match.
	none, err := s.FindByQualifiedName("ParseConfig", "other/file.go")
	if err != nil {
		t.Fatalf("FindByQualifiedName (wrong path): %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("FindByQualifiedName with wrong path = %+v, want no matches", none)
	}
}

// --- RelationsFrom: read counterpart to ReplaceRelations, satisfying
// coderetrieval.RelationGetter for `code pack`'s 1-hop relation staging ---

func TestRelationsFromReturnsOnlyFromKeysEdges(t *testing.T) {
	s := openTestStore(t, 3)
	fe := newFakeEmbedder(t)

	k1 := sym("k1", "Target", "Target", "", "func Target() {}")
	k2 := sym("k2", "Other", "Other", "", "func Other() {}")
	fe.register(k1, []float32{1, 0, 0})
	fe.register(k2, []float32{0, 1, 0})
	if _, err := s.UpsertSymbols("pkg/file.go", "filehash-1", []codeindex.Symbol{k1, k2}, 0, fe.embed); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	if err := s.ReplaceRelations(1, "k1", []string{"calls"}, []codeindex.Relation{
		{FromKey: "k1", ToKey: "k2", Kind: "calls", Source: "static"},
	}); err != nil {
		t.Fatalf("ReplaceRelations: %v", err)
	}
	if err := s.ReplaceRelations(1, "k2", []string{"calls"}, []codeindex.Relation{
		{FromKey: "k2", ToKey: "k1", Kind: "calls", Source: "static"},
	}); err != nil {
		t.Fatalf("ReplaceRelations (k2): %v", err)
	}

	got, err := s.RelationsFrom("k1")
	if err != nil {
		t.Fatalf("RelationsFrom: %v", err)
	}
	if len(got) != 1 || got[0].ToKey != "k2" || got[0].Kind != "calls" {
		t.Fatalf("RelationsFrom(k1) = %+v, want one relation k1->k2", got)
	}

	none, err := s.RelationsFrom("does-not-exist")
	if err != nil {
		t.Fatalf("RelationsFrom (missing key): %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("RelationsFrom(does-not-exist) = %+v, want empty", none)
	}
}

// --- SymbolFileHash: the file_hash recorded for a symbol's row, for
// building a coderetrieval.SymbolRef without re-reading the file from disk
// ---

func TestSymbolFileHashReturnsStoredHash(t *testing.T) {
	s := openTestStore(t, 3)
	fe := newFakeEmbedder(t)

	k1 := sym("k1", "ParseConfig", "ParseConfig", "", "func ParseConfig() {}")
	fe.register(k1, []float32{1, 0, 0})
	if _, err := s.UpsertSymbols("pkg/file.go", "filehash-xyz", []codeindex.Symbol{k1}, 0, fe.embed); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	hash, err := s.SymbolFileHash("k1")
	if err != nil {
		t.Fatalf("SymbolFileHash: %v", err)
	}
	if hash != "filehash-xyz" {
		t.Fatalf("SymbolFileHash(k1) = %q, want %q", hash, "filehash-xyz")
	}
}

func TestSymbolFileHashNotFound(t *testing.T) {
	s := openTestStore(t, 3)
	_, err := s.SymbolFileHash("does-not-exist")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("SymbolFileHash on missing key: err=%v, want ErrNotFound", err)
	}
}

// SymbolAt must pick the innermost (smallest-span) enclosing symbol when
// ranges overlap, not just any match -- otherwise a reference inside a
// method would resolve to a loosely-overlapping outer symbol instead of the
// method itself.
func TestSymbolAtPicksInnermostOnOverlap(t *testing.T) {
	s := openTestStore(t, 3)
	fe := newFakeEmbedder(t)

	outer := sym("outer", "Container", "Container", "", "type Container struct{}")
	outer.Range = codeindex.Range{Start: codeindex.Position{Line: 1}, End: codeindex.Position{Line: 100}}
	inner := sym("inner", "Method", "Container.Method", "", "func (c Container) Method() {}")
	inner.Range = codeindex.Range{Start: codeindex.Position{Line: 40}, End: codeindex.Position{Line: 50}}
	fe.register(outer, []float32{1, 0, 0})
	fe.register(inner, []float32{0, 1, 0})

	if _, err := s.UpsertSymbols("pkg/file.go", "filehash-1", []codeindex.Symbol{outer, inner}, 0, fe.embed); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	key, ok, err := s.SymbolAt("pkg/file.go", 45)
	if err != nil {
		t.Fatalf("SymbolAt: %v", err)
	}
	if !ok || key != "inner" {
		t.Fatalf("SymbolAt(45) = (%q, %v), want (inner, true)", key, ok)
	}

	// A line only the outer symbol covers must still resolve to it.
	key, ok, err = s.SymbolAt("pkg/file.go", 5)
	if err != nil {
		t.Fatalf("SymbolAt: %v", err)
	}
	if !ok || key != "outer" {
		t.Fatalf("SymbolAt(5) = (%q, %v), want (outer, true)", key, ok)
	}
}

func TestFtsQuery(t *testing.T) {
	cases := []struct{ in, want string }{
		{`parse config`, `"parse" OR "config"`},
		{`hello`, `"hello"`},
		{`say "hi"`, `"say" OR """hi"""`},
		{``, `""`},
		{`   `, `""`},
	}
	for _, c := range cases {
		if got := ftsQuery(c.in); got != c.want {
			t.Errorf("ftsQuery(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

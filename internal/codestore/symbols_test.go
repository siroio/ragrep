package codestore

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

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

	changed, err := s.UpsertSymbols("pkg/file.go", "filehash-1", []codeindex.Symbol{s1}, fe.embed)
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
	changed, err = s.UpsertSymbols("pkg/file.go", "filehash-1", []codeindex.Symbol{s1}, fe.embed)
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

	if _, err := s.UpsertSymbols("pkg/file.go", "filehash-1", []codeindex.Symbol{k1, k2}, fe.embed); err != nil {
		t.Fatalf("initial upsert: %v", err)
	}

	var k1ID int64
	if err := s.db.QueryRow(`SELECT id FROM symbols WHERE key='k1'`).Scan(&k1ID); err != nil {
		t.Fatalf("lookup k1 id: %v", err)
	}

	if err := s.ReplaceRelations(1, []codeindex.Relation{
		{FromKey: "k1", ToKey: "k2", Kind: "calls", Source: "static"},
	}); err != nil {
		t.Fatalf("ReplaceRelations: %v", err)
	}

	// Re-index the file with k1 removed (only k2 remains).
	changed, err := s.UpsertSymbols("pkg/file.go", "filehash-2", []codeindex.Symbol{k2}, fe.embed)
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
	if _, err := s.UpsertSymbols("pkg/file.go", "filehash-1", []codeindex.Symbol{k1}, fe.embed); err != nil {
		t.Fatalf("initial upsert: %v", err)
	}
	if fe.calls != 1 {
		t.Fatalf("calls after initial insert = %d, want 1", fe.calls)
	}

	// Re-upsert with a new file hash, but the symbol content (doc + body)
	// unchanged: must not re-embed.
	changed, err := s.UpsertSymbols("pkg/file.go", "filehash-2", []codeindex.Symbol{k1}, fe.embed)
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
	if _, err := s.UpsertSymbols("pkg/file.go", "filehash-3", []codeindex.Symbol{k1Doc}, fe.embed); err != nil {
		t.Fatalf("doc-changed re-upsert: %v", err)
	}
	if fe.calls != 2 {
		t.Fatalf("calls after doc-changed re-upsert = %d, want 2", fe.calls)
	}
}

func TestSearchSymbolsHybridExactMatchBeatsSemanticSimilarity(t *testing.T) {
	s := openTestStore(t, 3)
	fe := newFakeEmbedder(t)

	// exact: name matches the query text exactly, but its embedding is far
	// from the query vector (so vector search alone ranks it last).
	exact := sym("exact-key", "ParseConfig", "ParseConfig", "", "func ParseConfig() {}")
	fe.register(exact, []float32{0, 0, 1})

	// semantic: not an exact name match, but its embedding is exactly the
	// query vector (so vector search alone ranks it first) and it also
	// shares no useful FTS tokens with the query.
	semantic := sym("semantic-key", "LoadSettings", "ConfigLoader.LoadSettings", "", "func LoadSettings() {}")
	fe.register(semantic, []float32{1, 0, 0})

	if _, err := s.UpsertSymbols("pkg/file.go", "filehash-1", []codeindex.Symbol{exact, semantic}, fe.embed); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	queryVector := []float32{1, 0, 0} // == semantic's embedding
	hits, err := s.SearchSymbolsHybrid("ParseConfig", queryVector, 10)
	if err != nil {
		t.Fatalf("SearchSymbolsHybrid: %v", err)
	}
	if len(hits) < 2 {
		t.Fatalf("got %d hits, want >= 2", len(hits))
	}
	if hits[0].Key != "exact-key" {
		t.Errorf("hits[0].Key = %q, want exact-key (exact identifier match must be pinned above semantic similarity)", hits[0].Key)
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

	if _, err := s.UpsertSymbols("pkg/file.go", "filehash-1", []codeindex.Symbol{near, far}, fe.embed); err != nil {
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
	if _, err := s.UpsertSymbols("pkg/file.go", "filehash-1", []codeindex.Symbol{k1}, fe.embed); err != nil {
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

func TestReplaceRelationsReplacesOnlyMatchingFromKeys(t *testing.T) {
	s := openTestStore(t, 3)
	fe := newFakeEmbedder(t)

	k1 := sym("k1", "CallerOne", "CallerOne", "", "func CallerOne() {}")
	k2 := sym("k2", "CallerTwo", "CallerTwo", "", "func CallerTwo() {}")
	fe.register(k1, []float32{1, 0, 0})
	fe.register(k2, []float32{0, 1, 0})
	if _, err := s.UpsertSymbols("pkg/file.go", "filehash-1", []codeindex.Symbol{k1, k2}, fe.embed); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	if err := s.ReplaceRelations(1, []codeindex.Relation{
		{FromKey: "k1", ToKey: "k2", Kind: "calls", Source: "static"},
		{FromKey: "k2", ToKey: "k1", Kind: "calls", Source: "static"},
	}); err != nil {
		t.Fatalf("first ReplaceRelations: %v", err)
	}

	// Replace only k1's edges with a different target; k2's edge (from_key
	// not mentioned here) must survive untouched.
	if err := s.ReplaceRelations(2, []codeindex.Relation{
		{FromKey: "k1", ToKey: "k2", Kind: "imports", Source: "static"},
	}); err != nil {
		t.Fatalf("second ReplaceRelations: %v", err)
	}

	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM symbol_edges WHERE from_key='k1' AND kind='calls'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("old k1 'calls' edge still present after replace")
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM symbol_edges WHERE from_key='k1' AND kind='imports'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("new k1 'imports' edge missing")
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM symbol_edges WHERE from_key='k2'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("untouched k2 edge was removed, want it to survive (from_key not mentioned in second call)")
	}
}

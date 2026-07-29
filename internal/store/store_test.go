package store

import (
	"path/filepath"
	"testing"
)

// fakeEmbed returns a fixed-dimension deterministic vector (no ONNX needed).
func fakeEmbed(text string) ([]float32, error) {
	v := make([]float32, embedDim)
	for i, r := range text {
		v[i%embedDim] += float32(r % 13)
	}
	return v, nil
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestUpsertAndTextSearch(t *testing.T) {
	s := newTestStore(t)
	content := "認証エラーの一覧。\nERR_AUTH_104 はトークン期限切れ。\n\nネットワーク設定について。"
	changed, err := s.UpsertDoc("docs/auth.md", content, 100, fakeEmbed)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}

	// Same content again -> no change
	changed, err = s.UpsertDoc("docs/auth.md", content, 200, fakeEmbed)
	if err != nil || changed {
		t.Fatalf("re-upsert: changed=%v err=%v", changed, err)
	}

	hits, err := s.SearchText("ERR_AUTH_104", 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Doc != "docs/auth.md" || hits[0].Para != 0 || hits[0].Lines != "1-2" {
		t.Fatalf("unexpected hits: %+v", hits)
	}

	// Updated content replaces old paragraphs
	_, err = s.UpsertDoc("docs/auth.md", "全部書き換えた。", 300, fakeEmbed)
	if err != nil {
		t.Fatal(err)
	}
	hits, _ = s.SearchText("ERR_AUTH_104", 10, nil)
	if len(hits) != 0 {
		t.Fatalf("stale hits after update: %+v", hits)
	}
}

func TestRRFMerge(t *testing.T) {
	ids, scores := rrfMerge([][]int64{
		{1, 2, 3}, // text ranking
		{3, 1},    // vector ranking
	})
	// id1: 1/61 + 1/62 ≈ 0.03252, id3: 1/63 + 1/61 ≈ 0.03227, id2: 1/62 ≈ 0.01613
	want := []int64{1, 3, 2}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("order: got %v, want %v (scores=%v)", ids, want, scores)
		}
	}
	if scores[2] >= scores[3] {
		t.Fatalf("scores not descending: %v", scores)
	}
}

func TestSearchVectorAndHybrid(t *testing.T) {
	s := newTestStore(t)
	// Two docs with distinct content; fakeEmbed is deterministic so the
	// same text always maps to the same vector.
	if _, err := s.UpsertDoc("a.txt", "りんごは赤い果物です。", 1, fakeEmbed); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertDoc("b.txt", "会計システムの締め処理について。", 1, fakeEmbed); err != nil {
		t.Fatal(err)
	}

	qv, _ := fakeEmbed("title: none | text: りんごは赤い果物です。") // identical vector -> distance 0
	hits, err := s.SearchVector(qv, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 || hits[0].Doc != "a.txt" {
		t.Fatalf("vector hits: %+v", hits)
	}
	if hits[0].Score <= hits[1].Score {
		t.Fatalf("scores not descending: %+v", hits)
	}

	hy, err := s.SearchHybrid("りんご", qv, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(hy) == 0 || hy[0].Doc != "a.txt" {
		t.Fatalf("hybrid hits: %+v", hy)
	}
}

func TestGet(t *testing.T) {
	s := newTestStore(t)
	content := "p0\n\np1\n\np2\n\np3"
	if _, err := s.UpsertDoc("a.txt", content, 1, fakeEmbed); err != nil {
		t.Fatal(err)
	}

	doc, err := s.GetDoc("a.txt")
	if err != nil || doc != content {
		t.Fatalf("GetDoc: %q err=%v", doc, err)
	}
	if _, err := s.GetDoc("missing.txt"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}

	got, err := s.GetParas("a.txt", 1, 0)
	if err != nil || got != "p1" {
		t.Fatalf("GetParas(1,0)=%q err=%v", got, err)
	}
	// context expansion: ±1 paragraph, joined with blank line
	got, err = s.GetParas("a.txt", 1, 1)
	if err != nil || got != "p0\n\np1\n\np2" {
		t.Fatalf("GetParas(1,1)=%q err=%v", got, err)
	}
	// context clamps at document edges
	got, err = s.GetParas("a.txt", 0, 5)
	if err != nil || got != "p0\n\np1\n\np2\n\np3" {
		t.Fatalf("GetParas(0,5)=%q err=%v", got, err)
	}
	if _, err := s.GetParas("a.txt", 99, 0); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestFirstPath(t *testing.T) {
	s := newTestStore(t)
	p, err := s.FirstPath()
	if err != nil || p != "" {
		t.Fatalf("empty store: got %q err=%v, want \"\",nil", p, err)
	}

	if _, err := s.UpsertDoc("docs/a.md", "content", 1, fakeEmbed); err != nil {
		t.Fatal(err)
	}
	p, err = s.FirstPath()
	if err != nil || p != "docs/a.md" {
		t.Fatalf("got %q err=%v, want docs/a.md", p, err)
	}
}

func TestDeleteDoc(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.UpsertDoc("a.txt", "alpha content UNIQUEAAA111", 1, fakeEmbed); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertDoc("b.txt", "beta content UNIQUEBBB222", 1, fakeEmbed); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteDoc("a.txt"); err != nil {
		t.Fatal(err)
	}

	hits, err := s.SearchText("UNIQUEAAA111", 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("stale hits after delete: %+v", hits)
	}
	if _, err := s.GetDoc("a.txt"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if doc, err := s.GetDoc("b.txt"); err != nil || doc != "beta content UNIQUEBBB222" {
		t.Fatalf("other doc affected: %q err=%v", doc, err)
	}

	if err := s.DeleteDoc("missing.txt"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestSearchTextMultiWordOutOfOrder(t *testing.T) {
	s := newTestStore(t)
	content := "the config file is parsed at startup"
	if _, err := s.UpsertDoc("a.txt", content, 1, fakeEmbed); err != nil {
		t.Fatal(err)
	}

	hits, err := s.SearchText("parse config", 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Doc != "a.txt" {
		t.Fatalf("unexpected hits: %+v", hits)
	}
}

func TestSearchHitHeading(t *testing.T) {
	s := newTestStore(t)
	content := "# Auth\n\n## Errors\n\nretry with backoff"
	if _, err := s.UpsertDoc("docs/auth.md", content, 1, fakeEmbed); err != nil {
		t.Fatal(err)
	}

	hits, err := s.SearchText("retry", 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Heading != "Auth > Errors" {
		t.Fatalf("unexpected hits: %+v", hits)
	}
}

func TestSearchHitMtime(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.UpsertDoc("docs/auth.md", "retry with backoff", 1234, fakeEmbed); err != nil {
		t.Fatal(err)
	}

	hits, err := s.SearchText("retry", 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Mtime != 1234 {
		t.Fatalf("unexpected hits: %+v", hits)
	}
}

// DocHash exposes the stored content hash so callers (the converter index
// path) can skip re-conversion when the source file hasn't changed, without
// re-embedding.
func TestDocHash(t *testing.T) {
	s := newTestStore(t)

	if h, err := s.DocHash("missing.txt"); err != nil || h != "" {
		t.Fatalf("DocHash(missing)=%q err=%v, want \"\",nil", h, err)
	}

	changed, err := s.UpsertDocWithHash("a.txt", "body", 1, "h123", fakeEmbed)
	if err != nil || !changed {
		t.Fatalf("UpsertDocWithHash: changed=%v err=%v", changed, err)
	}
	if h, err := s.DocHash("a.txt"); err != nil || h != "h123" {
		t.Fatalf("DocHash(a.txt)=%q err=%v, want h123,nil", h, err)
	}

	// Re-upsert with the same caller-supplied hash: no re-embed, reported as
	// unchanged even though the content string itself differs.
	changed, err = s.UpsertDocWithHash("a.txt", "different body", 2, "h123", fakeEmbed)
	if err != nil || changed {
		t.Fatalf("re-upsert same hash: changed=%v err=%v", changed, err)
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

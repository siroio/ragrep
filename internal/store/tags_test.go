package store

import (
	"reflect"
	"strings"
	"testing"
)

func docSet(hits []Hit) map[string]bool {
	m := map[string]bool{}
	for _, h := range hits {
		m[h.Doc] = true
	}
	return m
}

func TestSearchTagFilter(t *testing.T) {
	s := newTestStore(t)
	docs := map[string]string{
		"d1.md": "---\ntags: [design, api]\n---\n\n設計の話 keyword です。",
		"d2.md": "---\ntags: [design]\n---\n\n別の設計 keyword です。",
		"d3.md": "タグなしの keyword です。",
	}
	for p, c := range docs {
		if _, err := s.UpsertDoc(p, c, 1, fakeEmbed); err != nil {
			t.Fatal(err)
		}
	}

	// no filter: all three
	hits, err := s.SearchText("keyword", 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 3 {
		t.Fatalf("unfiltered: %+v", hits)
	}

	// single tag
	hits, err = s.SearchText("keyword", 10, []string{"design"})
	if err != nil {
		t.Fatal(err)
	}
	got := docSet(hits)
	if len(hits) != 2 || !got["d1.md"] || !got["d2.md"] {
		t.Fatalf("tag=design: %+v", hits)
	}

	// multiple tags = AND
	hits, err = s.SearchText("keyword", 10, []string{"design", "api"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Doc != "d1.md" {
		t.Fatalf("tag=design AND api: %+v", hits)
	}

	// tag matching is case-insensitive on the query side
	hits, err = s.SearchText("keyword", 10, []string{"Design", "API"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Doc != "d1.md" {
		t.Fatalf("tag=Design AND API: %+v", hits)
	}

	// unknown tag: no hits
	hits, err = s.SearchText("keyword", 10, []string{"missing"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("tag=missing: %+v", hits)
	}

	// vector mode honors the filter
	qv, _ := fakeEmbed("title: none | text: 設計の話 keyword です。")
	vhits, err := s.SearchVector(qv, 10, []string{"api"})
	if err != nil {
		t.Fatal(err)
	}
	if len(vhits) != 1 || vhits[0].Doc != "d1.md" {
		t.Fatalf("vector tag=api: %+v", vhits)
	}

	// hybrid mode honors the filter
	hhits, err := s.SearchHybrid("keyword", qv, 10, []string{"api"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hhits) != 1 || hhits[0].Doc != "d1.md" {
		t.Fatalf("hybrid tag=api: %+v", hhits)
	}

	// re-upsert without tags replaces doc_tags
	if _, err := s.UpsertDoc("d2.md", "タグを消した keyword です。", 2, fakeEmbed); err != nil {
		t.Fatal(err)
	}
	hits, err = s.SearchText("keyword", 10, []string{"design"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Doc != "d1.md" {
		t.Fatalf("after tag removal: %+v", hits)
	}
}

// TestDeleteDocPurgesTags reproduces rowid-reuse tag inheritance: documents.id
// is INTEGER PRIMARY KEY without AUTOINCREMENT, so SQLite reuses a deleted
// row's id for the next insert. If DeleteDoc doesn't purge doc_tags, a new
// untagged document can silently inherit the deleted document's tags.
func TestDeleteDocPurgesTags(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.UpsertDoc("d1.md", "---\ntags: [design]\n---\n\n最初の keyword 文書。", 1, fakeEmbed); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteDoc("d1.md"); err != nil {
		t.Fatal(err)
	}
	// d2.md is the only document left, so it reuses id=1 (the id DeleteDoc
	// just freed) and must not inherit d1.md's "design" tag.
	if _, err := s.UpsertDoc("d2.md", "タグなしの keyword 文書。", 1, fakeEmbed); err != nil {
		t.Fatal(err)
	}
	hits, err := s.SearchText("keyword", 10, []string{"design"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("tag=design should not match untagged d2.md via inherited rowid tags: %+v", hits)
	}
}

// TestFrontmatterSeqRenumbered checks that skipping the frontmatter block
// during paragraph indexing still leaves the first indexed content
// paragraph at Seq/Para 0, matching the 0-based-contiguous invariant every
// other document shape has.
func TestFrontmatterSeqRenumbered(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.UpsertDoc("fm.md", "---\ntags: [x]\n---\n\n本文パラグラフ。", 1, fakeEmbed); err != nil {
		t.Fatal(err)
	}
	hits, err := s.SearchText("パラグラフ", 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Para != 0 {
		t.Fatalf("expected Para=0 for the first indexed content paragraph, got %+v", hits)
	}
	got, err := s.GetParas("fm.md", 0, 0)
	if err != nil || got != "本文パラグラフ。" {
		t.Fatalf("GetParas(0,0)=%q err=%v", got, err)
	}
}

// TestAutoTags checks autoTags derives tags from directory segments and
// file extension, lowercased.
func TestAutoTags(t *testing.T) {
	cases := []struct {
		path string
		want []string
	}{
		{"docs/design/foo.pdf", []string{"docs", "design", "pdf"}},
		{"README.md", []string{"md"}},
		{"Docs/API.md", []string{"docs", "md"}}, // lowercased
		{"noext", nil},
		{".hidden", nil}, // leading dot is not an extension
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			got := autoTags(c.path)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("autoTags(%q) = %v, want %v", c.path, got, c.want)
			}
		})
	}
}

// TestUpsertDocMergesAutoTags checks the tag-write site in
// UpsertDocWithHash merges frontmatter tags with path/extension-derived
// auto tags, deduped.
func TestUpsertDocMergesAutoTags(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.UpsertDoc("docs/x.md", "---\ntags: [api]\n---\n\nbody keyword here.", 1, fakeEmbed); err != nil {
		t.Fatal(err)
	}

	for _, tags := range [][]string{{"api"}, {"docs"}, {"md"}, {"md", "api"}} {
		hits, err := s.SearchText("keyword", 10, tags)
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 1 || hits[0].Doc != "docs/x.md" {
			t.Fatalf("tag=%v: %+v", tags, hits)
		}
	}
}

// TestFrontmatterNoBlankLineExcluded reproduces frontmatter fusing with the
// first body paragraph when there's no blank line after the closing "---":
// splitParas would otherwise treat frontmatter+body as a single paragraph
// (the `p.EndLine <= fmLines` skip never fires because EndLine extends past
// fmLines), leaking "tags:" into FTS/embeddings/snippets.
func TestFrontmatterNoBlankLineExcluded(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.UpsertDoc("nb.md", "---\ntags: [x]\n---\n本文 keyword。", 1, fakeEmbed); err != nil {
		t.Fatal(err)
	}
	hits, err := s.SearchText("keyword", 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Para != 0 {
		t.Fatalf("expected exactly 1 hit at Para 0, got %+v", hits)
	}
	if strings.Contains(hits[0].Snippet, "tags:") {
		t.Fatalf("frontmatter leaked into snippet: %q", hits[0].Snippet)
	}
}

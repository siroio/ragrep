package store

import "testing"

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

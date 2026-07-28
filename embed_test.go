package main

import (
	"math"
	"os"
	"runtime"
	"testing"
)

// Requires cached assets; run `rag init` (or ensureAssets) once beforehand.
func testEmbedder(t *testing.T) *Embedder {
	t.Helper()
	dir, err := cacheDir()
	if err != nil {
		t.Fatal(err)
	}
	asset, err := ortAssetFor(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	if f := missingAsset(dir, asset); f != "" {
		t.Skipf("%s not cached; run 'rag init' to enable this test", f)
	}
	e, err := newEmbedder(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	return e
}

func TestEmbedProperties(t *testing.T) {
	e := testEmbedder(t)
	v, err := e.Embed("title: none | text: 認証エラーの一覧")
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 768 {
		t.Fatalf("dim=%d", len(v))
	}
	var norm float64
	for _, x := range v {
		norm += float64(x) * float64(x)
	}
	if math.Abs(norm-1.0) > 1e-3 {
		t.Fatalf("not L2-normalized: %f", norm)
	}
}

func TestEmbedSimilarityOrdering(t *testing.T) {
	e := testEmbedder(t)
	q, _ := e.Embed("task: search result | query: 認証が失敗する原因")
	rel, _ := e.Embed("title: none | text: 認証エラーはトークンの期限切れで発生する")
	irrel, _ := e.Embed("title: none | text: 今日の東京の天気は晴れです")
	cos := func(a, b []float32) float64 {
		var s float64
		for i := range a {
			s += float64(a[i]) * float64(b[i])
		}
		return s
	}
	if cos(q, rel) <= cos(q, irrel) {
		t.Fatalf("similarity ordering wrong: rel=%f irrel=%f", cos(q, rel), cos(q, irrel))
	}
}

// TestEnsureAssets downloads assets when RAG_DOWNLOAD=1 is set.
// This doubles as the download path's integration test.
func TestEnsureAssets(t *testing.T) {
	if os.Getenv("RAG_DOWNLOAD") != "1" {
		t.Skip("set RAG_DOWNLOAD=1 to download ~310MB of model assets")
	}
	dir, err := cacheDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureAssets(dir); err != nil {
		t.Fatal(err)
	}
}

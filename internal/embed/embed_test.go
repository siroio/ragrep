package embed

import (
	"math"
	"os"
	"runtime"
	"testing"
)

// Requires cached assets; run `ragrep init` (or ensureAssets) once beforehand.
func testEmbedder(t *testing.T) *Embedder {
	t.Helper()
	dir, err := CacheDir()
	if err != nil {
		t.Fatal(err)
	}
	assets, err := ortAssetsFor(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	if f := missingAsset(dir, assets); f != "" {
		t.Skipf("%s not cached; run 'ragrep init' to enable this test", f)
	}
	e, err := New(dir)
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
	// Inverted comparison so NaN (always-false comparisons) fails too: an
	// fp16 overflow on the GPU path once turned every dimension NaN and the
	// old `> 1e-3` check passed vacuously.
	if !(math.Abs(norm-1.0) < 1e-3) {
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
	// Inverted comparison so NaN embeddings fail instead of passing vacuously.
	if !(cos(q, rel) > cos(q, irrel)) {
		t.Fatalf("similarity ordering wrong: rel=%f irrel=%f", cos(q, rel), cos(q, irrel))
	}
}

func TestOrtAssetsFor(t *testing.T) {
	win, err := ortAssetsFor("windows", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if len(win) != 2 || win[0].lib != "onnxruntime.dll" || win[1].lib != "DirectML.dll" {
		t.Fatalf("windows assets = %+v", win)
	}
	if want := "runtimes/win-x64/native/onnxruntime.dll"; win[0].inner != want {
		t.Fatalf("inner = %q, want %q", win[0].inner, want)
	}
	linux, err := ortAssetsFor("linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if len(linux) != 1 || linux[0].lib != "libonnxruntime.so" {
		t.Fatalf("linux assets = %+v", linux)
	}
	mac, err := ortAssetsFor("darwin", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	if len(mac) != 1 || mac[0].lib != "libonnxruntime.dylib" {
		t.Fatalf("darwin assets = %+v", mac)
	}
	if _, err := ortAssetsFor("plan9", "386"); err == nil {
		t.Fatal("expected error for unsupported platform")
	}
}

// TestEnsureAssets downloads assets when RAG_DOWNLOAD=1 is set.
// This doubles as the download path's integration test.
func TestEnsureAssets(t *testing.T) {
	if os.Getenv("RAG_DOWNLOAD") != "1" {
		t.Skip("set RAG_DOWNLOAD=1 to download model assets (~1.4GB on Windows/fp32, ~310MB elsewhere)")
	}
	dir, err := CacheDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureAssets(dir); err != nil {
		t.Fatal(err)
	}
}

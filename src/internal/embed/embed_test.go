package embed

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func fixtureZip(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	f, err := zw.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestExtractOrtLibRepairsCorruptCachedLibrary(t *testing.T) {
	body := []byte("valid runtime")
	archive := fixtureZip(t, "inner.dll", body)
	hArchive := sha256.Sum256(archive)
	hLib := sha256.Sum256(body)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(archive) }))
	defer srv.Close()
	dir := t.TempDir()
	lib := filepath.Join(libDir(dir), "runtime.dll")
	if err := os.MkdirAll(filepath.Dir(lib), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lib, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	asset := ortAsset{url: srv.URL + "/runtime.zip", inner: "inner.dll", lib: "runtime.dll", sha256: hex.EncodeToString(hLib[:]), archiveSHA256: hex.EncodeToString(hArchive[:])}
	if err := extractOrtLib(context.Background(), dir, asset); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(lib)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("library=%q, want %q", got, body)
	}
}

func TestExtractOrtLibHonorsCanceledContextBeforeHash(t *testing.T) {
	dir := t.TempDir()
	lib := filepath.Join(libDir(dir), "runtime.dll")
	if err := os.MkdirAll(filepath.Dir(lib), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lib, bytes.Repeat([]byte("x"), 1024), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := extractOrtLib(ctx, dir, ortAsset{url: "http://invalid", lib: "runtime.dll", sha256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"})
	if err == nil {
		t.Fatal("expected canceled extraction")
	}
	if ctx.Err() == nil {
		t.Fatal("context was not canceled")
	}
}

func TestContextReaderStopsCanceledExtractionRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := contextReader{ctx: ctx, r: bytes.NewReader([]byte("fixture"))}
	cancel()
	if _, err := r.Read(make([]byte, 8)); err != context.Canceled {
		t.Fatalf("read error=%v, want %v", err, context.Canceled)
	}
}

func TestAssetHelperProcess(t *testing.T) {
	if os.Getenv("RAGREP_ASSET_HELPER") != "1" {
		return
	}
	asset := ortAsset{url: os.Getenv("RAGREP_ASSET_URL"), inner: "inner.dll", lib: "runtime.dll", sha256: os.Getenv("RAGREP_ASSET_LIB_HASH"), archiveSHA256: os.Getenv("RAGREP_ASSET_ARCHIVE_HASH")}
	if err := extractOrtLib(context.Background(), os.Getenv("RAGREP_ASSET_DIR"), asset); err != nil {
		t.Fatal(err)
	}
}

func TestExtractOrtLibConcurrentProcessesRetainValidCache(t *testing.T) {
	body := []byte("valid runtime")
	archive := fixtureZip(t, "inner.dll", body)
	hArchive := sha256.Sum256(archive)
	hLib := sha256.Sum256(body)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(archive) }))
	defer srv.Close()
	dir := t.TempDir()
	if err := os.MkdirAll(libDir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(libDir(dir), "runtime.dll"), []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "runtime.zip"), []byte("corrupt archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"-test.run=TestAssetHelperProcess"}
	procs := make([]*exec.Cmd, 2)
	for i := range procs {
		procs[i] = exec.Command(os.Args[0], args...)
		procs[i].Env = append(os.Environ(), "RAGREP_ASSET_HELPER=1", "RAGREP_ASSET_URL="+srv.URL+"/runtime.zip", "RAGREP_ASSET_DIR="+dir, "RAGREP_ASSET_LIB_HASH="+hex.EncodeToString(hLib[:]), "RAGREP_ASSET_ARCHIVE_HASH="+hex.EncodeToString(hArchive[:]))
		if err := procs[i].Start(); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range procs {
		if err := p.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(filepath.Join(libDir(dir), "runtime.dll"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("library=%q, want %q", got, body)
	}
}

func TestDownloadContextVerifiesChecksumAndPublishesAtomically(t *testing.T) {
	want := []byte("trusted asset")
	h := sha256.Sum256(want)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(want) }))
	defer srv.Close()
	dest := filepath.Join(t.TempDir(), "asset.bin")
	if err := downloadContext(context.Background(), srv.URL, dest, hex.EncodeToString(h[:]), time.Second); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("asset=%q, want %q", got, want)
	}
}

func TestDownloadContextRejectsCorruptResponseWithoutCache(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = fmt.Fprint(w, "corrupt") }))
	defer srv.Close()
	dest := filepath.Join(t.TempDir(), "asset.bin")
	err := downloadContext(context.Background(), srv.URL, dest, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", time.Second)
	if err == nil {
		t.Fatal("expected checksum error")
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatalf("cache exists after failed verification: %v", statErr)
	}
}

func TestDownloadContextHonorsCancellationDuringBody(t *testing.T) {
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- downloadContext(ctx, srv.URL, filepath.Join(t.TempDir(), "asset.bin"), "", time.Second)
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if err == nil || ctx.Err() == nil {
			t.Fatalf("download error=%v, context=%v", err, ctx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("download did not stop after cancellation")
	}
}

func TestDownloadContextRetriesInterruptedDownload(t *testing.T) {
	var calls int
	want := []byte("complete")
	h := sha256.Sum256(want)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			_, _ = fmt.Fprint(w, "partial")
			return
		}
		_, _ = w.Write(want)
	}))
	defer srv.Close()
	dest := filepath.Join(t.TempDir(), "asset.bin")
	checksum := hex.EncodeToString(h[:])
	if err := downloadContext(context.Background(), srv.URL, dest, checksum, time.Second); err == nil {
		t.Fatal("expected first attempt to fail")
	}
	if err := downloadContext(context.Background(), srv.URL, dest, checksum, time.Second); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("requests=%d, want 2", calls)
	}
}

func TestDownloadContextConcurrentAcquisitionPublishesOneValidFile(t *testing.T) {
	want := []byte("shared")
	h := sha256.Sum256(want)
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		time.Sleep(20 * time.Millisecond)
		_, _ = w.Write(want)
	}))
	defer srv.Close()
	dest := filepath.Join(t.TempDir(), "asset.bin")
	checksum := hex.EncodeToString(h[:])
	errs := make(chan error, 2)
	for range 2 {
		go func() { errs <- downloadContext(context.Background(), srv.URL, dest, checksum, time.Second) }()
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if calls < 1 {
		t.Fatalf("requests=%d, want at least 1", calls)
	}
}

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

// TestPaddingInvariance: the DML path pads inputs to bucket lengths; the
// masked padding must not change the embedding. Compares the padded
// (useDirectML) output against the unpadded CPU output of the same model —
// these matched to ~4 decimals when padding was introduced.
func TestPaddingInvariance(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("padding is only applied on the DirectML path")
	}
	text := "title: none | text: 認証エラーはトークンの期限切れで発生する"
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
	embedOnce := func() []float32 {
		e, err := New(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer e.Close()
		v, err := e.Embed(text)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	padded := embedOnce()
	useDirectML = false
	t.Cleanup(func() { useDirectML = true })
	unpadded := embedOnce()
	var cos float64
	for i := range padded {
		cos += float64(padded[i]) * float64(unpadded[i])
	}
	if !(cos > 0.999) {
		t.Fatalf("padded vs unpadded cosine = %f", cos)
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

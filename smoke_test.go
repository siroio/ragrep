package main

import (
	"os"
	"path/filepath"
	"testing"
)

// End-to-end: index a small corpus and search it with the real model.
// Skips when model assets are not cached (run 'rag init' first).
func TestSmoke(t *testing.T) {
	dir, err := cacheDir()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "model_quantized.onnx")); err != nil {
		t.Skip("model not cached; run 'rag init' to enable this test")
	}

	tmp := t.TempDir()
	corpus := filepath.Join(tmp, "docs")
	os.MkdirAll(corpus, 0o755)
	os.WriteFile(filepath.Join(corpus, "auth.md"), []byte(
		"認証エラー一覧。\nERR_AUTH_104 はトークン期限切れ。\n\n再認証の手順はログイン画面から行う。\n"), 0o644)
	os.WriteFile(filepath.Join(corpus, "net.md"), []byte(
		"ネットワーク設定。\nプロキシはenvで指定する。\n"), 0o644)

	db := filepath.Join(tmp, "index.db")
	wd, _ := os.Getwd()
	os.Chdir(tmp)
	defer os.Chdir(wd)

	if code := run([]string{"index", "--db", db, "docs"}); code != 0 {
		t.Fatalf("index exit=%d", code)
	}
	if code := run([]string{"search", "--db", db, "--json", "ERR_AUTH_104"}); code != 0 {
		t.Fatalf("search exit=%d", code)
	}
	if code := run([]string{"search", "--db", db, "--mode", "text", "存在しない謎の文字列XYZQ"}); code != 2 {
		t.Fatalf("no-hit search exit=%d, want 2", code)
	}
	if code := run([]string{"get", "--db", db, "--para", "0", "--context", "1", "docs/auth.md"}); code != 0 {
		t.Fatalf("get exit=%d", code)
	}
	if code := run([]string{"get", "--db", db, "docs/missing.md"}); code != 2 {
		t.Fatalf("get missing exit=%d, want 2", code)
	}
	// Windows-style separators in the get path arg must still resolve to the
	// "docs/auth.md" key stored (with ToSlash) at index time.
	if code := run([]string{"get", "--db", db, `docs\auth.md`}); code != 0 {
		t.Fatalf("get backslash-path exit=%d, want 0", code)
	}
}

// A malformed flag must fail with exit 1 (this CLI's generic error code), not
// flag package's own exit 2 -- which would collide with the "no hits / not
// found" contract. Doesn't need the model: flag parsing fails before the
// embedder or DB are ever touched.
func TestUnknownFlagExitsOne(t *testing.T) {
	if code := run([]string{"search", "--bogusflag", "x"}); code != 1 {
		t.Fatalf("unknown flag exit=%d, want 1", code)
	}
}

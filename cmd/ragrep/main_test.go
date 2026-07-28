package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/siroio/ragrep/internal/store"
)

// fakeEmbed returns a fixed-dimension deterministic vector (no ONNX needed).
// Duplicated from internal/store's test helper of the same name: that one is
// unexported to package store and unreachable from here across the package
// boundary, and the value only needs to match store's own embedDim (768).
func fakeEmbed(text string) ([]float32, error) {
	const embedDim = 768
	v := make([]float32, embedDim)
	for i, r := range text {
		v[i%embedDim] += float32(r % 13)
	}
	return v, nil
}

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// -h/--help must print usage and exit 0, not read as a generic parse error
// (exit 1). Doesn't need the model: parsing fails before the embedder or DB
// are ever touched.
func TestHelpExitsZero(t *testing.T) {
	if code := run([]string{"search", "-h"}); code != 0 {
		t.Fatalf("search -h exit=%d, want 0", code)
	}
}

// getContent's --lines branch: happy path, invalid ranges, out-of-range
// start, clamping past EOF, and CRLF normalization. No model needed.
func TestGetContentLines(t *testing.T) {
	s := newTestStore(t)
	content := "l1\r\nl2\r\nl3\r\nl4\r\nl5"
	if _, err := s.UpsertDoc("a.txt", content, 1, fakeEmbed); err != nil {
		t.Fatal(err)
	}

	got, err := getContent(s, "a.txt", "2-3", -1, 0)
	if err != nil || got != "l2\nl3" {
		t.Fatalf("lines 2-3: got %q err=%v", got, err)
	}

	if _, err := getContent(s, "a.txt", "0-3", -1, 0); err == nil {
		t.Fatal("want error for a<1")
	}
	if _, err := getContent(s, "a.txt", "3-2", -1, 0); err == nil {
		t.Fatal("want error for b<a")
	}
	if _, err := getContent(s, "a.txt", "100-200", -1, 0); err != store.ErrNotFound {
		t.Fatalf("want ErrNotFound for a>len(lines), got %v", err)
	}

	got, err = getContent(s, "a.txt", "4-100", -1, 0)
	if err != nil || got != "l4\nl5" {
		t.Fatalf("clamp to EOF: got %q err=%v", got, err)
	}
}

// A panic escaping run() must surface as exit 1 (this CLI's generic error
// code), not Go's own panic exit code 2 -- which would collide with the
// "no hits / not found" contract.
// pruneDecision must only treat a "file gone" stat error as prunable; any
// other stat error (permission denied, transient I/O, AV lock, ...) must
// abort rather than be silently treated as "gone" (which would delete a
// still-valid document).
func TestPruneDecision(t *testing.T) {
	if prune, err := pruneDecision(nil); prune || err != nil {
		t.Fatalf("nil stat err: prune=%v err=%v, want false,nil", prune, err)
	}
	if prune, err := pruneDecision(fs.ErrNotExist); !prune || err != nil {
		t.Fatalf("ErrNotExist: prune=%v err=%v, want true,nil", prune, err)
	}
	other := errors.New("permission denied")
	if prune, err := pruneDecision(other); prune || err != other {
		t.Fatalf("other stat err: prune=%v err=%v, want false,%v", prune, err, other)
	}
}

func TestProtect(t *testing.T) {
	if code := protect(func() int { panic("boom") }); code != 1 {
		t.Fatalf("panic: got %d, want 1", code)
	}
	if code := protect(func() int { return 2 }); code != 2 {
		t.Fatalf("no panic: got %d, want 2", code)
	}
}

// RAGREP_DB overrides the default --db path; an explicit --db still wins.
func TestDBFlagEnvDefault(t *testing.T) {
	t.Setenv("RAGREP_DB", filepath.Join("some", "shared.db"))
	fs := newFlagSet("x")
	db := dbFlag(fs)
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if *db != filepath.Join("some", "shared.db") {
		t.Fatalf("db=%q, want env default", *db)
	}

	fs2 := newFlagSet("x")
	db2 := dbFlag(fs2)
	if err := fs2.Parse([]string{"--db", "explicit.db"}); err != nil {
		t.Fatal(err)
	}
	if *db2 != "explicit.db" {
		t.Fatalf("db=%q, want explicit.db", *db2)
	}
}

// From a subdirectory, the default db path resolves to the nearest ancestor's
// existing .ragrep/index.db; with none, it stays cwd-relative.
func TestDefaultDBPathWalksUp(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)

	if got := defaultDBPath(); got != filepath.Join(".ragrep", "index.db") {
		t.Fatalf("no ancestor db: got %q, want cwd-relative default", got)
	}

	rootDB := filepath.Join(root, ".ragrep", "index.db")
	if err := os.MkdirAll(filepath.Dir(rootDB), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rootDB, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := defaultDBPath(); got != rootDB {
		t.Fatalf("got %q, want %q", got, rootDB)
	}

	t.Setenv("RAGREP_DB", "env.db")
	if got := defaultDBPath(); got != "env.db" {
		t.Fatalf("env should win: got %q", got)
	}
}

// strFlags is a repeatable string flag (e.g. --tag t1 --tag t2); Set appends
// rather than overwrites so multiple occurrences accumulate.
func TestStrFlagsSet(t *testing.T) {
	var tags strFlags
	if err := tags.Set("a"); err != nil {
		t.Fatal(err)
	}
	if err := tags.Set("b"); err != nil {
		t.Fatal(err)
	}
	if len(tags) != 2 || tags[0] != "a" || tags[1] != "b" {
		t.Fatalf("tags=%v, want [a b]", tags)
	}
}

// withFrontmatter prepends a tags frontmatter block unless no tags were
// given or content already starts with one.
func TestWithFrontmatter(t *testing.T) {
	got := withFrontmatter("body text", []string{"go", "cli"})
	want := "---\ntags: [go, cli]\n---\n\nbody text"
	if got != want {
		t.Fatalf("with tags: got %q, want %q", got, want)
	}

	got = withFrontmatter("body text", nil)
	if got != "body text" {
		t.Fatalf("no tags: got %q, want passthrough", got)
	}

	existing := "---\ntags: [old]\n---\nbody text"
	got = withFrontmatter(existing, []string{"go"})
	if got != existing {
		t.Fatalf("existing frontmatter: got %q, want passthrough", got)
	}
}

// ragrep add must refuse to overwrite a file that already exists, and must
// do so before reading stdin -- so this test can run to completion (exit 1)
// without a model or stdin input.
func TestCmdAddRefusesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "existing.md")
	if err := os.WriteFile(path, []byte("already here"), 0o644); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(t.TempDir(), "index.db")

	if code := run([]string{"add", "--db", db, path}); code != 1 {
		t.Fatalf("add existing file: got exit %d, want 1", code)
	}

	// Flags-first, tags before the path -- the only ordering `flag` accepts
	// (it stops parsing at the first non-flag argument). Reaching the
	// existing-file refusal (exit 1, not a usage error) proves the flagset
	// parsed --tag and --db as flags and the path as the sole positional arg.
	if code := run([]string{"add", "--tag", "x", "--db", db, path}); code != 1 {
		t.Fatalf("add --tag x --db db path: got exit %d, want 1 (refusal)", code)
	}
}

// cmdAdd's flagset must parse `--tag t]... <path>` (flags before the sole
// positional), matching every other ragrep subcommand -- documenting this in
// a flagset-only test protects it independent of the CLI usage strings.
func TestCmdAddFlagOrdering(t *testing.T) {
	fs := newFlagSet("add")
	dbFlag(fs)
	var tags strFlags
	fs.Var(&tags, "tag", "tag to add to the new file's frontmatter (repeatable)")
	if err := fs.Parse([]string{"--tag", "x", "--tag", "y", "some/path.md"}); err != nil {
		t.Fatal(err)
	}
	if fs.NArg() != 1 || fs.Arg(0) != "some/path.md" {
		t.Fatalf("NArg=%d Arg(0)=%q, want 1 arg %q", fs.NArg(), fs.Arg(0), "some/path.md")
	}
	if len(tags) != 2 || tags[0] != "x" || tags[1] != "y" {
		t.Fatalf("tags=%v, want [x y]", tags)
	}
}

// All path argument styles normalize to one canonical absolute slash key.
func TestNormPath(t *testing.T) {
	t.Chdir(t.TempDir())
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	want, err := normPath("docs/foo.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range []string{"./docs/foo.md", filepath.Join(cwd, "docs", "foo.md"), want} {
		got, err := normPath(in)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("normPath(%q) = %q, want %q", in, got, want)
		}
	}
}

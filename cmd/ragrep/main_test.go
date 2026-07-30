package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/siroio/ragrep/internal/store"
)

func TestNormalizeInterspersedArgs(t *testing.T) {
	fs := newFlagSet("test")
	fs.String("mode", "hybrid", "")
	fs.Int("k", 10, "")
	fs.Bool("json", false, "")
	var tags strFlags
	fs.Var(&tags, "tag", "")

	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"flags first", []string{"--json", "-k", "5", "query"}, []string{"--json", "-k", "5", "query"}},
		{"flags last", []string{"query", "--json", "-k", "5"}, []string{"--json", "-k", "5", "query"}},
		{"mixed and repeated", []string{"a", "--tag", "x", "b", "--tag=y"}, []string{"--tag", "x", "--tag=y", "a", "b"}},
		{"equals", []string{"query", "--mode=text"}, []string{"--mode=text", "query"}},
		{"dash-prefixed value", []string{"query", "-k", "-1"}, []string{"-k", "-1", "query"}},
		{"single dash positional", []string{"-", "--json"}, []string{"--json", "-"}},
		{"terminator", []string{"query", "--json", "--", "-k", "literal"}, []string{"--json", "--", "query", "-k", "literal"}},
		{"unknown flag retained", []string{"query", "--bogus"}, []string{"--bogus", "query"}},
		{"missing value retained", []string{"query", "--mode"}, []string{"query", "--mode"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeInterspersedArgs(fs, tt.in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

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

func TestParseArgsAcceptsInterspersedFlags(t *testing.T) {
	fs := newFlagSet("test")
	mode := fs.String("mode", "hybrid", "")
	k := fs.Int("k", 10, "")
	asJSON := fs.Bool("json", false, "")
	var tags strFlags
	fs.Var(&tags, "tag", "")

	code, handled := parseArgs(fs, []string{"first", "--json", "--tag", "a", "second", "-k", "5", "--mode=text", "--tag=b"})
	if handled || code != 0 {
		t.Fatalf("parseArgs: code=%d handled=%v, want 0,false", code, handled)
	}
	if got, want := fs.Args(), []string{"first", "second"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Args=%q, want %q", got, want)
	}
	if *mode != "text" || *k != 5 || !*asJSON {
		t.Fatalf("mode=%q k=%d json=%v", *mode, *k, *asJSON)
	}
	if got, want := []string(tags), []string{"a", "b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("tags=%q, want %q", got, want)
	}
}

// -h/--help must print usage and exit 0, not read as a generic parse error
// (exit 1). Doesn't need the model: parsing fails before the embedder or DB
// are ever touched.
func TestHelpExitsZero(t *testing.T) {
	if code := run([]string{"search", "-h"}); code != 0 {
		t.Fatalf("search -h exit=%d, want 0", code)
	}
	if code := run([]string{"search", "query", "-h"}); code != 0 {
		t.Fatalf("search query -h exit=%d, want 0", code)
	}
}

func TestGetAcceptsFlagsAfterPath(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "index.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertDoc("a.md", "alpha paragraph.", 1, fakeEmbed); err != nil {
		t.Fatal(err)
	}
	s.Close()

	if code := run([]string{"get", "a.md", "--db", dbPath, "--para", "0"}); code != 0 {
		t.Fatalf("get path --flags exit=%d, want 0", code)
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

// TestHelperProcess is not a real test: it's the "converter" runConverter's
// tests exec as a subprocess (the stdlib os/exec self-exec pattern), guarded
// by GO_WANT_HELPER_PROCESS so a normal `go test` run doesn't execute its
// body as a test.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	fmt.Println("converted:" + filepath.Base(os.Args[len(os.Args)-1]))
	os.Exit(0)
}

// runConverter must substitute {input} into the argv, run it, and return
// stdout; env must reach the child (exec.Command inherits os.Environ() by
// default) so GO_WANT_HELPER_PROCESS set here via t.Setenv is visible to the
// self-exec'd TestHelperProcess above.
func TestRunConverter(t *testing.T) {
	t.Setenv("GO_WANT_HELPER_PROCESS", "1")
	argv := []string{os.Args[0], "-test.run=TestHelperProcess", "--", "{input}"}

	out, err := runConverter(argv, filepath.Join("tmp", "report.pdf"))
	if err != nil {
		t.Fatalf("runConverter: %v", err)
	}
	if out != "converted:report.pdf\n" {
		t.Fatalf("runConverter output=%q, want %q", out, "converted:report.pdf\n")
	}

	if _, err := runConverter([]string{"ragrep-no-such-converter-binary"}, "x"); err == nil {
		t.Fatal("runConverter: want error for unknown binary, got nil")
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

// Resolution order for the --db default: RAGREP_DB > .ragrep/config.json's
// "db" field > the plain default -- CLI flag > default is already covered by
// TestDBFlagEnvDefault (an explicit --db always wins over whatever default
// dbFlag was built with).
func TestDefaultDBPathUsesConfig(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)

	ragrepDir := filepath.Join(root, ".ragrep")
	if err := os.MkdirAll(ragrepDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgJSON := `{"db": "custom/idx.db"}`
	if err := os.WriteFile(filepath.Join(ragrepDir, "config.json"), []byte(cfgJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	want := filepath.Join(root, "custom", "idx.db")
	if got := defaultDBPath(); got != want {
		t.Fatalf("config db: got %q, want %q", got, want)
	}

	t.Setenv("RAGREP_DB", "env.db")
	if got := defaultDBPath(); got != "env.db" {
		t.Fatalf("env should win over config: got %q", got)
	}
}

// A malformed .ragrep/config.json must not crash --db default resolution
// (which runs at flagset-construction time, before any command can report a
// clean error) -- it falls back to the plain default under the discovered
// root instead.
func TestDefaultDBPathBadConfigFallsBack(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)

	ragrepDir := filepath.Join(root, ".ragrep")
	if err := os.MkdirAll(ragrepDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ragrepDir, "config.json"), []byte(`{not valid json`), 0o644); err != nil {
		t.Fatal(err)
	}

	want := filepath.Join(root, ".ragrep", "index.db")
	if got := defaultDBPath(); got != want {
		t.Fatalf("bad config: got %q, want fallback %q", got, want)
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

// cmdAdd must validate the target is inside the workspace root BEFORE
// writing anything to disk. Validating late (after os.MkdirAll/os.WriteFile)
// would leave a stray unindexed file outside the workspace when the command
// then fails.
func TestCmdAddRejectsOutsideRoot(t *testing.T) {
	root, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(root, ".ragrep", "index.db")

	outside := filepath.Join(filepath.Dir(root), "ragrep-add-outside-test.md")
	os.Remove(outside) // in case a previous failed run left it behind
	t.Cleanup(func() { os.Remove(outside) })

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString("stray content"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	oldStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = oldStdin }()

	if code := run([]string{"add", "--db", db, outside}); code != 1 {
		t.Fatalf("add outside root: exit=%d, want 1", code)
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatalf("add outside root must not create the file on disk; stat err=%v", err)
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

// workspaceRoot derives the workspace root from --db: a ".ragrep/index.db"
// shaped path roots at the grandparent (the workspace dir containing
// .ragrep/), any other db path roots at its own parent directory.
func TestWorkspaceRoot(t *testing.T) {
	tmp, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	ragrepDB := filepath.Join(tmp, ".ragrep", "index.db")
	root, err := workspaceRoot(ragrepDB)
	if err != nil {
		t.Fatal(err)
	}
	if root != tmp {
		t.Fatalf(".ragrep/index.db shaped db: root=%q, want grandparent %q", root, tmp)
	}

	plainDB := filepath.Join(tmp, "sub", "plain.db")
	wantSub := filepath.Join(tmp, "sub")
	root, err = workspaceRoot(plainDB)
	if err != nil {
		t.Fatal(err)
	}
	if root != wantSub {
		t.Fatalf("plain db: root=%q, want parent %q", root, wantSub)
	}
}

// All in-workspace path argument styles (relative, ./x, absolute) normalize
// to one canonical root-relative slash key; the root itself normalizes to
// ".", and anything outside the root is rejected with an explicit error.
func TestNormPath(t *testing.T) {
	root, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const want = "docs/foo.md"

	got, err := normPath(filepath.Join(root, "docs", "foo.md"), root)
	if err != nil || got != want {
		t.Fatalf("absolute-in-root: got %q err=%v, want %q", got, err, want)
	}

	t.Chdir(root)
	for _, in := range []string{"docs/foo.md", "./docs/foo.md"} {
		got, err := normPath(in, root)
		if err != nil || got != want {
			t.Fatalf("normPath(%q, root) = %q err=%v, want %q", in, got, err, want)
		}
	}

	got, err = normPath(root, root)
	if err != nil || got != "." {
		t.Fatalf("normPath(root, root) = %q err=%v, want \".\"", got, err)
	}

	outside := filepath.Dir(root) // root's own parent: definitely outside root
	_, err = normPath(outside, root)
	wantErr := fmt.Sprintf("%s is outside the workspace root %s (indexable paths must live under the workspace)", outside, root)
	if err == nil || err.Error() != wantErr {
		t.Fatalf("normPath(outside, root) err=%v, want %q", err, wantErr)
	}
}

// looksAbsKey detects the old (pre-relative) key format: a leading "/" or a
// Windows drive-letter prefix like "C:/". Root-relative keys never look like
// this, so it doubles as the guard predicate for the old-DB check.
func TestLooksAbsKey(t *testing.T) {
	cases := map[string]bool{
		"/u/x.md":   true,
		"C:/u/x.md": true,
		"docs/x.md": false,
		".":         false,
	}
	for k, want := range cases {
		if got := looksAbsKey(k); got != want {
			t.Fatalf("looksAbsKey(%q)=%v, want %v", k, got, want)
		}
	}
}

// A DB carrying old absolute-form keys must be rejected up front with an
// explicit migration message (exit 1), not silently misbehave under the new
// root-relative key scheme.
func TestOldKeyGuard(t *testing.T) {
	db := filepath.Join(t.TempDir(), "index.db")
	s, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertDoc("/abs/old/key.md", "content", 1, fakeEmbed); err != nil {
		t.Fatal(err)
	}
	s.Close()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = old }()
	code := run([]string{"search", "--db", db, "--mode", "text", "q"})
	w.Close()
	os.Stderr = old
	var buf bytes.Buffer
	io.Copy(&buf, r)

	if code != 1 {
		t.Fatalf("exit=%d, want 1", code)
	}
	if !strings.Contains(buf.String(), "old absolute-path key format") {
		t.Fatalf("stderr=%q, want guard message", buf.String())
	}
}

// cmdGet resolves a path argument three ways against the same document: the
// verbatim (root-relative) key as printed by search, a cwd-relative path,
// and an absolute path -- regardless of which subdirectory the command runs
// from.
func TestGetFallback(t *testing.T) {
	root, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(root, ".ragrep", "index.db")
	if err := os.MkdirAll(filepath.Dir(db), 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertDoc("docs/auth.md", "auth content", 1, fakeEmbed); err != nil {
		t.Fatal(err)
	}
	s.Close()

	sub := filepath.Join(root, "docs")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)

	if code := run([]string{"get", "--db", db, "docs/auth.md"}); code != 0 {
		t.Fatalf("verbatim root-relative key: exit=%d, want 0", code)
	}
	if code := run([]string{"get", "--db", db, "./auth.md"}); code != 0 {
		t.Fatalf("cwd-relative path: exit=%d, want 0", code)
	}
	if code := run([]string{"get", "--db", db, filepath.Join(sub, "auth.md")}); code != 0 {
		t.Fatalf("absolute path: exit=%d, want 0", code)
	}
}

// If the verbatim key misses and the fallback normPath rejects the arg as
// outside the workspace root, cmdGet must preserve the original "not found"
// result (exit 2) rather than surfacing normPath's outside-workspace error
// as a generic failure (exit 1).
func TestGetFallbackOutsideRootStaysNotFound(t *testing.T) {
	root, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(root, ".ragrep", "index.db")
	if err := os.MkdirAll(filepath.Dir(db), 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	t.Chdir(root)
	if code := run([]string{"get", "--db", db, "../nonexistent-outside.md"}); code != 2 {
		t.Fatalf("get outside-root arg: exit=%d, want 2 (not found), not 1", code)
	}
}

// cmdIndex must validate every root argument is inside the workspace root UP
// FRONT, before walking any of them -- not only when the walk happens to
// reach an indexable file. Previously an outside-root arg that was empty (or
// contained only skipped files) walked to "0 indexed" and exited 0 instead of
// failing; a companion empty dir INSIDE the root proves the check is about
// root membership, not mere emptiness.
func TestCmdIndexRejectsOutsideRootEvenWhenEmpty(t *testing.T) {
	root, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(root, ".ragrep", "index.db")

	outside := filepath.Join(filepath.Dir(root), "ragrep-index-outside-empty-test")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(outside) })

	if code := run([]string{"index", "--db", db, outside}); code != 1 {
		t.Fatalf("index outside-root empty dir: exit=%d, want 1 (bug: was 0)", code)
	}

	inside := filepath.Join(root, "empty")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"index", "--db", db, inside}); code != 0 {
		t.Fatalf("index inside-root empty dir: exit=%d, want 0", code)
	}
}

// --prune's existence check must be resolved against the workspace root, not
// the process's cwd: running from a sibling directory of "docs" must still
// prune the doc that's actually gone and keep the one that's actually there.
func TestPruneRootJoinStat(t *testing.T) {
	root, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	docs := filepath.Join(root, "docs")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(docs, "keep.md"), []byte("keep content"), 0o644); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(root, "other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}

	db := filepath.Join(root, ".ragrep", "index.db")
	if err := os.MkdirAll(filepath.Dir(db), 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertDoc("docs/gone.md", "gone content", 1, fakeEmbed); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertDoc("docs/keep.md", "keep content", 1, fakeEmbed); err != nil {
		t.Fatal(err)
	}
	s.Close()

	t.Chdir(other)

	if code := run([]string{"index", "--prune", "--db", db, "../docs"}); code != 0 {
		t.Fatalf("index --prune exit=%d", code)
	}

	s2, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if _, err := s2.GetDoc("docs/gone.md"); err != store.ErrNotFound {
		t.Fatalf("gone.md: want pruned (ErrNotFound), got %v", err)
	}
	if _, err := s2.GetDoc("docs/keep.md"); err != nil {
		t.Fatalf("keep.md: want kept, got %v", err)
	}
}

// markStale flags hits whose on-disk file is missing or whose mtime differs
// from the indexed one; hits already matching the real file are left alone.
func TestMarkStale(t *testing.T) {
	tmpdir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpdir, "a.md"), []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(tmpdir, "a.md"))
	if err != nil {
		t.Fatal(err)
	}
	realMtime := info.ModTime().Unix()

	hits := []store.Hit{
		{Doc: "a.md", Mtime: realMtime},
		{Doc: "a.md", Mtime: realMtime - 10},
		{Doc: "gone.md", Mtime: 1},
	}
	n := markStale(hits, tmpdir)
	if n != 2 {
		t.Fatalf("n=%d, want 2", n)
	}
	if hits[0].Stale {
		t.Fatal("hits[0] (fresh) marked stale")
	}
	if !hits[1].Stale {
		t.Fatal("hits[1] (changed mtime) not marked stale")
	}
	if !hits[2].Stale {
		t.Fatal("hits[2] (missing file) not marked stale")
	}
}

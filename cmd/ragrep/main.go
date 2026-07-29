package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/siroio/ragrep/internal/config"
	"github.com/siroio/ragrep/internal/embed"
	"github.com/siroio/ragrep/internal/store"
)

const usage = `ragrep - adaptive retrieval unit search CLI

Usage:
  ragrep init                              create DB, download model assets
  ragrep index <path>... [--prune] [--include-code]   index text files (recursive)
  ragrep search <query> [--mode hybrid|vector|text] [-k 10] [--json] [--tag t]...
  ragrep get <path> [--para N] [--context N] [--lines A-B]
  ragrep add [--tag t]... <path>            (reads content from stdin)
  ragrep code index|search|get|expand|pack|verify ...  code symbol indexing/search (see 'ragrep code -h')

Flags common to all commands:
  --db PATH    index database (default $RAGREP_DB, else .ragrep/config.json db, else .ragrep/index.db)

Exit codes: 0 success, 1 error, 2 no hits / not found
`

func main() {
	os.Exit(protect(func() int { return run(os.Args[1:]) }))
}

// protect recovers a panic escaping f and converts it to exit code 1 (this
// CLI's generic error code) instead of letting it escape main() and exit
// with Go's own panic code 2 -- which would collide with the "no hits /
// not found" contract.
func protect(f func() int) (code int) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintln(os.Stderr, "panic:", r)
			code = 1
		}
	}()
	return f()
}

func fail(err error) int {
	fmt.Fprintln(os.Stderr, "error:", err)
	return 1
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 1
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "init":
		return cmdInit(rest)
	case "index":
		return cmdIndex(rest)
	case "search":
		return cmdSearch(rest)
	case "get":
		return cmdGet(rest)
	case "add":
		return cmdAdd(rest)
	case "code":
		return cmdCode(rest)
	default:
		fmt.Fprint(os.Stderr, usage)
		return 1
	}
}

func dbFlag(fs *flag.FlagSet) *string {
	return fs.String("db", defaultDBPath(), "index database path")
}

// defaultDBPath resolves the --db default, in priority order: $RAGREP_DB
// (kept for compatibility) > .ragrep/config.json's "db" field > the plain
// default. The workspace root is found by walking up from cwd looking for a
// .ragrep directory (so running from a subdirectory of an indexed repo finds
// the root index/config), else the default is cwd-relative. A config.json
// that fails to parse doesn't abort here -- this runs at flagset-construction
// time for every command, before any command can report a clean error -- so
// it falls back to the plain default under the discovered root instead; the
// same malformed file is still reported clearly by config.Load's own callers
// (e.g. language server lookups). DB keys are workspace-root-relative (see
// normPath), not absolute -- any-subdirectory operation comes from this
// walk-up discovery plus resolving paths relative to the discovered root,
// not from the keys themselves.
func defaultDBPath() string {
	if p := os.Getenv("RAGREP_DB"); p != "" {
		return p
	}
	local := filepath.FromSlash(config.DefaultDB)
	dir, err := os.Getwd()
	if err != nil {
		return local
	}
	for {
		if info, err := os.Stat(filepath.Join(dir, ".ragrep")); err == nil && info.IsDir() {
			cfg, err := config.Load(dir)
			if err != nil {
				return filepath.Join(dir, local)
			}
			return filepath.Join(dir, filepath.FromSlash(cfg.DB))
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return local
		}
		dir = parent
	}
}

// newFlagSet builds a FlagSet that reports parse errors to the caller
// (ContinueOnError) instead of exiting the process with flag's own exit code
// 2 — that code collides with this CLI's "no hits / not found" contract.
// Output is discarded because parseArgs below prints instead, so a bad flag
// (or -h) doesn't get printed twice.
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// parseArgs parses args into fs. -h/--help prints usage and reports exit 0
// (flag.ErrHelp would otherwise read as a generic parse failure → exit 1).
// Any other parse error goes through fail(). handled is true when the caller
// should return code immediately instead of continuing.
func parseArgs(fs *flag.FlagSet, args []string) (code int, handled bool) {
	return parseArgsUsage(fs, args, usage)
}

// parseArgsUsage is parseArgs, printing usageText instead of the top-level
// document-index usage on -h/--help -- cmd/ragrep's code.go uses this with
// codeUsage so `ragrep code <sub> -h` prints the `code` subcommand help, not
// the unrelated top-level one.
func parseArgsUsage(fs *flag.FlagSet, args []string, usageText string) (code int, handled bool) {
	err := fs.Parse(args)
	if err == nil {
		return 0, false
	}
	if err == flag.ErrHelp {
		fmt.Fprint(os.Stderr, usageText)
		return 0, true
	}
	return fail(err), true
}

// workspaceRoot derives the workspace root from a --db path: the normal
// shape is <root>/.ragrep/index.db, so the root is that directory's
// grandparent; for any other db path (e.g. an explicit --db elsewhere) the
// root is just the db's own parent directory. Doc keys are stored relative
// to this root (see normPath), so the whole workspace can be moved, renamed,
// or copied without invalidating the index.
func workspaceRoot(dbPath string) (string, error) {
	abs, err := filepath.Abs(dbPath)
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(abs)
	if filepath.Base(dir) == ".ragrep" {
		return filepath.Dir(dir), nil
	}
	return dir, nil
}

// looksAbsKey reports whether k has the shape of a key from the old
// (pre-relative) absolute-path key format: a leading "/" (POSIX-style) or a
// Windows drive-letter prefix such as "C:/". Root-relative keys (this
// version's format) never start this way.
func looksAbsKey(k string) bool {
	if strings.HasPrefix(k, "/") {
		return true
	}
	if len(k) >= 3 && k[2] == '/' && k[1] == ':' {
		c := k[0]
		return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
	}
	return false
}

func openStoreAt(dbPath string) (*store.Store, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, err
	}
	s, err := store.Open(dbPath)
	if err != nil {
		return nil, err
	}
	first, err := s.FirstPath()
	if err != nil {
		s.Close()
		return nil, err
	}
	if looksAbsKey(first) {
		s.Close()
		return nil, fmt.Errorf("old absolute-path key format; delete .ragrep/ (or the index db) and re-run ragrep index")
	}
	return s, nil
}

func cmdInit(args []string) int {
	fs := newFlagSet("init")
	db := dbFlag(fs)
	if code, handled := parseArgs(fs, args); handled {
		return code
	}
	dir, err := embed.CacheDir()
	if err != nil {
		return fail(err)
	}
	if err := embed.EnsureAssets(dir); err != nil {
		return fail(err)
	}
	s, err := openStoreAt(*db)
	if err != nil {
		return fail(err)
	}
	s.Close()
	fmt.Printf("initialized %s (assets in %s)\n", *db, dir)
	return 0
}

// isTextFile reports whether the first 8KB contain no NUL byte.
func isTextFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 8192)
	n, _ := f.Read(buf)
	for _, b := range buf[:n] {
		if b == 0 {
			return false
		}
	}
	return n > 0
}

const maxFileSize = 10 << 20 // 10MB

// codeExtensions are file extensions the document `index` command excludes
// by default -- source code belongs in the code index (`ragrep code index`),
// not the document index. --include-code disables this exclusion.
var codeExtensions = map[string]bool{
	".go": true, ".py": true, ".js": true, ".ts": true, ".java": true,
	".c": true, ".cpp": true, ".h": true, ".rs": true,
}

// pruneDecision interprets an os.Stat error for --prune: a file that's gone
// (fs.ErrNotExist) should be pruned; any other stat error (permission
// denied, transient I/O, AV lock, ...) must abort the run rather than be
// silently treated as "gone" -- which would delete a still-valid document.
func pruneDecision(statErr error) (prune bool, err error) {
	if statErr == nil {
		return false, nil
	}
	if errors.Is(statErr, fs.ErrNotExist) {
		return true, nil
	}
	return false, statErr
}

func cmdIndex(args []string) int {
	fset := newFlagSet("index")
	db := dbFlag(fset)
	prune := fset.Bool("prune", false, "remove indexed docs under the given roots that no longer exist on disk")
	includeCode := fset.Bool("include-code", false, "don't exclude common source code extensions (.go, .py, ...) from the document index")
	if code, handled := parseArgs(fset, args); handled {
		return code
	}
	if fset.NArg() == 0 {
		return fail(fmt.Errorf("usage: ragrep index <path>..."))
	}
	wsRoot, err := workspaceRoot(*db)
	if err != nil {
		return fail(err)
	}
	// Validate every root arg is inside the workspace UP FRONT, before
	// opening the store or loading the (slow) embedding model. Deferring this
	// to normPath inside the walk only fires it when the walk reaches an
	// indexable file -- an outside-root arg that's empty or binary-only would
	// walk to "0 indexed" and exit 0, and a mix of good/bad args would
	// partially index before failing on the bad one.
	normRoots := make([]string, fset.NArg())
	for i, r := range fset.Args() {
		nr, err := normPath(r, wsRoot)
		if err != nil {
			return fail(err)
		}
		normRoots[i] = nr
	}
	s, err := openStoreAt(*db)
	if err != nil {
		return fail(err)
	}
	defer s.Close()
	dir, err := embed.CacheDir()
	if err != nil {
		return fail(err)
	}
	e, err := embed.New(dir)
	if err != nil {
		return fail(err)
	}
	defer e.Close()

	indexed, skipped, excluded := 0, 0, 0
	for _, root := range fset.Args() {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			name := d.Name()
			if d.IsDir() {
				if strings.HasPrefix(name, ".") && path != root {
					return filepath.SkipDir
				}
				return nil
			}
			if !*includeCode && codeExtensions[strings.ToLower(filepath.Ext(name))] {
				excluded++
				return nil
			}
			info, err := d.Info()
			if err != nil || info.Size() > maxFileSize || !isTextFile(path) {
				skipped++
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, err := normPath(path, wsRoot)
			if err != nil {
				return err
			}
			changed, err := s.UpsertDoc(rel, string(data), info.ModTime().Unix(), e.Embed)
			if err != nil {
				return fmt.Errorf("%s: %w", rel, err)
			}
			if changed {
				fmt.Println("indexed", rel)
				indexed++
			}
			return nil
		})
		if err != nil {
			return fail(err)
		}
	}
	pruned := 0
	if *prune {
		roots := normRoots
		paths, err := s.ListPaths()
		if err != nil {
			return fail(err)
		}
		for _, p := range paths {
			underRoot := false
			for _, root := range roots {
				if root == "." || p == root || strings.HasPrefix(p, root+"/") {
					underRoot = true
					break
				}
			}
			if !underRoot {
				continue
			}
			_, statErr := os.Stat(filepath.Join(wsRoot, filepath.FromSlash(p)))
			doPrune, err := pruneDecision(statErr)
			if err != nil {
				return fail(err)
			}
			if !doPrune {
				continue
			}
			if err := s.DeleteDoc(p); err != nil {
				return fail(err)
			}
			fmt.Println("pruned", p)
			pruned++
		}
		fmt.Printf("done: %d indexed, %d skipped, %d excluded (code), %d pruned\n", indexed, skipped, excluded, pruned)
	} else {
		fmt.Printf("done: %d indexed, %d skipped, %d excluded (code)\n", indexed, skipped, excluded)
	}
	return 0
}

// strFlags is a repeatable string flag: each --tag occurrence appends to the
// slice instead of overwriting it, so `--tag a --tag b` yields ["a", "b"].
type strFlags []string

func (f *strFlags) String() string { return strings.Join(*f, ",") }

func (f *strFlags) Set(v string) error {
	*f = append(*f, v)
	return nil
}

func cmdSearch(args []string) int {
	fs := newFlagSet("search")
	db := dbFlag(fs)
	mode := fs.String("mode", "hybrid", "hybrid|vector|text")
	k := fs.Int("k", 10, "max results")
	asJSON := fs.Bool("json", false, "JSON output")
	var tags strFlags
	fs.Var(&tags, "tag", "filter to docs having this tag (repeatable, AND)")
	if code, handled := parseArgs(fs, args); handled {
		return code
	}
	if fs.NArg() != 1 {
		return fail(fmt.Errorf("usage: ragrep search <query>"))
	}
	query := fs.Arg(0)

	s, err := openStoreAt(*db)
	if err != nil {
		return fail(err)
	}
	defer s.Close()

	var hits []store.Hit
	switch *mode {
	case "text":
		hits, err = s.SearchText(query, *k, []string(tags))
	case "vector", "hybrid":
		dir, derr := embed.CacheDir()
		if derr != nil {
			return fail(derr)
		}
		e, eerr := embed.New(dir)
		if eerr != nil {
			return fail(eerr)
		}
		defer e.Close()
		qv, verr := e.Embed("task: search result | query: " + query)
		if verr != nil {
			return fail(verr)
		}
		if *mode == "vector" {
			hits, err = s.SearchVector(qv, *k, []string(tags))
		} else {
			hits, err = s.SearchHybrid(query, qv, *k, []string(tags))
		}
	default:
		return fail(fmt.Errorf("unknown mode %q", *mode))
	}
	if err != nil {
		return fail(err)
	}
	if len(hits) == 0 {
		fmt.Fprintln(os.Stderr, "no hits")
		return 2
	}
	if *asJSON {
		json.NewEncoder(os.Stdout).Encode(hits)
	} else {
		for _, h := range hits {
			hdr := ""
			if h.Heading != "" {
				hdr = " | " + h.Heading
			}
			fmt.Printf("%s#%d (lines %s, score %.4f)%s\n  %s\n", h.Doc, h.Para, h.Lines, h.Score, hdr, h.Snippet)
		}
	}
	return 0
}

// normPath converts p to the canonical DB key form: root-relative,
// slash-separated. One canonical form means index/get/prune agree no matter
// what cwd or path style (relative, ./x, absolute) the user passes, and
// keys stay valid if the whole workspace is moved, renamed, or copied
// elsewhere -- only position relative to root matters. p outside root
// (including a Rel failure, e.g. a different drive on Windows) is an error.
// ponytail: no case-folding or symlink resolution. filepath.Rel already
// case-folds the comparison on Windows, so drive-letter case isn't the
// ceiling; the real one is that the key's case comes verbatim from the arg as
// typed -- `ragrep index DOCS` vs `ragrep index docs` produces two distinct
// keys for the same file on a case-insensitive filesystem, and --prune's
// prefix match (strings.HasPrefix, case-sensitive) won't catch the stale
// duplicate. Fold keys to one case on Windows/mac if this ever bites.
func normPath(p, root string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, abs)
	if err == nil {
		rel = filepath.ToSlash(rel)
	}
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("%s is outside the workspace root %s (indexable paths must live under the workspace)", p, root)
	}
	return rel, nil
}

func cmdGet(args []string) int {
	fs := newFlagSet("get")
	db := dbFlag(fs)
	para := fs.Int("para", -1, "paragraph number (from search results)")
	context := fs.Int("context", 0, "±N paragraphs around --para")
	lines := fs.String("lines", "", "line range A-B")
	if code, handled := parseArgs(fs, args); handled {
		return code
	}
	if fs.NArg() != 1 {
		return fail(fmt.Errorf("usage: ragrep get <path>"))
	}
	arg := fs.Arg(0)
	wsRoot, err := workspaceRoot(*db)
	if err != nil {
		return fail(err)
	}

	s, err := openStoreAt(*db)
	if err != nil {
		return fail(err)
	}
	defer s.Close()

	// Resolution order: try the arg verbatim (cleaned/slashed) first, since
	// that's the exact root-relative key search results print -- it must
	// match regardless of cwd. Only on ErrNotFound do we fall back to
	// resolving the arg as a cwd-relative or absolute path against the
	// workspace root (a normPath failure, i.e. outside the workspace, leaves
	// the original not-found result in place).
	key := path.Clean(filepath.ToSlash(arg))
	out, err := getContent(s, key, *lines, *para, *context)
	if err == store.ErrNotFound {
		if normKey, nerr := normPath(arg, wsRoot); nerr == nil {
			out, err = getContent(s, normKey, *lines, *para, *context)
		}
	}
	if err == store.ErrNotFound {
		fmt.Fprintln(os.Stderr, "not found")
		return 2
	}
	if err != nil {
		return fail(err)
	}
	fmt.Println(out)
	return 0
}

// getContent resolves the requested slice of a document: an explicit line
// range, a paragraph ± context window, or (default) the whole document.
// Split out of cmdGet because the brief's inline switch/break mixed err
// scopes across cases in a way that didn't read cleanly as straight-line code.
func getContent(s *store.Store, path, lines string, para, context int) (string, error) {
	switch {
	case lines != "":
		var a, b int
		if _, err := fmt.Sscanf(lines, "%d-%d", &a, &b); err != nil || a < 1 || b < a {
			return "", fmt.Errorf("invalid --lines %q (want A-B)", lines)
		}
		doc, err := s.GetDoc(path)
		if err != nil {
			return "", err
		}
		ls := strings.Split(strings.ReplaceAll(doc, "\r\n", "\n"), "\n")
		if a > len(ls) {
			return "", store.ErrNotFound
		}
		if b > len(ls) {
			b = len(ls)
		}
		return strings.Join(ls[a-1:b], "\n"), nil
	case para >= 0:
		return s.GetParas(path, para, context)
	default:
		return s.GetDoc(path)
	}
}

// withFrontmatter prepends a tags frontmatter block unless content already
// starts with one or no tags were given.
func withFrontmatter(content string, tags []string) string {
	if len(tags) == 0 || strings.HasPrefix(content, "---") {
		return content
	}
	return "---\ntags: [" + strings.Join(tags, ", ") + "]\n---\n\n" + content
}

// cmdAdd creates a new file from stdin content, indexes it, and reports its
// path. It refuses to overwrite an existing file. Order matters: the
// existing-file check runs before stdin is read, so a bad invocation against
// an existing file fails fast without blocking on stdin.
func cmdAdd(args []string) int {
	fs := newFlagSet("add")
	db := dbFlag(fs)
	var tags strFlags
	fs.Var(&tags, "tag", "tag to add to the new file's frontmatter (repeatable)")
	if code, handled := parseArgs(fs, args); handled {
		return code
	}
	if fs.NArg() != 1 {
		return fail(fmt.Errorf("usage: ragrep add [--tag t]... <path>"))
	}
	path := fs.Arg(0)

	if _, err := os.Stat(path); err == nil {
		return fail(fmt.Errorf("%s already exists; edit it and run `ragrep index` instead", path))
	}
	wsRoot, err := workspaceRoot(*db)
	if err != nil {
		return fail(err)
	}
	key, err := normPath(path, wsRoot)
	if err != nil {
		return fail(err)
	}

	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fail(err)
	}
	if len(data) == 0 {
		return fail(fmt.Errorf("no content on stdin"))
	}

	content := withFrontmatter(string(data), []string(tags))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fail(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fail(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		return fail(err)
	}

	s, err := openStoreAt(*db)
	if err != nil {
		return fail(err)
	}
	defer s.Close()
	dir, err := embed.CacheDir()
	if err != nil {
		return fail(err)
	}
	e, err := embed.New(dir)
	if err != nil {
		return fail(err)
	}
	defer e.Close()

	if _, err := s.UpsertDoc(key, content, info.ModTime().Unix(), e.Embed); err != nil {
		return fail(err)
	}
	fmt.Println("indexed", key)
	return 0
}

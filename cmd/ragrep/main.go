package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/siroio/ragrep/internal/embed"
	"github.com/siroio/ragrep/internal/store"
)

const usage = `ragrep - adaptive retrieval unit search CLI

Usage:
  ragrep init                              create DB, download model assets
  ragrep index <path>...                   index text files (recursive)
  ragrep search <query> [--mode hybrid|vector|text] [-k 10] [--json]
  ragrep get <path> [--para N] [--context N] [--lines A-B]

Flags common to all commands:
  --db PATH    index database (default $RAGREP_DB, else .ragrep/index.db)

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
	default:
		fmt.Fprint(os.Stderr, usage)
		return 1
	}
}

func dbFlag(fs *flag.FlagSet) *string {
	return fs.String("db", defaultDBPath(), "index database path")
}

// defaultDBPath resolves the --db default: $RAGREP_DB if set, else the
// nearest existing .ragrep/index.db walking up from cwd (so running from a
// subdirectory of an indexed repo finds the root index), else
// .ragrep/index.db in cwd. DB keys are absolute (see normPath), so any
// command works from any subdirectory once the DB is found.
func defaultDBPath() string {
	if p := os.Getenv("RAGREP_DB"); p != "" {
		return p
	}
	local := filepath.Join(".ragrep", "index.db")
	dir, err := os.Getwd()
	if err != nil {
		return local
	}
	for {
		p := filepath.Join(dir, ".ragrep", "index.db")
		if _, err := os.Stat(p); err == nil {
			return p
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
	err := fs.Parse(args)
	if err == nil {
		return 0, false
	}
	if err == flag.ErrHelp {
		fmt.Fprint(os.Stderr, usage)
		return 0, true
	}
	return fail(err), true
}

func openStoreAt(path string) (*store.Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return store.Open(path)
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
	if code, handled := parseArgs(fset, args); handled {
		return code
	}
	if fset.NArg() == 0 {
		return fail(fmt.Errorf("usage: ragrep index <path>..."))
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

	indexed, skipped := 0, 0
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
			info, err := d.Info()
			if err != nil || info.Size() > maxFileSize || !isTextFile(path) {
				skipped++
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, err := normPath(path)
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
		roots := make([]string, len(fset.Args()))
		for i, r := range fset.Args() {
			root, err := normPath(r)
			if err != nil {
				return fail(err)
			}
			roots[i] = root
		}
		paths, err := s.ListPaths()
		if err != nil {
			return fail(err)
		}
		for _, p := range paths {
			underRoot := false
			for _, root := range roots {
				if p == root || strings.HasPrefix(p, root+"/") {
					underRoot = true
					break
				}
			}
			if !underRoot {
				continue
			}
			_, statErr := os.Stat(filepath.FromSlash(p))
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
		fmt.Printf("done: %d indexed, %d skipped, %d pruned\n", indexed, skipped, pruned)
	} else {
		fmt.Printf("done: %d indexed, %d skipped\n", indexed, skipped)
	}
	return 0
}

func cmdSearch(args []string) int {
	fs := newFlagSet("search")
	db := dbFlag(fs)
	mode := fs.String("mode", "hybrid", "hybrid|vector|text")
	k := fs.Int("k", 10, "max results")
	asJSON := fs.Bool("json", false, "JSON output")
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
		hits, err = s.SearchText(query, *k, nil)
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
			hits, err = s.SearchVector(qv, *k, nil)
		} else {
			hits, err = s.SearchHybrid(query, qv, *k, nil)
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
			fmt.Printf("%s#%d (lines %s, score %.4f)\n  %s\n", h.Doc, h.Para, h.Lines, h.Score, h.Snippet)
		}
	}
	return 0
}

// normPath converts p to the canonical DB key form: absolute, slash-separated.
// One canonical form means index/get/prune agree no matter what cwd or path
// style (relative, ./x, absolute) the user passes.
// ponytail: no case-folding or symlink resolution; on Windows "D:" vs "d:"
// still mismatch — add EqualFold/EvalSymlinks here if that ever bites.
func normPath(p string) (string, error) {
	a, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(a), nil
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
	// Normalize the arg to the canonical absolute slash form used as DB keys,
	// so relative, ./x, and absolute arguments all match the stored key.
	path, err := normPath(fs.Arg(0))
	if err != nil {
		return fail(err)
	}

	s, err := openStoreAt(*db)
	if err != nil {
		return fail(err)
	}
	defer s.Close()

	out, err := getContent(s, path, *lines, *para, *context)
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

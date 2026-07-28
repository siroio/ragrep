package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const usage = `rag - adaptive retrieval unit search CLI

Usage:
  rag init                              create DB, download model assets
  rag index <path>...                   index text files (recursive)
  rag search <query> [--mode hybrid|vector|text] [-k 10] [--json]
  rag get <path> [--para N] [--context N] [--lines A-B]

Flags common to all commands:
  --db PATH    index database (default .rag/index.db)

Exit codes: 0 success, 1 error, 2 no hits / not found
`

func main() {
	os.Exit(run(os.Args[1:]))
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
	return fs.String("db", filepath.Join(".rag", "index.db"), "index database path")
}

// newFlagSet builds a FlagSet that reports parse errors to the caller
// (ContinueOnError) instead of exiting the process with flag's own exit code
// 2 — that code collides with this CLI's "no hits / not found" contract.
// Output is discarded because fail(err) below prints the message instead,
// so a bad flag doesn't get printed twice.
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

func openStoreAt(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return openStore(path)
}

func cmdInit(args []string) int {
	fs := newFlagSet("init")
	db := dbFlag(fs)
	if err := fs.Parse(args); err != nil {
		return fail(err)
	}
	dir, err := cacheDir()
	if err != nil {
		return fail(err)
	}
	if err := ensureAssets(dir); err != nil {
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

func cmdIndex(args []string) int {
	fset := newFlagSet("index")
	db := dbFlag(fset)
	if err := fset.Parse(args); err != nil {
		return fail(err)
	}
	if fset.NArg() == 0 {
		return fail(fmt.Errorf("usage: rag index <path>..."))
	}
	s, err := openStoreAt(*db)
	if err != nil {
		return fail(err)
	}
	defer s.Close()
	dir, err := cacheDir()
	if err != nil {
		return fail(err)
	}
	e, err := newEmbedder(dir)
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
			rel := filepath.ToSlash(path)
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
	fmt.Printf("done: %d indexed, %d skipped\n", indexed, skipped)
	return 0
}

func cmdSearch(args []string) int {
	fs := newFlagSet("search")
	db := dbFlag(fs)
	mode := fs.String("mode", "hybrid", "hybrid|vector|text")
	k := fs.Int("k", 10, "max results")
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		return fail(err)
	}
	if fs.NArg() != 1 {
		return fail(fmt.Errorf("usage: rag search <query>"))
	}
	query := fs.Arg(0)

	s, err := openStoreAt(*db)
	if err != nil {
		return fail(err)
	}
	defer s.Close()

	var hits []Hit
	switch *mode {
	case "text":
		hits, err = s.SearchText(query, *k)
	case "vector", "hybrid":
		dir, derr := cacheDir()
		if derr != nil {
			return fail(derr)
		}
		e, eerr := newEmbedder(dir)
		if eerr != nil {
			return fail(eerr)
		}
		defer e.Close()
		qv, verr := e.Embed("task: search result | query: " + query)
		if verr != nil {
			return fail(verr)
		}
		if *mode == "vector" {
			hits, err = s.SearchVector(qv, *k)
		} else {
			hits, err = s.SearchHybrid(query, qv, *k)
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

func cmdGet(args []string) int {
	fs := newFlagSet("get")
	db := dbFlag(fs)
	para := fs.Int("para", -1, "paragraph number (from search results)")
	context := fs.Int("context", 0, "±N paragraphs around --para")
	lines := fs.String("lines", "", "line range A-B")
	if err := fs.Parse(args); err != nil {
		return fail(err)
	}
	if fs.NArg() != 1 {
		return fail(fmt.Errorf("usage: rag get <path>"))
	}
	// Normalize like cmdIndex's filepath.ToSlash(path) at index time, so a
	// Windows-style "docs\auth.md" argument still matches the "docs/auth.md"
	// key stored in the DB.
	path := filepath.ToSlash(fs.Arg(0))

	s, err := openStoreAt(*db)
	if err != nil {
		return fail(err)
	}
	defer s.Close()

	out, err := getContent(s, path, *lines, *para, *context)
	if err == errNotFound {
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
func getContent(s *Store, path, lines string, para, context int) (string, error) {
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
			return "", errNotFound
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

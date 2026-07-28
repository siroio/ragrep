package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/siroio/ragrep/internal/codeindex"
	"github.com/siroio/ragrep/internal/codestore"
	"github.com/siroio/ragrep/internal/config"
	"github.com/siroio/ragrep/internal/embed"
	"github.com/siroio/ragrep/internal/lsp"
)

const codeUsage = `ragrep code - code symbol indexing/search (uses code.db, never the document index.db)

Usage:
  ragrep code index --language go <path>...        index symbols via the configured language server
  ragrep code search [--json] [-k N] <query>        search indexed symbols (no body in the output)
  ragrep code get --symbol <key> [--body] [--json]  fetch one symbol's metadata (and body with --body)

Flags common to code subcommands:
  --db PATH    code database (default .ragrep/config.json code_db, else .ragrep/code.db)
`

// codeModelID identifies the embedding model recorded in code.db's metadata
// (see codestore.Open). internal/embed exposes no model-identity string of
// its own, so this names the model it pins (see internal/embed's
// repoBase/modelURL).
const codeModelID = "embeddinggemma-300m"

// codeEmbedDim mirrors internal/embed's private embedDim (768). The two
// packages don't import each other -- internal/store already duplicates the
// same constant for the same reason (see internal/store/store.go).
const codeEmbedDim = 768

// codeLangExt maps a --language value to the file extension code index
// enumerates. Only "go" is wired up in this task; a second language needs an
// entry here plus its own codeindex extractor.
var codeLangExt = map[string]string{"go": ".go"}

func cmdCode(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, codeUsage)
		return 1
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "index":
		return cmdCodeIndex(rest)
	case "search":
		return cmdCodeSearch(rest)
	case "get":
		return cmdCodeGet(rest)
	case "-h", "--help":
		fmt.Fprint(os.Stderr, codeUsage)
		return 0
	default:
		fmt.Fprint(os.Stderr, codeUsage)
		return 1
	}
}

// defaultCodeDBPath mirrors defaultDBPath's walk-up-to-.ragrep discovery,
// but resolves config.Config.CodeDB instead of DB -- code subcommands never
// open the document index.db (see codeDBFlag).
func defaultCodeDBPath() string {
	local := filepath.FromSlash(config.DefaultCodeDB)
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
			return filepath.Join(dir, filepath.FromSlash(cfg.CodeDB))
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return local
		}
		dir = parent
	}
}

func codeDBFlag(fs *flag.FlagSet) *string {
	return fs.String("db", defaultCodeDBPath(), "code index database path")
}

func openCodeStoreAt(dbPath string) (*codestore.Store, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, err
	}
	return codestore.Open(dbPath, codeModelID, codeEmbedDim)
}

// codeIndexSkipDirs are directory names never descended into: dependency
// trees (vendor, node_modules) and the common build-output/artifact
// directory names across ecosystems (dist, build, bin, out, target, obj) --
// the binding constraint excludes "vendor/生成物/依存パッケージ/ビルド成果物" by
// default. Directories starting with "." are skipped separately (see
// discoverCodeFiles), not listed here.
var codeIndexSkipDirs = map[string]bool{
	"vendor": true, "node_modules": true,
	"dist": true, "build": true, "bin": true, "out": true, "target": true, "obj": true,
}

// generatedCodeRE matches the standard Go generated-code marker line
// (https://go.dev/s/generatedcode): a comment exactly "// Code generated
// ... DO NOT EDIT." appearing before the package clause.
var generatedCodeRE = regexp.MustCompile(`^// Code generated .* DO NOT EDIT\.$`)

// isGeneratedGoFile reports whether path's header marks it as generated Go
// code. Only the first 5 lines are checked, stopping early at the first
// non-comment line -- the marker is always right at the top of the file in
// practice (before the package clause), so scanning further would only cost
// time on every ordinary file.
func isGeneratedGoFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for i := 0; i < 5 && sc.Scan(); i++ {
		line := sc.Text()
		if !strings.HasPrefix(line, "//") {
			return false
		}
		if generatedCodeRE.MatchString(line) {
			return true
		}
	}
	return false
}

// discoverCodeFiles enumerates every file under root whose extension is ext,
// deterministically: directories in codeIndexSkipDirs, and any directory
// (other than root itself) starting with ".", are skipped; for Go, a file
// whose header marks it as generated (see isGeneratedGoFile) is also
// skipped. The result is sorted. Test files (e.g. _test.go) and testdata/
// are included -- both are needed for later relation extraction
// (tests-relation).
func discoverCodeFiles(root, ext string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if p != root && (strings.HasPrefix(name, ".") || codeIndexSkipDirs[name]) {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(name) != ext {
			return nil
		}
		if ext == ".go" && isGeneratedGoFile(p) {
			return nil
		}
		files = append(files, p)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}

// fileURI converts an on-disk path into a file:// URI understood by the
// language server.
func fileURI(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	slashed := filepath.ToSlash(abs)
	if !strings.HasPrefix(slashed, "/") {
		slashed = "/" + slashed // Windows drive-letter paths (C:/...) need a leading slash
	}
	return "file://" + slashed
}

// gitRevision returns `git rev-parse HEAD` run in wsRoot, or "unknown" if
// git isn't available or wsRoot isn't a git checkout -- index_runs.revision
// is provenance, not a hard requirement.
func gitRevision(wsRoot string) string {
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = wsRoot
	out, err := cmd.Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

// startLanguageServer launches serverCmd and drives it through
// initialize/initialized. The caller must Close the returned client.
func startLanguageServer(serverCmd, wsRoot string) (*lsp.Client, *lsp.InitializeResult, error) {
	client, err := lsp.Start(serverCmd, nil, lsp.WithDir(wsRoot))
	if err != nil {
		return nil, nil, fmt.Errorf("starting language server %q: %w", serverCmd, err)
	}
	pid := os.Getpid()
	result, err := client.Initialize(context.Background(), lsp.InitializeParams{
		ProcessID:    &pid,
		RootURI:      fileURI(wsRoot),
		Capabilities: lsp.DefaultClientCapabilities(),
	})
	if err != nil {
		client.Close()
		return nil, nil, fmt.Errorf("initialize: %w", err)
	}
	if err := client.Initialized(); err != nil {
		client.Close()
		return nil, nil, fmt.Errorf("initialized: %w", err)
	}
	return client, result, nil
}

// recordIndexRun inserts one index_runs row directly against the code.db
// file. codestore has no exported API for this table (only ReplaceRelations
// takes a runID, for symbol_edges) -- wiring symbols.index_run_id itself is
// deferred to a later task; this only records the run's own provenance
// (revision, server, model, time), reusing the sqlite3 driver already
// registered by internal/codestore's blank imports.
func recordIndexRun(dbPath, language, revision, serverName, serverVersion, modelID string, roots []string) error {
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec(`
		INSERT INTO index_runs(scope, revision, language, server_name, server_version, model_id, created_at)
		VALUES (?,?,?,?,?,?,?)`,
		strings.Join(roots, ","), revision, language, serverName, serverVersion, modelID,
		time.Now().UTC().Format(time.RFC3339))
	return err
}

func cmdCodeIndex(args []string) int {
	fs := newFlagSet("code index")
	db := codeDBFlag(fs)
	language := fs.String("language", "", "language server registration key (e.g. go)")
	if code, handled := parseArgs(fs, args); handled {
		return code
	}
	if *language == "" || fs.NArg() == 0 {
		return fail(fmt.Errorf("usage: ragrep code index --language <lang> <path>..."))
	}
	ext, ok := codeLangExt[*language]
	if !ok {
		return fail(fmt.Errorf("unsupported --language %q (supported: go)", *language))
	}

	wsRoot, err := workspaceRoot(*db)
	if err != nil {
		return fail(err)
	}

	// Validate every root arg is inside the workspace UP FRONT, mirroring
	// cmdIndex's doc-index rationale: fail before resolving the server or
	// touching the (slow) embedding model, not partway through a walk.
	roots := fs.Args()
	relRoots := make([]string, len(roots))
	for i, r := range roots {
		rel, err := normPath(r, wsRoot)
		if err != nil {
			return fail(err)
		}
		relRoots[i] = rel
	}

	cfg, err := config.Load(wsRoot)
	if err != nil {
		return fail(err)
	}
	serverCmd, err := cfg.ServerCommand(*language)
	if err != nil {
		return fail(err)
	}

	var files []string
	for _, r := range roots {
		found, err := discoverCodeFiles(r, ext)
		if err != nil {
			return fail(err)
		}
		files = append(files, found...)
	}

	if len(files) == 0 {
		fmt.Println("done: 0 indexed (0 files scanned)")
		return 0
	}

	// Start the server and gate on its advertised capabilities BEFORE paying
	// for the codestore/embedding-model setup below: a server that can't do
	// textDocument/documentSymbol (or isn't the executable we expect) should
	// fail fast, the same "cheap/critical checks before expensive ones"
	// rationale as the workspace-root validation above.
	client, initResult, err := startLanguageServer(serverCmd, wsRoot)
	if err != nil {
		return fail(err)
	}
	defer client.Close()
	if !client.Supports(lsp.FeatureDocumentSymbol) {
		return fail(fmt.Errorf("language server %q does not support textDocument/documentSymbol (required relation: document symbols for indexing)", serverCmd))
	}

	s, err := openCodeStoreAt(*db)
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

	indexed := 0
	ctx := context.Background()
	for _, f := range files {
		rel, err := normPath(f, wsRoot)
		if err != nil {
			return fail(err)
		}
		content, err := os.ReadFile(f)
		if err != nil {
			return fail(err)
		}
		docSyms, err := client.DocumentSymbol(ctx, lsp.DocumentSymbolParams{
			TextDocument: lsp.TextDocumentIdentifier{URI: fileURI(f)},
		})
		if err != nil {
			return fail(fmt.Errorf("%s: %w", rel, err))
		}
		symbols, err := codeindex.Extract(*language, rel, content, docSyms)
		if err != nil {
			return fail(err)
		}
		changed, err := s.UpsertSymbols(rel, codeindex.FileHash(content), symbols, e.Embed)
		if err != nil {
			return fail(fmt.Errorf("%s: %w", rel, err))
		}
		if changed {
			fmt.Println("indexed", rel)
			indexed++
		}
	}

	serverName, serverVersion := "unknown", "unknown"
	if initResult != nil && initResult.ServerInfo != nil {
		serverName = initResult.ServerInfo.Name
		if initResult.ServerInfo.Version != "" {
			serverVersion = initResult.ServerInfo.Version
		}
	}
	if err := recordIndexRun(*db, *language, gitRevision(wsRoot), serverName, serverVersion, codeModelID, relRoots); err != nil {
		return fail(err)
	}

	fmt.Printf("done: %d indexed (%d files scanned)\n", indexed, len(files))
	return 0
}

// formatCodeSearchHits writes hits as either one line per hit (key, kind,
// qualified name, signature, path:startLine-endLine, score breakdown) or, if
// asJSON, a JSON array of the full SymbolHit values. Neither form includes a
// symbol's body -- search is for locating symbols, use `code get --body` to
// read one.
func formatCodeSearchHits(w io.Writer, hits []codestore.SymbolHit, asJSON bool) error {
	if asJSON {
		return json.NewEncoder(w).Encode(hits)
	}
	for _, h := range hits {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s:%d-%d\tscore=%.4f (fts=%d vec=%d exact=%v)\n",
			h.Key, h.Kind, h.QualifiedName, h.Signature, h.Path, h.StartLine, h.EndLine,
			h.Score, h.FTSRank, h.VecRank, h.ExactMatch)
	}
	return nil
}

func cmdCodeSearch(args []string) int {
	fs := newFlagSet("code search")
	db := codeDBFlag(fs)
	k := fs.Int("k", 10, "max results")
	asJSON := fs.Bool("json", false, "JSON output")
	if code, handled := parseArgs(fs, args); handled {
		return code
	}
	if fs.NArg() != 1 {
		return fail(fmt.Errorf("usage: ragrep code search [--json] [-k N] <query>"))
	}
	query := fs.Arg(0)

	s, err := openCodeStoreAt(*db)
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

	qv, err := e.Embed(query)
	if err != nil {
		return fail(err)
	}

	hits, err := s.SearchSymbolsHybrid(query, qv, *k)
	if err != nil {
		return fail(err)
	}
	if len(hits) == 0 {
		fmt.Fprintln(os.Stderr, "no hits")
		return 2
	}
	if err := formatCodeSearchHits(os.Stdout, hits, *asJSON); err != nil {
		return fail(err)
	}
	return 0
}

// codeSymbolOutput is `code get`'s output shape: metadata always, Body only
// when --body was given (omitempty keeps it out of the JSON entirely rather
// than emitting `"body":""`).
type codeSymbolOutput struct {
	Key           string `json:"key"`
	Language      string `json:"language"`
	Kind          string `json:"kind"`
	Name          string `json:"name"`
	QualifiedName string `json:"qualified_name"`
	Signature     string `json:"signature"`
	Documentation string `json:"documentation,omitempty"`
	Container     string `json:"container,omitempty"`
	Path          string `json:"path"`
	StartLine     int    `json:"start_line"`
	StartChar     int    `json:"start_character"`
	EndLine       int    `json:"end_line"`
	EndChar       int    `json:"end_character"`
	Body          string `json:"body,omitempty"`
}

func formatCodeSymbol(w io.Writer, sym codeindex.Symbol, includeBody, asJSON bool) error {
	out := codeSymbolOutput{
		Key: sym.Key, Language: sym.Language, Kind: sym.Kind, Name: sym.Name,
		QualifiedName: sym.QualifiedName, Signature: sym.Signature, Documentation: sym.Documentation,
		Container: sym.Container, Path: sym.Path,
		StartLine: sym.Range.Start.Line, StartChar: sym.Range.Start.Character,
		EndLine: sym.Range.End.Line, EndChar: sym.Range.End.Character,
	}
	if includeBody {
		out.Body = sym.Body
	}
	if asJSON {
		return json.NewEncoder(w).Encode(out)
	}
	fmt.Fprintf(w, "%s\n  kind: %s\n  name: %s\n  qualified_name: %s\n  signature: %s\n  path: %s:%d-%d\n",
		out.Key, out.Kind, out.Name, out.QualifiedName, out.Signature, out.Path, out.StartLine, out.EndLine)
	if out.Documentation != "" {
		fmt.Fprintf(w, "  documentation: %s\n", out.Documentation)
	}
	if includeBody {
		fmt.Fprintf(w, "  body:\n%s\n", out.Body)
	}
	return nil
}

func cmdCodeGet(args []string) int {
	fs := newFlagSet("code get")
	db := codeDBFlag(fs)
	symbolKey := fs.String("symbol", "", "stable symbol key (from `code search`)")
	body := fs.Bool("body", false, "include the symbol's body text")
	asJSON := fs.Bool("json", false, "JSON output")
	if code, handled := parseArgs(fs, args); handled {
		return code
	}
	if fs.NArg() != 0 || *symbolKey == "" {
		return fail(fmt.Errorf("usage: ragrep code get --symbol <stable-key> [--body] [--json]"))
	}

	s, err := openCodeStoreAt(*db)
	if err != nil {
		return fail(err)
	}
	defer s.Close()

	sym, err := s.GetSymbol(*symbolKey)
	if err == codestore.ErrNotFound {
		fmt.Fprintln(os.Stderr, "not found")
		return 2
	}
	if err != nil {
		return fail(err)
	}

	if err := formatCodeSymbol(os.Stdout, sym, *body, *asJSON); err != nil {
		return fail(err)
	}
	return 0
}

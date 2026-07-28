package main

import (
	"bufio"
	"context"
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
	"unicode/utf16"

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
  ragrep code expand --symbol <key> --relation definition|references|callers|callees|tests [--json]
                                                     live-query the language server for one symbol's
                                                     relations (1 hop), save them, print resolved targets

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
	case "expand":
		return cmdCodeExpand(rest)
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

// serverIdentity extracts the server's self-reported name/version from an
// initialize response ("unknown"/"unknown" when the server didn't advertise
// serverInfo, or initResult itself is nil), for index_runs.server_name /
// server_version and (in cmdCodeExpand) symbol_edges.source.
func serverIdentity(initResult *lsp.InitializeResult) (name, version string) {
	name, version = "unknown", "unknown"
	if initResult != nil && initResult.ServerInfo != nil {
		name = initResult.ServerInfo.Name
		if initResult.ServerInfo.Version != "" {
			version = initResult.ServerInfo.Version
		}
	}
	return name, version
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

	// Record the run BEFORE indexing any file, not after: UpsertSymbols
	// needs a real runID to stamp into symbols.index_run_id as it goes, so
	// the row's own id must exist first.
	serverName, serverVersion := serverIdentity(initResult)
	runID, err := s.RecordIndexRun(strings.Join(relRoots, ","), gitRevision(wsRoot), *language, serverName, serverVersion, codeModelID, time.Now())
	if err != nil {
		return fail(err)
	}

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
		changed, err := s.UpsertSymbols(rel, codeindex.FileHash(content), symbols, runID, e.Embed)
		if err != nil {
			return fail(fmt.Errorf("%s: %w", rel, err))
		}
		if changed {
			fmt.Println("indexed", rel)
			indexed++
		}
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

// codeExpandFeature maps a --relation value to the LSP capability that must
// be advertised to serve it: "tests" reuses textDocument/references (a test
// relation is just a references result whose path happens to end in
// _test.go -- see codeindex.ReferenceRelations), "callers"/"callees" both
// need call hierarchy (prepareCallHierarchy, then one of
// incoming/outgoingCalls).
var codeExpandFeature = map[string]string{
	"definition": lsp.FeatureDefinition,
	"references": lsp.FeatureReferences,
	"tests":      lsp.FeatureReferences,
	"callers":    lsp.FeatureCallHierarchy,
	"callees":    lsp.FeatureCallHierarchy,
}

// codeExpandReplaceGroup maps a --relation value to the full set of
// symbol_edges kinds that ONE query for it can produce -- and therefore the
// kinds codestore.ReplaceRelations must clear together, so it deletes stale
// edges from a symbol's own earlier expand calls without also deleting a
// DIFFERENT kind's edges from a different expand call it didn't just touch.
// "references" and "tests" are always one group: both come from a single
// textDocument/references query (see codeindex.ReferenceRelations), so
// `--relation references` and `--relation tests` must each clear (and can
// each produce) both kinds -- otherwise a `references` expand that finds
// zero non-test hits would leave a stale "tests" edge from an earlier run
// stranded forever, and vice versa. "definition", "callers", "callees" are
// each their own one-kind group.
var codeExpandReplaceGroup = map[string][]string{
	"definition": {"definition"},
	"references": {"references", "tests"},
	"tests":      {"references", "tests"},
	"callers":    {"callers"},
	"callees":    {"callees"},
}

// declarationPosition locates sym's own name token within its declaration,
// scanning the source lines sym.Range covers for the first whole-word
// occurrence of the last dot-separated segment of sym.Name (gopls reports a
// Go method's Name as "(Receiver).Method"; splitting on "." isolates
// "Method"). This matters because sym.Range.Start -- a documentSymbol's
// overall range, not the narrower selectionRange that would point straight
// at the identifier; codeindex.Symbol doesn't carry that -- usually lands on
// the "func"/"type"/... keyword instead of the name, and several LSP
// requests this command drives (prepareCallHierarchy in particular) only
// resolve when the position is actually on the identifier. Falls back to
// sym.Range.Start (best-effort, not a hard failure) when content is empty
// or the name can't be found on any of its declaration lines.
func declarationPosition(content []byte, sym codeindex.Symbol) lsp.Position {
	fallback := lsp.Position{Line: sym.Range.Start.Line, Character: sym.Range.Start.Character}
	name := sym.Name
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:]
	}
	if name == "" {
		return fallback
	}

	lines := strings.Split(string(content), "\n")
	for lineNo := sym.Range.Start.Line; lineNo <= sym.Range.End.Line && lineNo < len(lines); lineNo++ {
		line := strings.TrimSuffix(lines[lineNo], "\r")
		if idx := indexWholeWord(line, name); idx >= 0 {
			return lsp.Position{Line: lineNo, Character: utf16Len(line[:idx])}
		}
	}
	return fallback
}

// indexWholeWord returns the byte offset of the first occurrence of word in
// line that isn't part of a larger identifier (e.g. "Greet" inside
// "Greeter" doesn't count), or -1 if there's none.
func indexWholeWord(line, word string) int {
	for start := 0; start <= len(line)-len(word); {
		idx := strings.Index(line[start:], word)
		if idx < 0 {
			return -1
		}
		idx += start
		before, after := byte(0), byte(0)
		if idx > 0 {
			before = line[idx-1]
		}
		if idx+len(word) < len(line) {
			after = line[idx+len(word)]
		}
		if !isIdentByte(before) && !isIdentByte(after) {
			return idx
		}
		start = idx + 1
	}
	return -1
}

func isIdentByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// utf16Len is the number of UTF-16 code units s encodes to -- the unit LSP
// Position.Character counts in, matching codeindex's own UTF-16 handling
// (see internal/codeindex/extract.go's lineIndex).
func utf16Len(s string) int {
	return len(utf16.Encode([]rune(s)))
}

// pathFromURI converts a file:// URI (as returned by a language server)
// back into a display path: workspace-relative and slash-separated (the
// same convention as codeindex.Symbol.Path) when it's inside wsRoot, or the
// absolute path as-is when it isn't (e.g. a stdlib location under GOROOT) --
// still informative even though it won't match anything codestore.SymbolAt
// can resolve. Mirrors fileURI's construction in reverse.
func pathFromURI(uri, wsRoot string) string {
	p := strings.TrimPrefix(uri, "file://")
	if len(p) >= 3 && p[0] == '/' && p[2] == ':' {
		p = p[1:] // strip the leading "/" fileURI adds before a Windows drive letter
	}
	abs := filepath.FromSlash(p)
	if rel, err := filepath.Rel(wsRoot, abs); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(abs)
}

// locsFromLSP converts textDocument/definition or textDocument/references
// results into codeindex.Loc, anchored at each location's Range.Start.
func locsFromLSP(wsRoot string, locs []lsp.Location) []codeindex.Loc {
	out := make([]codeindex.Loc, 0, len(locs))
	for _, l := range locs {
		out = append(out, codeindex.Loc{
			Path:     pathFromURI(l.URI, wsRoot),
			Position: codeindex.Position{Line: l.Range.Start.Line, Character: l.Range.Start.Character},
		})
	}
	return out
}

// locsFromCallHierarchyItems converts callHierarchy/incomingCalls' "From"
// items or callHierarchy/outgoingCalls' "To" items into codeindex.Loc,
// anchored at each item's SelectionRange.Start (the identifier itself,
// narrower and more precise than Range, which spans the whole declaration).
func locsFromCallHierarchyItems(wsRoot string, items []lsp.CallHierarchyItem) []codeindex.Loc {
	out := make([]codeindex.Loc, 0, len(items))
	for _, it := range items {
		out = append(out, codeindex.Loc{
			Path:     pathFromURI(it.URI, wsRoot),
			Position: codeindex.Position{Line: it.SelectionRange.Start.Line, Character: it.SelectionRange.Start.Character},
		})
	}
	return out
}

// resolverFor adapts codestore.Store.SymbolAt (which can fail) to
// codeindex.Resolver (which can't): the first SymbolAt error is captured
// into *error and every resolve call after that short-circuits to
// unresolved, so the caller can check the pointer once after converting all
// locations instead of threading an error return through every relations.go
// helper.
func resolverFor(s *codestore.Store) (codeindex.Resolver, *error) {
	var firstErr error
	resolve := func(path string, line int) (string, bool) {
		if firstErr != nil {
			return "", false
		}
		key, ok, err := s.SymbolAt(path, line)
		if err != nil {
			firstErr = err
			return "", false
		}
		return key, ok
	}
	return resolve, &firstErr
}

// codeExpandTarget is one line of `code expand` output: either a resolved
// indexed symbol's declaration metadata, or (Resolved == false) the bare
// path/position of a location that didn't resolve to one -- never a
// fabricated key.
type codeExpandTarget struct {
	Relation      string `json:"relation"`
	Resolved      bool   `json:"resolved"`
	Key           string `json:"key,omitempty"`
	Kind          string `json:"kind,omitempty"`
	QualifiedName string `json:"qualified_name,omitempty"`
	Signature     string `json:"signature,omitempty"`
	Path          string `json:"path"`
	StartLine     int    `json:"start_line,omitempty"`
	EndLine       int    `json:"end_line,omitempty"`
	Line          int    `json:"line,omitempty"`      // unresolved only
	Character     int    `json:"character,omitempty"` // unresolved only
}

// expandTargets builds one codeExpandTarget per relation, in order,
// resolving each ToKey's declaration metadata via GetSymbol. Body is never
// included -- see formatCodeSymbol's doc comment; `code get --body` is the
// only way to read one.
func expandTargets(s *codestore.Store, relations []codeindex.Relation) ([]codeExpandTarget, error) {
	out := make([]codeExpandTarget, 0, len(relations))
	for _, r := range relations {
		if r.ToKey == "" {
			out = append(out, codeExpandTarget{
				Relation: r.Kind, Resolved: false,
				Path: r.ToPath, Line: r.ToPosition.Line, Character: r.ToPosition.Character,
			})
			continue
		}
		sym, err := s.GetSymbol(r.ToKey)
		if err != nil {
			return nil, fmt.Errorf("expand: resolved key %s: %w", r.ToKey, err)
		}
		out = append(out, codeExpandTarget{
			Relation: r.Kind, Resolved: true,
			Key: sym.Key, Kind: sym.Kind, QualifiedName: sym.QualifiedName, Signature: sym.Signature,
			Path: sym.Path, StartLine: sym.Range.Start.Line, EndLine: sym.Range.End.Line,
		})
	}
	return out, nil
}

func formatCodeExpandTargets(w io.Writer, targets []codeExpandTarget, asJSON bool) error {
	if asJSON {
		return json.NewEncoder(w).Encode(targets)
	}
	for _, t := range targets {
		if !t.Resolved {
			fmt.Fprintf(w, "%s\tunresolved\t%s:%d\n", t.Relation, t.Path, t.Line)
			continue
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s:%d-%d\n",
			t.Relation, t.Key, t.Kind, t.QualifiedName, t.Signature, t.Path, t.StartLine, t.EndLine)
	}
	return nil
}

func cmdCodeExpand(args []string) int {
	fs := newFlagSet("code expand")
	db := codeDBFlag(fs)
	symbolKey := fs.String("symbol", "", "stable symbol key (from `code search`)")
	relation := fs.String("relation", "", "definition|references|callers|callees|tests")
	asJSON := fs.Bool("json", false, "JSON output")
	if code, handled := parseArgs(fs, args); handled {
		return code
	}
	feature, validRelation := codeExpandFeature[*relation]
	if fs.NArg() != 0 || *symbolKey == "" || !validRelation {
		return fail(fmt.Errorf("usage: ragrep code expand --symbol <stable-key> --relation definition|references|callers|callees|tests [--json]"))
	}

	wsRoot, err := workspaceRoot(*db)
	if err != nil {
		return fail(err)
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

	cfg, err := config.Load(wsRoot)
	if err != nil {
		return fail(err)
	}
	serverCmd, err := cfg.ServerCommand(sym.Language)
	if err != nil {
		return fail(err)
	}

	client, initResult, err := startLanguageServer(serverCmd, wsRoot)
	if err != nil {
		return fail(err)
	}
	defer client.Close()

	// Distinct from "no results": the server itself can't do this, so no
	// query was even attempted.
	if !client.Supports(feature) {
		fmt.Fprintf(os.Stderr, "not supported by server %s\n", serverCmd)
		return 1
	}

	serverName, serverVersion := serverIdentity(initResult)
	resolve, resolveErr := resolverFor(s)
	absPath := filepath.Join(wsRoot, filepath.FromSlash(sym.Path))
	content, _ := os.ReadFile(absPath) // best-effort; declarationPosition falls back to sym.Range.Start if this fails or the name can't be found
	pos := lsp.TextDocumentPositionParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: fileURI(absPath)},
		Position:     declarationPosition(content, sym),
	}

	ctx := context.Background()
	var relations []codeindex.Relation
	switch *relation {
	case "definition":
		locs, err := client.Definition(ctx, lsp.DefinitionParams(pos))
		if err != nil {
			return fail(err)
		}
		relations = codeindex.DefinitionRelations(sym.Key, locsFromLSP(wsRoot, locs), serverName, resolve)

	case "references", "tests":
		locs, err := client.References(ctx, lsp.ReferenceParams{
			TextDocumentPositionParams: pos,
			Context:                    lsp.ReferenceContext{IncludeDeclaration: false},
		})
		if err != nil {
			return fail(err)
		}
		relations = codeindex.ReferenceRelations(sym.Key, locsFromLSP(wsRoot, locs), serverName, resolve)

	case "callers", "callees":
		items, err := client.PrepareCallHierarchy(ctx, lsp.CallHierarchyPrepareParams(pos))
		if err != nil {
			return fail(err)
		}
		if len(items) > 0 {
			// prepareCallHierarchy can return multiple candidate items when a
			// position is ambiguous; deliberately taking just the first
			// (items[0]) rather than querying every candidate keeps this to
			// one call each of Incoming/OutgoingCalls. In practice this
			// position always names exactly one indexed symbol's own
			// declaration (see declarationPosition), so ambiguity isn't
			// expected to bite -- revisit if a language server ever returns
			// more than one candidate here for a real symbol.
			//
			// 1 hop only: query the prepared item's calls exactly once, no
			// further traversal from the results.
			item := items[0]
			if *relation == "callers" {
				calls, err := client.IncomingCalls(ctx, lsp.CallHierarchyIncomingCallsParams{Item: item})
				if err != nil {
					return fail(err)
				}
				froms := make([]lsp.CallHierarchyItem, len(calls))
				for i, c := range calls {
					froms[i] = c.From
				}
				relations = codeindex.CallerRelations(sym.Key, locsFromCallHierarchyItems(wsRoot, froms), serverName, resolve)
			} else {
				calls, err := client.OutgoingCalls(ctx, lsp.CallHierarchyOutgoingCallsParams{Item: item})
				if err != nil {
					return fail(err)
				}
				tos := make([]lsp.CallHierarchyItem, len(calls))
				for i, c := range calls {
					tos[i] = c.To
				}
				relations = codeindex.CalleeRelations(sym.Key, locsFromCallHierarchyItems(wsRoot, tos), serverName, resolve)
			}
		}
	}

	if *resolveErr != nil {
		return fail(*resolveErr)
	}

	runID, err := s.RecordIndexRun(sym.Path, gitRevision(wsRoot), sym.Language, serverName, serverVersion, codeModelID, time.Now())
	if err != nil {
		return fail(err)
	}
	if err := s.ReplaceRelations(runID, sym.Key, codeExpandReplaceGroup[*relation], relations); err != nil {
		return fail(err)
	}

	// The requested relation only, even though references/tests share one
	// underlying LSP call and ReplaceRelations just saved both kinds.
	wantKind := *relation
	filtered := relations[:0:0]
	for _, r := range relations {
		if r.Kind == wantKind {
			filtered = append(filtered, r)
		}
	}

	if len(filtered) == 0 {
		fmt.Fprintln(os.Stderr, "no results")
		return 2
	}

	targets, err := expandTargets(s, filtered)
	if err != nil {
		return fail(err)
	}
	if err := formatCodeExpandTargets(os.Stdout, targets, *asJSON); err != nil {
		return fail(err)
	}
	return 0
}

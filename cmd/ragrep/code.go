package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
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
	"github.com/siroio/ragrep/internal/coderetrieval"
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
  ragrep code pack --query <q> [--select KEY]... [--budget N] [--json]
                                                     hybrid-search top-k candidates, optionally stage 1-3
                                                     symbols' full bodies, assemble a budgeted context pack
                                                     plus a stale-detectable manifest
  ragrep code verify --manifest FILE [--json]       check a code-pack manifest against the workspace and
                                                     the store: staleness (files changed) + re-resolution
                                                     (stable keys still valid)

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
	case "pack":
		return cmdCodePack(rest)
	case "verify":
		return cmdCodeVerify(rest)
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
			version = truncateServerVersion(initResult.ServerInfo.Version)
		}
	}
	return name, version
}

// maxServerVersionLen caps truncateServerVersion's output. Generous for any
// real semantic-version-ish string, tiny next to the multi-KB build-info
// blob it guards against (see truncateServerVersion).
const maxServerVersionLen = 120

// truncateServerVersion trims a server-reported version string down to
// something fit for a single index_runs/manifest field: gopls's
// serverInfo.version is a multi-line build-info blob (module path, main
// version, further "    dep version h1:hash" lines) rather than a short
// version string -- stored verbatim it's ~2.7KB duplicated into every
// index_runs row and every manifest. Only the first line is kept, further
// capped at maxServerVersionLen characters.
func truncateServerVersion(v string) string {
	if i := strings.IndexAny(v, "\r\n"); i >= 0 {
		v = v[:i]
	}
	if len(v) > maxServerVersionLen {
		v = v[:maxServerVersionLen]
	}
	return v
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
		// Still prune: every file that used to be under one of these roots
		// may have been deleted, including the last one -- that must not
		// require the server or embedding model either (see
		// TestCmdCodeIndexZeroFilesSkipsServerAndModel).
		s, err := openCodeStoreAt(*db)
		if err != nil {
			return fail(err)
		}
		defer s.Close()
		pruned, err := pruneCodeSymbols(s, relRoots, nil)
		if err != nil {
			return fail(err)
		}
		fmt.Printf("done: 0 indexed (0 files scanned, %d pruned)\n", pruned)
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
	seen := make(map[string]bool, len(files))
	ctx := context.Background()
	for _, f := range files {
		rel, err := normPath(f, wsRoot)
		if err != nil {
			return fail(err)
		}
		seen[rel] = true
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

	pruned, err := pruneCodeSymbols(s, relRoots, seen)
	if err != nil {
		return fail(err)
	}

	fmt.Printf("done: %d indexed (%d files scanned, %d pruned)\n", indexed, len(files), pruned)
	return 0
}

// pruneCodeSymbols removes every stored symbol whose path is under one of
// relRoots but wasn't seen this run (seen holds the workspace-relative
// paths this run's discoverCodeFiles walk actually found, same convention
// as relRoots) -- `code index`'s default pruning pass for files that were
// deleted, or moved out of an indexed root, since the last index. Returns
// the number of distinct paths pruned; each is also printed, mirroring the
// doc index's own --prune output.
func pruneCodeSymbols(s *codestore.Store, relRoots []string, seen map[string]bool) (int, error) {
	allPaths, err := s.ListPaths()
	if err != nil {
		return 0, err
	}
	pruned := 0
	for _, p := range allPaths {
		if seen[p] || !pathUnderAnyRoot(p, relRoots) {
			continue
		}
		if err := s.DeleteSymbolsForPath(p); err != nil {
			return pruned, err
		}
		fmt.Println("pruned", p)
		pruned++
	}
	return pruned, nil
}

// pathUnderAnyRoot reports whether p (a workspace-relative path) falls under
// any of roots (also workspace-relative), mirroring cmdIndex's own --prune
// containment check: root == "." matches everything, otherwise p must equal
// root or start with "root/".
func pathUnderAnyRoot(p string, roots []string) bool {
	for _, root := range roots {
		if root == "." || p == root || strings.HasPrefix(p, root+"/") {
			return true
		}
	}
	return false
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
	// Only resolved, deduped relations are persisted -- an unresolved
	// location (ToKey=="") has no symbol_edges columns to hold it, and an
	// LSP query naturally returns one location per reference/call site, so a
	// symbol referenced or called twice from the same enclosing symbol would
	// otherwise collide on symbol_edges' UNIQUE(from_key, to_key, kind,
	// source) constraint. See codeindex.DedupResolvedRelations.
	toPersist := codeindex.DedupResolvedRelations(relations)
	if err := s.ReplaceRelations(runID, sym.Key, codeExpandReplaceGroup[*relation], toPersist); err != nil {
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

// codePackDefaultBudget is `code pack`'s default --budget: the max total
// JSON-encoded size, in characters, of everything staged into the pack
// (candidates + selected symbol bodies + their relations -- see
// coderetrieval.AssembleOptions.Budget). Large enough for several full
// symbol bodies plus their candidate metadata without unboundedly growing
// an LLM caller's context by default.
const codePackDefaultBudget = 20000

// codePackOutput is `code pack`'s JSON shape: the assembled context pack
// plus a stale-detectable manifest describing what it drew on, ready to
// write straight to a file for a later `code verify --manifest`.
type codePackOutput struct {
	Pack     coderetrieval.ContextPack `json:"pack"`
	Manifest coderetrieval.Manifest    `json:"manifest"`
}

// buildManifest assembles a coderetrieval.Manifest from pack's actually
// included symbol bodies: index identity (revision/server/model) from the
// most recently recorded index_runs row, plus one SymbolRef per included
// symbol using its store-recorded file_hash (see
// (*codestore.Store).SymbolFileHash's doc comment for why that's preferred
// over re-hashing the file from disk here). A store with no recorded index
// run yet (LatestIndexRun -> ErrNotFound) still produces a manifest, just
// with empty identity fields -- that's provenance, not a hard requirement
// (mirrors gitRevision's "unknown" fallback for the document/code indexes).
func buildManifest(s *codestore.Store, pack coderetrieval.ContextPack) (coderetrieval.Manifest, error) {
	run, err := s.LatestIndexRun()
	if err != nil && !errors.Is(err, codestore.ErrNotFound) {
		return coderetrieval.Manifest{}, err
	}

	m := coderetrieval.Manifest{
		IndexRevision: run.Revision,
		ServerName:    run.ServerName,
		ServerVersion: run.ServerVersion,
		ModelID:       run.ModelID,
	}
	for _, sym := range pack.Symbols {
		hash, err := s.SymbolFileHash(sym.Key)
		if err != nil {
			return coderetrieval.Manifest{}, fmt.Errorf("manifest: file hash for %q: %w", sym.Key, err)
		}
		m.Symbols = append(m.Symbols, coderetrieval.SymbolRef{
			Key: sym.Key, QualifiedName: sym.QualifiedName, Path: sym.Path,
			StartLine: sym.Range.Start.Line, EndLine: sym.Range.End.Line, FileHash: hash,
		})
	}
	return m, nil
}

// runCodePack is `code pack`'s core logic: hybrid search, then
// coderetrieval.BuildContextPack, then buildManifest. Split out from
// cmdCodePack (which resolves qv via the real embedding model) so tests can
// drive it with a fake query vector, the same way TestFormatCodeSearchHits
// drives formatCodeSearchHits directly without ever touching ONNX.
func runCodePack(s *codestore.Store, query string, qv []float32, k, budget int, selectedKeys []string) (codePackOutput, error) {
	hits, err := s.SearchSymbolsHybrid(query, qv, k)
	if err != nil {
		return codePackOutput{}, err
	}

	pack, err := coderetrieval.BuildContextPack(hits, coderetrieval.AssembleOptions{
		Budget:       budget,
		SelectedKeys: selectedKeys,
		GetSymbol:    s.GetSymbol,
		GetRelations: s.RelationsFrom,
	})
	if err != nil {
		return codePackOutput{}, err
	}

	manifest, err := buildManifest(s, pack)
	if err != nil {
		return codePackOutput{}, err
	}

	return codePackOutput{Pack: pack, Manifest: manifest}, nil
}

// formatCodePackOutput writes out as JSON, or (text mode) a compact summary
// -- candidate/symbol/relation counts, budget usage, and the manifest's
// identity fields. Text mode never includes candidate metadata or symbol
// bodies; `--json` is the primary way to consume a pack (see codeUsage).
func formatCodePackOutput(w io.Writer, out codePackOutput, asJSON bool) error {
	if asJSON {
		return json.NewEncoder(w).Encode(out)
	}
	p := out.Pack
	fmt.Fprintf(w, "candidates=%d symbols=%d relations=%d used_chars=%d/%d truncated=%v\n",
		len(p.Candidates), len(p.Symbols), len(p.Relations), p.UsedChars, p.Budget, p.Truncated)
	if p.Truncated {
		fmt.Fprintf(w, "skipped: %s\n", strings.Join(p.Skipped, ", "))
	}
	m := out.Manifest
	fmt.Fprintf(w, "manifest: revision=%s server=%s/%s model=%s symbols=%d\n",
		m.IndexRevision, m.ServerName, m.ServerVersion, m.ModelID, len(m.Symbols))
	return nil
}

func cmdCodePack(args []string) int {
	fs := newFlagSet("code pack")
	db := codeDBFlag(fs)
	query := fs.String("query", "", "search query for top-k candidates")
	k := fs.Int("k", 10, "max candidates")
	budget := fs.Int("budget", codePackDefaultBudget, "max JSON-encoded size (chars) of the assembled pack")
	asJSON := fs.Bool("json", false, "JSON output")
	var selected strFlags
	fs.Var(&selected, "select", "symbol key to include the full body for (repeatable, 1-3)")
	if code, handled := parseArgs(fs, args); handled {
		return code
	}
	if *query == "" {
		return fail(fmt.Errorf("usage: ragrep code pack --query <q> [--select KEY]... [--budget N] [--json]"))
	}
	if len(selected) > 3 {
		return fail(fmt.Errorf("--select accepts at most 3 keys, got %d", len(selected)))
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

	qv, err := e.Embed(*query)
	if err != nil {
		return fail(err)
	}

	out, err := runCodePack(s, *query, qv, *k, *budget, []string(selected))
	if err != nil {
		return fail(err)
	}
	if err := formatCodePackOutput(os.Stdout, out, *asJSON); err != nil {
		return fail(err)
	}
	return 0
}

// codeVerifyEntry is one manifest symbol's `code verify` outcome: staleness
// (from coderetrieval.CheckStale) and re-resolution (from
// coderetrieval.ResolveRef) are reported independently -- a symbol can be
// stale but still resolve (the file changed but the symbol's own key is
// still valid), or resolve to a new key entirely, or fail to resolve at all
// (ErrAmbiguousResolution -- Error is set, ResolvedKey is not).
type codeVerifyEntry struct {
	Key         string `json:"key"`
	Path        string `json:"path"`
	Stale       bool   `json:"stale"`
	Resolved    bool   `json:"resolved"`
	ResolvedKey string `json:"resolved_key,omitempty"`
	Error       string `json:"error,omitempty"`
}

// codeVerifyOutput is `code verify`'s result: per-entry status, plus Clean
// true only when every entry is both fresh and resolved -- the condition
// cmdCodeVerify's exit code (0 vs. 2) is based on.
type codeVerifyOutput struct {
	Entries []codeVerifyEntry `json:"entries"`
	Clean   bool              `json:"clean"`
}

// runCodeVerify checks m's staleness against wsRoot's on-disk files (see
// coderetrieval.CheckStale) and re-resolves every entry against s (see
// coderetrieval.ResolveRef), producing one codeVerifyEntry per manifest
// symbol. Only a genuine store error aborts with a non-nil error --
// ErrAmbiguousResolution is a normal per-entry outcome (see ResolveRef's
// doc comment on why the caller must halt on it rather than guess), folded
// into that entry's Error field, not propagated.
func runCodeVerify(s *codestore.Store, m coderetrieval.Manifest, wsRoot string) (codeVerifyOutput, error) {
	staleReport := coderetrieval.CheckStale(m, func(path string) ([]byte, error) {
		return os.ReadFile(filepath.Join(wsRoot, filepath.FromSlash(path)))
	})
	staleByKey := make(map[string]bool, len(staleReport.Entries))
	for _, e := range staleReport.Entries {
		staleByKey[e.Key] = e.Stale
	}

	out := codeVerifyOutput{Clean: true}
	for _, ref := range m.Symbols {
		entry := codeVerifyEntry{Key: ref.Key, Path: ref.Path, Stale: staleByKey[ref.Key]}
		if entry.Stale {
			out.Clean = false
		}

		resolved, err := coderetrieval.ResolveRef(ref, s.GetSymbol, s.FindByQualifiedName)
		switch {
		case err == nil:
			entry.Resolved = true
			entry.ResolvedKey = resolved.Key
		case errors.Is(err, coderetrieval.ErrAmbiguousResolution):
			entry.Error = err.Error()
			out.Clean = false
		default:
			return codeVerifyOutput{}, err
		}

		out.Entries = append(out.Entries, entry)
	}
	return out, nil
}

// formatCodeVerifyOutput writes out as JSON, or (text mode) one tab-separated
// line per entry (key, path, status -- "ok"/"stale"/"unresolved"/
// "stale,unresolved" -- and any resolution error) followed by a final
// clean=true/false summary line.
func formatCodeVerifyOutput(w io.Writer, out codeVerifyOutput, asJSON bool) error {
	if asJSON {
		return json.NewEncoder(w).Encode(out)
	}
	for _, e := range out.Entries {
		status := "ok"
		if e.Stale {
			status = "stale"
		}
		if !e.Resolved {
			if status == "ok" {
				status = "unresolved"
			} else {
				status += ",unresolved"
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s", e.Key, e.Path, status)
		if e.Error != "" {
			fmt.Fprintf(w, "\t%s", e.Error)
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "clean=%v\n", out.Clean)
	return nil
}

// loadManifest parses data as either shape `code verify --manifest` may be
// pointed at: a bare coderetrieval.Manifest, or the wrapper `code pack
// --json` actually writes to disk ({"pack":...,"manifest":...}, see
// codePackOutput) -- the README documents pointing verify straight at a
// pack's JSON output file, which is the wrapper shape, not a bare manifest.
// A wrapper is detected by its "manifest" key successfully binding; anything
// else (including a bare manifest, or JSON with no recognizable shape at
// all) falls back to unmarshaling data directly as a Manifest -- the caller
// is responsible for treating a resulting zero-symbol Manifest as an error,
// since json.Unmarshal itself can't distinguish "a real empty manifest"
// from "not a manifest at all" (e.g. `{"hello":"world"}`).
func loadManifest(data []byte) (coderetrieval.Manifest, error) {
	var wrapper struct {
		Manifest *coderetrieval.Manifest `json:"manifest"`
	}
	if err := json.Unmarshal(data, &wrapper); err == nil && wrapper.Manifest != nil {
		return *wrapper.Manifest, nil
	}
	var m coderetrieval.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return coderetrieval.Manifest{}, err
	}
	return m, nil
}

func cmdCodeVerify(args []string) int {
	fs := newFlagSet("code verify")
	db := codeDBFlag(fs)
	manifestPath := fs.String("manifest", "", "manifest JSON file (from `code pack`)")
	asJSON := fs.Bool("json", false, "JSON output")
	if code, handled := parseArgs(fs, args); handled {
		return code
	}
	if *manifestPath == "" {
		return fail(fmt.Errorf("usage: ragrep code verify --manifest <file> [--json]"))
	}

	data, err := os.ReadFile(*manifestPath)
	if err != nil {
		return fail(err)
	}
	m, err := loadManifest(data)
	if err != nil {
		return fail(fmt.Errorf("parsing manifest %s: %w", *manifestPath, err))
	}
	if len(m.Symbols) == 0 {
		return fail(fmt.Errorf("manifest %s has zero symbols (not a valid manifest or code-pack output?)", *manifestPath))
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

	out, err := runCodeVerify(s, m, wsRoot)
	if err != nil {
		return fail(err)
	}
	if err := formatCodeVerifyOutput(os.Stdout, out, *asJSON); err != nil {
		return fail(err)
	}
	if !out.Clean {
		return 2
	}
	return 0
}

package coderetrieval

import (
	"errors"
	"fmt"

	"github.com/siroio/ragrep/internal/codeindex"
	"github.com/siroio/ragrep/internal/codestore"
)

// SymbolRef is one manifest entry: a reference to an indexed symbol by
// stable key, plus enough denormalized identity (qualified name, path,
// line range, file hash) to re-resolve or stale-check it later without
// going back to the store. It never holds the symbol's Body.
type SymbolRef struct {
	Key           string `json:"key"`
	QualifiedName string `json:"qualified_name"`
	Path          string `json:"path"`
	StartLine     int    `json:"start_line"`
	EndLine       int    `json:"end_line"`
	FileHash      string `json:"file_hash"`
}

// Manifest is a stale-detectable plan: which symbols a context pack was
// built from (by reference, never by copying their bodies), plus the index
// identity it was built against. A caller re-checks it with CheckStale
// before trusting it, and re-resolves individual entries with ResolveRef
// when a stable key no longer exists.
type Manifest struct {
	IndexRevision string      `json:"index_revision"`
	ServerName    string      `json:"server_name"`
	ServerVersion string      `json:"server_version"`
	ModelID       string      `json:"model_id"`
	Symbols       []SymbolRef `json:"symbols"`
}

// StaleEntry is one manifest symbol's staleness after CheckStale recomputed
// its file's current hash.
type StaleEntry struct {
	Key   string `json:"key"`
	Path  string `json:"path"`
	Stale bool   `json:"stale"`
}

// StaleReport is CheckStale's result: per-entry staleness, plus Stale true
// overall if any entry is stale.
type StaleReport struct {
	Entries []StaleEntry `json:"entries"`
	Stale   bool         `json:"stale"`
}

// CheckStale recomputes each referenced file's hash via readFile
// (os.ReadFile in production; codeindex.FileHash is the same hashing rule
// the indexer stamps into codestore, so this validates directly against
// what's on disk) and compares it against the hash recorded at plan time.
// Each distinct path is read and hashed once even if several symbols
// reference it.
func CheckStale(m Manifest, readFile func(path string) ([]byte, error)) (StaleReport, error) {
	hashes := map[string]string{}
	var report StaleReport
	for _, ref := range m.Symbols {
		hash, ok := hashes[ref.Path]
		if !ok {
			content, err := readFile(ref.Path)
			if err != nil {
				return StaleReport{}, fmt.Errorf("coderetrieval: read %q: %w", ref.Path, err)
			}
			hash = codeindex.FileHash(content)
			hashes[ref.Path] = hash
		}
		stale := hash != ref.FileHash
		report.Entries = append(report.Entries, StaleEntry{Key: ref.Key, Path: ref.Path, Stale: stale})
		if stale {
			report.Stale = true
		}
	}
	return report, nil
}

// SymbolFinder looks up symbols by exact qualified name and
// workspace-relative path, for re-resolving a manifest entry whose stable
// key no longer exists (the declaration moved -- Key embeds its signature,
// see codeindex.Symbol.Key -- but its qualified name and file didn't).
type SymbolFinder func(qualifiedName, path string) ([]codeindex.Symbol, error)

// ErrAmbiguousResolution is returned by ResolveRef when re-resolving by
// qualified name + path matches zero or more than one symbol. The caller
// must halt on this -- there is no auto-adoption of a guessed match.
var ErrAmbiguousResolution = errors.New("coderetrieval: ambiguous symbol re-resolution")

// ResolveRef checks whether ref's stable key still resolves via getSymbol.
// If it does, ref is returned unchanged. If getSymbol reports the key isn't
// found (errors.Is(..., codestore.ErrNotFound)), ResolveRef falls back to
// findSymbol(ref.QualifiedName, ref.Path): exactly one match re-resolves
// ref to the match's new key and line range; zero or multiple matches is an
// error wrapping ErrAmbiguousResolution, and ref is not modified. Any other
// getSymbol error is returned as-is.
func ResolveRef(ref SymbolRef, getSymbol SymbolGetter, findSymbol SymbolFinder) (SymbolRef, error) {
	if _, err := getSymbol(ref.Key); err == nil {
		return ref, nil
	} else if !errors.Is(err, codestore.ErrNotFound) {
		return SymbolRef{}, err
	}

	matches, err := findSymbol(ref.QualifiedName, ref.Path)
	if err != nil {
		return SymbolRef{}, err
	}
	if len(matches) != 1 {
		return SymbolRef{}, fmt.Errorf("%w: %q in %s matched %d symbols, want exactly 1",
			ErrAmbiguousResolution, ref.QualifiedName, ref.Path, len(matches))
	}

	sym := matches[0]
	resolved := ref
	resolved.Key = sym.Key
	resolved.StartLine = sym.Range.Start.Line
	resolved.EndLine = sym.Range.End.Line
	return resolved, nil
}

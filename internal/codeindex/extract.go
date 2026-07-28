package codeindex

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/siroio/ragrep/internal/lsp"
)

// Extract flattens the (possibly hierarchical) document symbols gopls
// returned for one file into code-index records. relPath must already be
// workspace-relative, slash-separated (see internal/store's doc-key
// convention); it is stored verbatim as Symbol.Path.
//
// Nesting (DocumentSymbol.Children) becomes container tracking, not nested
// records: every symbol at every depth — including a struct/class and each
// of its methods — becomes its own flat Symbol. QualifiedName is the dot
// chain of ancestor names ending in the symbol's own name (e.g.
// "Store.Save" for a Save method nested under a Store type); Container is
// just the immediate parent's name ("Store"), or "" at the top level.
func Extract(language, relPath string, content []byte, symbols []lsp.DocumentSymbol) ([]Symbol, error) {
	li := newLineIndex(content)
	var out []Symbol
	if err := flatten(li, language, relPath, symbols, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// FileHash is the SHA-256 hex digest of a whole file's content, used by
// callers to detect that a file changed since it was last indexed. It is
// not stored on Symbol (a file may contain many symbols); callers persist
// it once per indexed file.
func FileHash(content []byte) string {
	return hashHex(content)
}

func flatten(li *lineIndex, language, relPath string, symbols []lsp.DocumentSymbol, containers []string, out *[]Symbol) error {
	for _, sym := range symbols {
		body, err := extractBody(li, sym.Range)
		if err != nil {
			return fmt.Errorf("codeindex: symbol %q in %s: %w", sym.Name, relPath, err)
		}

		s := Symbol{
			Language:      language,
			Kind:          kindName(sym.Kind),
			Name:          sym.Name,
			QualifiedName: qualifiedName(containers, sym.Name),
			Signature:     declSignature(body),
			Documentation: li.docComment(sym.Range.Start.Line),
			Container:     immediateContainer(containers),
			Path:          relPath,
			Range:         fromLSPRange(sym.Range),
			Body:          body,
			BodyHash:      hashHexString(body),
		}
		s.Key = stableKey(s.Language, s.Path, s.Kind, s.QualifiedName, s.Signature)
		s.EmbeddingText = RenderEmbeddingText(s)
		*out = append(*out, s)

		if len(sym.Children) > 0 {
			// Copy, don't reuse: sibling calls at this depth must not share
			// containers' backing array once each has appended its own name.
			childContainers := append(append([]string{}, containers...), sym.Name)
			if err := flatten(li, language, relPath, sym.Children, childContainers, out); err != nil {
				return err
			}
		}
	}
	return nil
}

func qualifiedName(containers []string, name string) string {
	if len(containers) == 0 {
		return name
	}
	return strings.Join(containers, ".") + "." + name
}

func immediateContainer(containers []string) string {
	if len(containers) == 0 {
		return ""
	}
	return containers[len(containers)-1]
}

// declSignature approximates a symbol's declaration from its extracted
// body: everything up to (not including) the first '{', with internal
// whitespace/newlines collapsed to single spaces — e.g. a Go func's
// multi-line parameter list becomes one line. Symbols with no brace in
// their range (fields, constants, type aliases without a body) fall back to
// their first line.
func declSignature(body string) string {
	head := body
	if idx := strings.IndexByte(body, '{'); idx >= 0 {
		head = body[:idx]
	} else if idx := strings.IndexByte(body, '\n'); idx >= 0 {
		head = body[:idx]
	}
	return strings.Join(strings.Fields(head), " ")
}

func extractBody(li *lineIndex, r lsp.Range) (string, error) {
	start, err := li.offset(fromLSPPosition(r.Start))
	if err != nil {
		return "", fmt.Errorf("range start: %w", err)
	}
	end, err := li.offset(fromLSPPosition(r.End))
	if err != nil {
		return "", fmt.Errorf("range end: %w", err)
	}
	if end < start {
		return "", fmt.Errorf("range end (%d) before start (%d)", end, start)
	}
	return string(li.content[start:end]), nil
}

func fromLSPPosition(p lsp.Position) Position {
	return Position{Line: p.Line, Character: p.Character}
}

func fromLSPRange(r lsp.Range) Range {
	return Range{Start: fromLSPPosition(r.Start), End: fromLSPPosition(r.End)}
}

func stableKey(language, path, kind, qualifiedName, signature string) string {
	joined := strings.Join([]string{language, path, kind, qualifiedName, signature}, "\x00")
	return hashHexString(joined)
}

func hashHexString(s string) string { return hashHex([]byte(s)) }

func hashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// symbolKindNames maps LSP's SymbolKind enum (LSP 3.17 §3.17.4) to the
// lowercase kind names codeindex stores. DocumentSymbol.Kind values outside
// this table map to "symbol".
var symbolKindNames = map[int]string{
	1:  "file",
	2:  "module",
	3:  "namespace",
	4:  "package",
	5:  "class",
	6:  "method",
	7:  "property",
	8:  "field",
	9:  "constructor",
	10: "enum",
	11: "interface",
	12: "function",
	13: "variable",
	14: "constant",
	15: "string",
	16: "number",
	17: "boolean",
	18: "array",
	19: "object",
	20: "key",
	21: "null",
	22: "enum_member",
	23: "struct",
	24: "event",
	25: "operator",
	26: "type_parameter",
}

func kindName(k int) string {
	if name, ok := symbolKindNames[k]; ok {
		return name
	}
	return "symbol"
}

// --- UTF-16 position -> byte offset conversion ---

// lineIndex maps LSP Positions (UTF-16 code units, LSP-spec line endings)
// to byte offsets within content, computed once per file and reused for
// every symbol's Range.
type lineIndex struct {
	content []byte
	starts  []int // byte offset of the start of each line
}

func newLineIndex(content []byte) *lineIndex {
	starts := []int{0}
	for i := 0; i < len(content); i++ {
		switch content[i] {
		case '\r':
			if i+1 < len(content) && content[i+1] == '\n' {
				i++
			}
			starts = append(starts, i+1)
		case '\n':
			starts = append(starts, i+1)
		}
	}
	return &lineIndex{content: content, starts: starts}
}

// offset converts an LSP-style Position into a byte offset in content.
// Character is clamped to the line's actual UTF-16 length, so a position at
// (or past) end-of-line resolves to the byte offset right before the line
// terminator — matching LSP's tolerant handling of edge-of-line positions.
func (li *lineIndex) offset(pos Position) (int, error) {
	if pos.Line < 0 || pos.Line >= len(li.starts) {
		return 0, fmt.Errorf("codeindex: line %d out of range (file has %d lines)", pos.Line, len(li.starts))
	}
	if pos.Character < 0 {
		return 0, fmt.Errorf("codeindex: character %d is negative", pos.Character)
	}

	line := li.lineBytes(pos.Line)
	units, b := 0, 0
	for b < len(line) {
		if units >= pos.Character {
			break
		}
		r, size := utf8.DecodeRune(line[b:])
		n := utf16.RuneLen(r)
		if n < 0 {
			n = 1
		}
		units += n
		b += size
	}
	if units < pos.Character {
		b = len(line) // past end-of-line: clamp
	}
	return li.starts[pos.Line] + b, nil
}

// lineBytes returns line n's content, excluding its line terminator.
func (li *lineIndex) lineBytes(n int) []byte {
	if n < 0 || n >= len(li.starts) {
		return nil
	}
	start := li.starts[n]
	end := len(li.content)
	if n+1 < len(li.starts) {
		end = li.starts[n+1]
	}
	return trimLineTerminator(li.content[start:end])
}

func trimLineTerminator(line []byte) []byte {
	if n := len(line); n > 0 && line[n-1] == '\n' {
		line = line[:n-1]
	}
	if n := len(line); n > 0 && line[n-1] == '\r' {
		line = line[:n-1]
	}
	return line
}

// docComment collects the contiguous "//" comment lines directly above
// declLine (no blank line in between), in source order, "// " prefixes
// stripped — Go's usual doc-comment convention. Returns "" if declLine has
// no such block immediately above it.
//
// This is Go-specific (only "//" line comments; no "/* */" block comments).
// A second language's extractor will need its own comment-style handling
// here, or this renamed/parameterized per language.
func (li *lineIndex) docComment(declLine int) string {
	var lines []string
	for n := declLine - 1; n >= 0; n-- {
		text := strings.TrimSpace(string(li.lineBytes(n)))
		if !strings.HasPrefix(text, "//") {
			break
		}
		lines = append(lines, strings.TrimSpace(strings.TrimPrefix(text, "//")))
	}
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	return strings.Join(lines, "\n")
}

package codeindex

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/siroio/ragrep/internal/lsp"
)

// --- UTF-16 position -> byte offset conversion ---
//
// LSP Positions count UTF-16 code units per line, not bytes and not runes.
// These cases must not be ASCII-only: CRLF line endings, multi-byte UTF-8
// (Japanese, one UTF-16 unit but three UTF-8 bytes per character), and an
// emoji (one rune but a UTF-16 *surrogate pair*, i.e. two units).

func TestLineIndexOffset(t *testing.T) {
	tests := []struct {
		name    string
		content string
		pos     Position
		want    int
	}{
		{
			name:    "ascii start of second line, CRLF endings",
			content: "line one\r\nsecond line\r\n",
			pos:     Position{Line: 1, Character: 0},
			want:    10, // len("line one\r\n")
		},
		{
			name:    "ascii end of CRLF line clamps before the terminator",
			content: "line one\r\nsecond line\r\n",
			pos:     Position{Line: 0, Character: 8},
			want:    8, // right after "line one", before \r\n
		},
		{
			name:    "character past end of line clamps to line end",
			content: "line one\r\nsecond line\r\n",
			pos:     Position{Line: 0, Character: 999},
			want:    8,
		},
		{
			name:    "japanese: one UTF-16 unit, three UTF-8 bytes per rune",
			content: "日本語\nテスト\n",
			pos:     Position{Line: 0, Character: 1},
			want:    3, // after "日"
		},
		{
			name:    "japanese: end of first line",
			content: "日本語\nテスト\n",
			pos:     Position{Line: 0, Character: 3},
			want:    9, // "日本語" = 3 runes * 3 bytes
		},
		{
			name:    "japanese: into the second line",
			content: "日本語\nテスト\n",
			pos:     Position{Line: 1, Character: 2},
			want:    16, // line1 starts at byte 10, +2 runes * 3 bytes
		},
		{
			name:    "emoji surrogate pair counts as two UTF-16 units",
			content: "😀abc\n",
			pos:     Position{Line: 0, Character: 2},
			want:    4, // 😀 is 4 UTF-8 bytes but 2 UTF-16 units
		},
		{
			name:    "emoji: byte offset after trailing ascii",
			content: "😀abc\n",
			pos:     Position{Line: 0, Character: 3},
			want:    5, // 4 (emoji) + 1 ('a')
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			li := newLineIndex([]byte(tt.content))
			got, err := li.offset(tt.pos)
			if err != nil {
				t.Fatalf("offset(%+v) returned error: %v", tt.pos, err)
			}
			if got != tt.want {
				t.Errorf("offset(%+v) = %d, want %d", tt.pos, got, tt.want)
			}
		})
	}
}

func TestLineIndexOffsetErrors(t *testing.T) {
	li := newLineIndex([]byte("only line\n"))
	if _, err := li.offset(Position{Line: 5, Character: 0}); err == nil {
		t.Error("expected an error for an out-of-range line, got nil")
	}
	if _, err := li.offset(Position{Line: 0, Character: -1}); err == nil {
		t.Error("expected an error for a negative character, got nil")
	}
}

// --- Extract: nesting, qualified names, struct/method separation ---

func loadSample(t *testing.T) []byte {
	t.Helper()
	content, err := os.ReadFile("testdata/go/sample.go")
	if err != nil {
		t.Fatalf("reading testdata: %v", err)
	}
	return content
}

// sampleSymbols builds the DocumentSymbol tree gopls would plausibly return
// for testdata/go/sample.go: a Store struct containing its Name field and
// its Save/Load methods as children (gopls nests Go methods under their
// receiver type in hierarchical document symbols). Ranges are hand-derived
// from the fixture file's fixed line layout (see sample.go); every range
// used here starts at character 0 of its first line and, where the line
// content is just "}", ends at character 1 of that line — so no UTF-16
// arithmetic is needed to keep these in sync with the file.
func sampleSymbols() []lsp.DocumentSymbol {
	return []lsp.DocumentSymbol{
		{
			Name: "Store",
			Kind: 23, // struct
			Range: lsp.Range{
				Start: lsp.Position{Line: 7, Character: 0},  // "type Store struct {"
				End:   lsp.Position{Line: 10, Character: 1}, // closing "}"
			},
			Children: []lsp.DocumentSymbol{
				{
					Name: "Name",
					Kind: 8, // field
					Range: lsp.Range{
						Start: lsp.Position{Line: 9, Character: 0},
						End:   lsp.Position{Line: 9, Character: 999}, // clamps to end of line
					},
				},
				{
					Name: "Save",
					Kind: 6, // method
					Range: lsp.Range{
						Start: lsp.Position{Line: 13, Character: 0}, // func (s *Store) Save(...) {
						End:   lsp.Position{Line: 16, Character: 1}, // closing "}"
					},
				},
				{
					Name: "Load",
					Kind: 6, // method
					Range: lsp.Range{
						Start: lsp.Position{Line: 18, Character: 0}, // func (s *Store) Load(...) {
						End:   lsp.Position{Line: 21, Character: 1}, // closing "}"
					},
				},
			},
		},
	}
}

func TestExtract_NestedQualifiedNamesAndContainers(t *testing.T) {
	content := loadSample(t)
	symbols, err := Extract("go", "sample.go", content, sampleSymbols())
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	byName := map[string]Symbol{}
	for _, s := range symbols {
		byName[s.Name] = s
	}

	store, ok := byName["Store"]
	if !ok {
		t.Fatal("missing Store symbol")
	}
	if store.QualifiedName != "Store" || store.Container != "" {
		t.Errorf("Store: QualifiedName=%q Container=%q, want QualifiedName=%q Container=%q",
			store.QualifiedName, store.Container, "Store", "")
	}

	save, ok := byName["Save"]
	if !ok {
		t.Fatal("missing Save symbol")
	}
	if save.QualifiedName != "Store.Save" || save.Container != "Store" {
		t.Errorf("Save: QualifiedName=%q Container=%q, want QualifiedName=%q Container=%q",
			save.QualifiedName, save.Container, "Store.Save", "Store")
	}

	name, ok := byName["Name"]
	if !ok {
		t.Fatal("missing Name field symbol")
	}
	if name.QualifiedName != "Store.Name" || name.Container != "Store" {
		t.Errorf("Name: QualifiedName=%q Container=%q, want QualifiedName=%q Container=%q",
			name.QualifiedName, name.Container, "Store.Name", "Store")
	}
}

func TestExtract_StructAndMethodsAreSeparateRecords(t *testing.T) {
	content := loadSample(t)
	symbols, err := Extract("go", "sample.go", content, sampleSymbols())
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	if len(symbols) != 4 {
		names := make([]string, len(symbols))
		for i, s := range symbols {
			names[i] = s.Name
		}
		t.Fatalf("got %d symbols %v, want 4 (Store, Name, Save, Load)", len(symbols), names)
	}

	var store, save, load Symbol
	for _, s := range symbols {
		switch s.Name {
		case "Store":
			store = s
		case "Save":
			save = s
		case "Load":
			load = s
		}
	}

	if store.Kind != "struct" {
		t.Errorf("Store.Kind = %q, want %q", store.Kind, "struct")
	}
	if save.Kind != "method" || load.Kind != "method" {
		t.Errorf("Save.Kind=%q Load.Kind=%q, want both %q", save.Kind, load.Kind, "method")
	}

	// The struct's own body must not contain the methods' bodies.
	if want := "func (s *Store) Save"; strings.Contains(store.Body, want) {
		t.Errorf("Store.Body unexpectedly contains %q:\n%s", want, store.Body)
	}
	if want := "func (s *Store) Load"; strings.Contains(store.Body, want) {
		t.Errorf("Store.Body unexpectedly contains %q:\n%s", want, store.Body)
	}
	// The struct's embedding text must not carry any method body either.
	if want := "return nil"; strings.Contains(store.EmbeddingText, want) {
		t.Errorf("Store.EmbeddingText unexpectedly contains method body %q:\n%s", want, store.EmbeddingText)
	}

	if store.Key == save.Key || store.Key == load.Key || save.Key == load.Key {
		t.Errorf("expected distinct keys for distinct records, got Store=%q Save=%q Load=%q",
			store.Key, save.Key, load.Key)
	}
}

func TestExtract_JapaneseDocComment(t *testing.T) {
	content := loadSample(t)
	symbols, err := Extract("go", "sample.go", content, sampleSymbols())
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	for _, s := range symbols {
		if s.Name != "Store" {
			continue
		}
		want := "Store persists items keyed by ID. 日本語のコメントも含みます。"
		if s.Documentation != want {
			t.Errorf("Store.Documentation = %q, want %q", s.Documentation, want)
		}
		return
	}
	t.Fatal("missing Store symbol")
}

func TestExtract_EmojiInDocComment(t *testing.T) {
	content := loadSample(t)
	symbols, err := Extract("go", "sample.go", content, sampleSymbols())
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	for _, s := range symbols {
		if s.Name != "Save" {
			continue
		}
		if !strings.Contains(s.Documentation, "🎉") {
			t.Errorf("Save.Documentation = %q, want it to contain the emoji", s.Documentation)
		}
		return
	}
	t.Fatal("missing Save symbol")
}

// --- Determinism: same input -> identical keys and embedding text ---

func TestExtract_Deterministic(t *testing.T) {
	content := loadSample(t)
	symbols := sampleSymbols()

	first, err := Extract("go", "sample.go", content, symbols)
	if err != nil {
		t.Fatalf("Extract (first run): %v", err)
	}
	second, err := Extract("go", "sample.go", content, symbols)
	if err != nil {
		t.Fatalf("Extract (second run): %v", err)
	}

	if !reflect.DeepEqual(first, second) {
		t.Fatalf("two Extract runs over identical input produced different results:\nfirst:  %+v\nsecond: %+v", first, second)
	}
	for i := range first {
		if first[i].Key == "" {
			t.Errorf("symbol %q has an empty Key", first[i].Name)
		}
	}
}

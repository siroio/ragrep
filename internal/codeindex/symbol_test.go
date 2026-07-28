package codeindex

import (
	"encoding/json"
	"strings"
	"testing"
)

// Symbol/Relation are serialized straight into `code pack`'s JSON output
// (see coderetrieval.ContextPack); they must use the same snake_case
// convention as codestore.SymbolHit's own json tags, not Go's default
// PascalCase field names. EmbeddingText is internal render output (always
// recomputable from the other fields, see RenderEmbeddingText) and must
// never be serialized.
func TestSymbolJSONTagsAreSnakeCaseAndOmitEmbeddingText(t *testing.T) {
	sym := Symbol{
		Key:           "k1",
		Language:      "go",
		Kind:          "function",
		Name:          "Foo",
		QualifiedName: "Foo",
		Signature:     "func Foo()",
		Documentation: "Foo does things.",
		Container:     "",
		Path:          "pkg/foo.go",
		Range: Range{
			Start: Position{Line: 1, Character: 0},
			End:   Position{Line: 3, Character: 1},
		},
		Body:          "func Foo() {}\n",
		BodyHash:      "bodyhash1",
		EmbeddingText: "this must never appear in JSON output",
	}

	b, err := json.Marshal(sym)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)

	if strings.Contains(got, "embedding_text") || strings.Contains(got, "EmbeddingText") ||
		strings.Contains(got, "this must never appear") {
		t.Fatalf("marshaled Symbol must never include EmbeddingText: %s", got)
	}

	for _, want := range []string{
		`"key":`, `"language":`, `"kind":`, `"name":`, `"qualified_name":`,
		`"signature":`, `"documentation":`, `"path":`, `"range":`,
		`"body":`, `"body_hash":`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("marshaled Symbol = %s, want it to contain %s", got, want)
		}
	}

	// PascalCase field names must not leak through.
	for _, notWant := range []string{`"Key":`, `"QualifiedName":`, `"BodyHash":`} {
		if strings.Contains(got, notWant) {
			t.Fatalf("marshaled Symbol = %s, must not contain PascalCase key %s", got, notWant)
		}
	}

	var decoded Symbol
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatal(err)
	}
	decoded.EmbeddingText = sym.EmbeddingText // excluded from JSON by design; restore for comparison
	if decoded != sym {
		t.Fatalf("round-trip mismatch:\ngot:  %#v\nwant: %#v", decoded, sym)
	}
}

func TestRelationJSONTagsAreSnakeCase(t *testing.T) {
	rel := Relation{
		FromKey: "f", ToKey: "t", Kind: "references", Source: "gopls",
	}
	b, err := json.Marshal(rel)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, want := range []string{`"from_key":`, `"to_key":`, `"kind":`, `"source":`} {
		if !strings.Contains(got, want) {
			t.Fatalf("marshaled Relation = %s, want it to contain %s", got, want)
		}
	}
	for _, notWant := range []string{`"FromKey":`, `"ToKey":`} {
		if strings.Contains(got, notWant) {
			t.Fatalf("marshaled Relation = %s, must not contain PascalCase key %s", got, notWant)
		}
	}
}

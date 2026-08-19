package codeindex

import "testing"

func TestRenderEmbeddingText_AllFields(t *testing.T) {
	s := Symbol{
		Language:      "go",
		Kind:          "method",
		QualifiedName: "Store.Save",
		Container:     "Store",
		Signature:     "func (s *Store) Save(ctx context.Context, item string) error",
		Documentation: "Saves one item transactionally.",
		Path:          "pkg/store.go",
	}
	want := "language: go\n" +
		"kind: method\n" +
		"qualified_name: Store.Save\n" +
		"container: Store\n" +
		"signature: func (s *Store) Save(ctx context.Context, item string) error\n" +
		"documentation: Saves one item transactionally.\n" +
		"path: pkg/store.go"

	got := RenderEmbeddingText(s)
	if got != want {
		t.Errorf("RenderEmbeddingText mismatch\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestRenderEmbeddingText_OmitsEmptyFields(t *testing.T) {
	s := Symbol{
		Language:      "go",
		Kind:          "struct",
		QualifiedName: "Store",
		// Container and Documentation intentionally left empty.
		Signature: "type Store struct",
		Path:      "pkg/store.go",
	}
	want := "language: go\n" +
		"kind: struct\n" +
		"qualified_name: Store\n" +
		"signature: type Store struct\n" +
		"path: pkg/store.go"

	got := RenderEmbeddingText(s)
	if got != want {
		t.Errorf("RenderEmbeddingText mismatch\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestRenderEmbeddingText_Deterministic(t *testing.T) {
	s := Symbol{Language: "go", Kind: "function", QualifiedName: "Do", Signature: "func Do()"}
	first := RenderEmbeddingText(s)
	second := RenderEmbeddingText(s)
	if first != second {
		t.Fatalf("RenderEmbeddingText is not deterministic: %q vs %q", first, second)
	}
}

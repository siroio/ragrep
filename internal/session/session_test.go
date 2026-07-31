package session

import (
	"path/filepath"
	"testing"

	"github.com/siroio/ragrep/internal/store"
)

type fakeEmbedder struct {
	calls  int
	closes int
}

func (e *fakeEmbedder) Embed(string) ([]float32, error) {
	e.calls++
	return make([]float32, 768), nil
}

func (e *fakeEmbedder) Close() { e.closes++ }

func TestSessionLifecycleHybridSearchesReuseEmbedder(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertDoc("a.md", "alpha", 1, func(string) ([]float32, error) { return make([]float32, 768), nil }); err != nil {
		t.Fatal(err)
	}
	e := &fakeEmbedder{}
	runtime := New(s, e)

	for range 2 {
		if _, err := runtime.Search("hybrid", "alpha", 10, nil); err != nil {
			t.Fatal(err)
		}
	}
	if e.calls != 2 {
		t.Fatalf("embed calls=%d, want 2 from one reusable embedder", e.calls)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSessionLifecycleGetReusesStore(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertDoc("a.md", "alpha", 1, func(string) ([]float32, error) { return make([]float32, 768), nil }); err != nil {
		t.Fatal(err)
	}
	runtime := New(s, &fakeEmbedder{})

	got, err := runtime.Get("a.md")
	if err != nil || got != "alpha" {
		t.Fatalf("Get() = %q, %v; want alpha, nil", got, err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSessionLifecycleCloseIsIdempotent(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	e := &fakeEmbedder{}
	runtime := New(s, e)

	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	if e.closes != 1 {
		t.Fatalf("embed close calls=%d, want 1", e.closes)
	}
	if _, err := runtime.Get("a.md"); err == nil {
		t.Fatal("Get after Close: want session-closed error")
	}
}

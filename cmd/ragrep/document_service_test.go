package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/siroio/ragrep/internal/store"
)

func seedDocumentDB(t *testing.T, path, doc, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.UpsertDoc(doc, content, 1, fakeEmbed); err != nil {
		t.Fatal(err)
	}
}

func TestDocumentStorePoolOpensConcurrentFirstUseOnce(t *testing.T) {
	db := filepath.Join(t.TempDir(), "index.db")
	seedDocumentDB(t, db, "notes/result.md", "search result")
	const searches = 8
	var opens atomic.Int32
	openerStarted := make(chan struct{})
	releaseOpener := make(chan struct{})
	svc := newDocumentService(nil, func(path string) (*store.Store, error) {
		if opens.Add(1) == 1 {
			close(openerStarted)
			<-releaseOpener
		}
		return store.Open(path)
	})
	t.Cleanup(func() { _ = svc.Close() })

	start := make(chan struct{})
	searchesReady := make(chan struct{}, searches)
	var wg sync.WaitGroup
	for range searches {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			searchesReady <- struct{}{}
			hits, err := svc.SearchDocuments(context.Background(), documentSearchRequest{DB: db, Query: "search", Mode: "text", K: 1})
			if err != nil || len(hits) != 1 || hits[0].Doc != "notes/result.md" {
				t.Errorf("hits=%+v err=%v", hits, err)
			}
		}()
	}
	close(start)
	<-openerStarted
	for range searches {
		<-searchesReady
	}
	close(releaseOpener)
	wg.Wait()
	if got := opens.Load(); got != 1 {
		t.Fatalf("open calls=%d, want 1", got)
	}
}

func TestDocumentStorePoolKeepsDatabasesIsolated(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first.db")
	second := filepath.Join(dir, "second.db")
	seedDocumentDB(t, first, "first.md", "first only")
	seedDocumentDB(t, second, "second.md", "second only")
	svc := newDocumentService(nil, store.Open)
	t.Cleanup(func() { _ = svc.Close() })

	for _, tc := range []struct{ db, query, want string }{
		{first, "first", "first.md"},
		{second, "second", "second.md"},
	} {
		hits, err := svc.SearchDocuments(context.Background(), documentSearchRequest{DB: tc.db, Query: tc.query, Mode: "text", K: 1})
		if err != nil || len(hits) != 1 || hits[0].Doc != tc.want {
			t.Fatalf("db=%s hits=%+v err=%v", tc.db, hits, err)
		}
	}
}

func TestDocumentStorePoolReusesExistingFileAlias(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "index.db")
	alias := filepath.Join(dir, "index-alias.db")
	seedDocumentDB(t, db, "notes/result.md", "search result")
	if err := os.Link(db, alias); err != nil {
		t.Fatal(err)
	}
	var opens atomic.Int32
	svc := newDocumentService(nil, func(path string) (*store.Store, error) {
		opens.Add(1)
		return store.Open(path)
	})
	t.Cleanup(func() { _ = svc.Close() })

	for _, path := range []string{db, alias} {
		hits, err := svc.SearchDocuments(context.Background(), documentSearchRequest{DB: path, Query: "search", Mode: "text", K: 1})
		if err != nil || len(hits) != 1 {
			t.Fatalf("path=%s hits=%+v err=%v", path, hits, err)
		}
	}
	if got := opens.Load(); got != 1 {
		t.Fatalf("open calls=%d, want 1 for aliases", got)
	}
}

func TestDocumentServiceCloseWaitsForLeaseAndRejectsLaterSearch(t *testing.T) {
	db := filepath.Join(t.TempDir(), "index.db")
	otherDB := filepath.Join(filepath.Dir(db), "other.db")
	seedDocumentDB(t, db, "notes/result.md", "search result")
	seedDocumentDB(t, otherDB, "notes/other.md", "other result")
	started := make(chan struct{})
	release := make(chan struct{})
	p := newEmbeddingPool(func() (textEmbedder, error) {
		return textEmbedderFunc(func(string) ([]float32, error) {
			close(started)
			<-release
			return fakeEmbed("search")
		}), nil
	})
	var opened []*store.Store
	var openedMu sync.Mutex
	svc := newDocumentService(p, func(path string) (*store.Store, error) {
		s, err := store.Open(path)
		if err == nil {
			openedMu.Lock()
			opened = append(opened, s)
			openedMu.Unlock()
		}
		return s, err
	})
	if _, err := svc.SearchDocuments(context.Background(), documentSearchRequest{DB: otherDB, Query: "other", Mode: "text", K: 1}); err != nil {
		t.Fatal(err)
	}
	searchDone := make(chan error, 1)
	go func() {
		_, err := svc.SearchDocuments(context.Background(), documentSearchRequest{DB: db, Query: "search", Mode: "vector", K: 1})
		searchDone <- err
	}()
	<-started

	closed := make(chan error, 1)
	go func() { closed <- svc.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned before lease finished: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if _, err := svc.SearchDocuments(context.Background(), documentSearchRequest{DB: db, Query: "search", Mode: "text", K: 1}); err == nil {
		t.Fatal("search after Close: want error")
	}
	close(release)
	if err := <-searchDone; err != nil {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	openedMu.Lock()
	stores := append([]*store.Store(nil), opened...)
	openedMu.Unlock()
	if got := len(stores); got != 2 {
		t.Fatalf("opened stores=%d, want 2", got)
	}
	for _, s := range stores {
		if _, err := s.SearchText("search", 1, nil); err == nil {
			t.Fatal("store remains usable after service Close")
		}
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDocumentServiceTextDoesNotConstructSharedEmbedder(t *testing.T) {
	db := filepath.Join(t.TempDir(), "index.db")
	seedDocumentDB(t, db, "notes/result.md", "search result")
	var constructed atomic.Int32
	p := newEmbeddingPool(func() (textEmbedder, error) {
		constructed.Add(1)
		return textEmbedderFunc(fakeEmbed), nil
	})
	svc := newDocumentService(p, store.Open)
	t.Cleanup(func() { _ = svc.Close(); _ = p.Close() })

	if _, err := svc.SearchDocuments(context.Background(), documentSearchRequest{DB: db, Query: "search", Mode: "text", K: 1}); err != nil {
		t.Fatal(err)
	}
	if got := constructed.Load(); got != 0 {
		t.Fatalf("constructed=%d, want 0", got)
	}
}

func TestDocumentServiceVectorAndHybridUseSharedEmbeddingPool(t *testing.T) {
	db := filepath.Join(t.TempDir(), "index.db")
	seedDocumentDB(t, db, "notes/result.md", "search result")
	var calls atomic.Int32
	p := newEmbeddingPool(func() (textEmbedder, error) {
		return textEmbedderFunc(func(query string) ([]float32, error) {
			calls.Add(1)
			if query != "task: search result | query: search" {
				return nil, errors.New("unexpected embedding query")
			}
			return fakeEmbed(query)
		}), nil
	})
	svc := newDocumentService(p, store.Open)
	t.Cleanup(func() { _ = svc.Close(); _ = p.Close() })

	for _, mode := range []string{"vector", "hybrid"} {
		hits, err := svc.SearchDocuments(context.Background(), documentSearchRequest{DB: db, Query: "search", Mode: mode, K: 1})
		if err != nil || len(hits) != 1 || hits[0].Doc != "notes/result.md" {
			t.Fatalf("mode=%s hits=%+v err=%v", mode, hits, err)
		}
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("shared embedding calls=%d, want 2", got)
	}
}

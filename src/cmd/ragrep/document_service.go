package main

import (
	"context"
	"errors"
	"path/filepath"
	"sync"

	"github.com/siroio/ragrep/internal/store"
)

type documentSearchRequest struct {
	DB    string
	Query string
	Mode  string
	K     int
	Tags  []string
}

type documentSearcher interface {
	SearchDocuments(context.Context, documentSearchRequest) ([]store.Hit, error)
}

type documentStoreEntry struct {
	path     string
	store    *store.Store
	err      error
	ready    chan struct{}
	released chan struct{}
	opening  bool
	waiters  int
	leases   int
}

type documentService struct {
	mu         sync.Mutex
	embeddings *embeddingPool
	open       func(string) (*store.Store, error)
	entries    map[string]*documentStoreEntry
	closed     bool
	closeDone  chan struct{}
	closeErr   error
}

func newDocumentService(embeddings *embeddingPool, open func(string) (*store.Store, error)) *documentService {
	if open == nil {
		open = openStoreAt
	}
	return &documentService{
		embeddings: embeddings,
		open:       open,
		entries:    make(map[string]*documentStoreEntry),
		closeDone:  make(chan struct{}),
	}
}

func (s *documentService) findEntryLocked(path string) (string, *documentStoreEntry) {
	key := canonicalCodeDBKey(path)
	if entry := s.entries[key]; entry != nil {
		return key, entry
	}
	for candidate, entry := range s.entries {
		if sameCodeDB(candidate, path) {
			return candidate, entry
		}
	}
	return key, nil
}

func (s *documentService) acquire(ctx context.Context, db string) (*store.Store, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	path, err := filepath.Abs(db)
	if err != nil {
		return nil, nil, err
	}
	for {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return nil, nil, errors.New("document service is closed")
		}
		key, entry := s.findEntryLocked(path)
		created := entry == nil
		waiting := false
		if created {
			released := make(chan struct{})
			close(released)
			entry = &documentStoreEntry{path: path, ready: make(chan struct{}), released: released, opening: true}
			s.entries[key] = entry
		} else if entry.opening {
			entry.waiters++
			waiting = true
		}
		s.mu.Unlock()

		if created {
			opened, openErr := s.open(path)
			s.mu.Lock()
			entry.store = opened
			entry.err = openErr
			entry.opening = false
			close(entry.ready)
			if openErr != nil && s.entries[key] == entry {
				delete(s.entries, key)
			}
			s.mu.Unlock()
		}

		var waitErr error
		select {
		case <-ctx.Done():
			waitErr = ctx.Err()
		case <-entry.ready:
		}
		if waiting {
			s.mu.Lock()
			entry.waiters--
			s.mu.Unlock()
		}
		if waitErr != nil {
			return nil, nil, waitErr
		}
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}

		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return nil, nil, errors.New("document service is closed")
		}
		if entry.err != nil {
			s.mu.Unlock()
			return nil, nil, entry.err
		}
		if err := ctx.Err(); err != nil {
			s.mu.Unlock()
			return nil, nil, err
		}
		if entry.leases == 0 {
			entry.released = make(chan struct{})
		}
		entry.leases++
		s.mu.Unlock()

		var once sync.Once
		release := func() {
			once.Do(func() {
				s.mu.Lock()
				entry.leases--
				if entry.leases == 0 {
					close(entry.released)
				}
				s.mu.Unlock()
			})
		}
		return entry.store, release, nil
	}
}

func (s *documentService) SearchDocuments(ctx context.Context, req documentSearchRequest) ([]store.Hit, error) {
	db, release, err := s.acquire(ctx, req.DB)
	if err != nil {
		return nil, err
	}
	defer release()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return runDocumentSearch(ctx, db, req.Mode, req.Query, req.K, req.Tags, func(ctx context.Context, query string) ([]float32, error) {
		if s.embeddings == nil {
			return nil, errors.New("document embedding pool is unavailable")
		}
		return s.embeddings.Embed(ctx, query)
	})
}

func (s *documentService) Close() error {
	s.mu.Lock()
	if s.closed {
		done := s.closeDone
		s.mu.Unlock()
		<-done
		s.mu.Lock()
		err := s.closeErr
		s.mu.Unlock()
		return err
	}
	s.closed = true
	entries := make([]*documentStoreEntry, 0, len(s.entries))
	for _, entry := range s.entries {
		entries = append(entries, entry)
	}
	s.mu.Unlock()

	for _, entry := range entries {
		<-entry.ready
		<-entry.released
	}
	var errs []error
	for _, entry := range entries {
		if entry.store != nil {
			errs = append(errs, entry.store.Close())
		}
	}

	s.mu.Lock()
	s.closeErr = errors.Join(errs...)
	close(s.closeDone)
	s.mu.Unlock()
	return s.closeErr
}

// Package session keeps an already-open store and embedder available for
// sequential local search requests.
package session

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/siroio/ragrep/internal/store"
)

type Embedder interface {
	Embed(string) ([]float32, error)
	Close()
}

type ContentStore interface {
	GetDoc(string) (string, error)
	GetParas(string, int, int) (string, error)
}

type Session struct {
	store    *store.Store
	embedder Embedder
	mu       sync.Mutex
	closed   bool
	closeErr error
}

func New(s *store.Store, e Embedder) *Session {
	return &Session{store: s, embedder: e}
}

func (s *Session) Search(mode, query string, k int, tags []string) ([]store.Hit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("session is closed")
	}
	switch mode {
	case "text":
		return s.store.SearchText(query, k, tags)
	case "vector", "hybrid":
		qv, err := s.embedder.Embed("task: search result | query: " + query)
		if err != nil {
			return nil, err
		}
		if mode == "vector" {
			return s.store.SearchVector(qv, k, tags)
		}
		return s.store.SearchHybrid(query, qv, k, tags)
	default:
		return nil, fmt.Errorf("unknown mode %q", mode)
	}
}

func (s *Session) Get(path string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return "", errors.New("session is closed")
	}
	return s.store.GetDoc(path)
}

func (s *Session) GetContent(path, lines string, para, context int) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return "", errors.New("session is closed")
	}
	return GetContent(s.store, path, lines, para, context)
}

func (s *Session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return s.closeErr
	}
	s.closed = true
	s.embedder.Close()
	s.closeErr = s.store.Close()
	return s.closeErr
}

func GetContent(s ContentStore, path, lines string, para, context int) (string, error) {
	switch {
	case lines != "":
		var a, b int
		if _, err := fmt.Sscanf(lines, "%d-%d", &a, &b); err != nil || a < 1 || b < a {
			return "", fmt.Errorf("invalid --lines %q (want A-B)", lines)
		}
		doc, err := s.GetDoc(path)
		if err != nil {
			return "", err
		}
		parts := strings.Split(strings.ReplaceAll(doc, "\r\n", "\n"), "\n")
		if a > len(parts) {
			return "", store.ErrNotFound
		}
		if b > len(parts) {
			b = len(parts)
		}
		return strings.Join(parts[a-1:b], "\n"), nil
	case para >= 0:
		return s.GetParas(path, para, context)
	default:
		return s.GetDoc(path)
	}
}

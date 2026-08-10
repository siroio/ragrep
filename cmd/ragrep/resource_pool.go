package main

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"time"

	"github.com/siroio/ragrep/internal/config"
	"github.com/siroio/ragrep/internal/embed"
	"github.com/siroio/ragrep/internal/lsp"
)

const defaultLSPIdleTimeout = 5 * time.Minute

type textEmbedder interface {
	Embed(string) ([]float32, error)
	Close()
}

type embeddingPool struct {
	mu          contextMutex
	stateMu     sync.Mutex
	constructor func() (textEmbedder, error)
	embedder    textEmbedder
	closed      bool
	active      bool
}

func newEmbeddingPool(constructor func() (textEmbedder, error)) *embeddingPool {
	if constructor == nil {
		constructor = func() (textEmbedder, error) {
			dir, err := embed.CacheDir()
			if err != nil {
				return nil, err
			}
			return embed.New(dir)
		}
	}
	return &embeddingPool{constructor: constructor}
}

func (p *embeddingPool) Embed(ctx context.Context, text string) ([]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.stateMu.Lock()
	closed := p.closed
	p.stateMu.Unlock()
	if closed {
		return nil, errors.New("embedding pool is closed")
	}
	if err := p.mu.LockContext(ctx); err != nil {
		return nil, err
	}
	p.stateMu.Lock()
	if p.closed {
		p.stateMu.Unlock()
		p.mu.Unlock()
		return nil, errors.New("embedding pool is closed")
	}
	if p.embedder == nil {
		embedder, err := p.constructor()
		if err != nil {
			p.stateMu.Unlock()
			p.mu.Unlock()
			return nil, err
		}
		p.embedder = embedder
	}
	embedder := p.embedder
	p.active = true
	p.stateMu.Unlock()

	type result struct {
		vector []float32
		err    error
	}
	done := make(chan result, 1)
	go func() {
		vector, err := embedder.Embed(text)
		p.stateMu.Lock()
		p.active = false
		closeEmbedder := p.closed && p.embedder == embedder
		if closeEmbedder {
			p.embedder = nil
		}
		p.stateMu.Unlock()
		if closeEmbedder {
			embedder.Close()
		}
		p.mu.Unlock()
		done <- result{vector: vector, err: err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case result := <-done:
		return result.vector, result.err
	}
}

func (p *embeddingPool) Close() error {
	p.stateMu.Lock()
	if p.closed {
		p.stateMu.Unlock()
		return nil
	}
	p.closed = true
	if p.active {
		p.stateMu.Unlock()
		return nil
	}
	embedder := p.embedder
	p.embedder = nil
	p.stateMu.Unlock()
	if embedder != nil {
		embedder.Close()
	}
	return nil
}

type pooledLanguageServer struct {
	client                    *lsp.Client
	serverName, serverVersion string
	close                     func() error
}

type lspPoolEntry struct {
	server          *pooledLanguageServer
	leases          int
	timer           *time.Timer
	timerGeneration uint64
}

type lspPoolKey struct {
	root, language string
}

type lspPool struct {
	mu          contextMutex
	idle        time.Duration
	constructor func(context.Context, string, string) (*pooledLanguageServer, error)
	entries     map[lspPoolKey]*lspPoolEntry
	closed      bool
}

func newLSPPool(idle time.Duration, constructor func(context.Context, string, string) (*pooledLanguageServer, error)) *lspPool {
	if idle == 0 {
		idle = defaultLSPIdleTimeout
	}
	if constructor == nil {
		constructor = func(ctx context.Context, root, language string) (*pooledLanguageServer, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			cfg, err := config.Load(root)
			if err != nil {
				return nil, err
			}
			command, err := cfg.ServerCommand(language)
			if err != nil {
				return nil, err
			}
			client, initResult, err := startLanguageServer(ctx, command, root)
			if err != nil {
				return nil, err
			}
			name, version := serverIdentity(initResult)
			return &pooledLanguageServer{client: client, serverName: name, serverVersion: version, close: client.Close}, nil
		}
	}
	return &lspPool{
		idle:        idle,
		constructor: constructor,
		entries:     make(map[lspPoolKey]*lspPoolEntry),
	}
}

func (p *lspPool) Acquire(ctx context.Context, root, language string) (*lsp.Client, func(), error) {
	client, _, _, release, err := p.AcquireWithMetadata(ctx, root, language)
	return client, release, err
}

func (p *lspPool) AcquireWithMetadata(ctx context.Context, root, language string) (*lsp.Client, string, string, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, "", "", nil, err
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, "", "", nil, err
	}
	key := lspPoolKey{root: root, language: language}
	if err := p.mu.LockContext(ctx); err != nil {
		return nil, "", "", nil, err
	}
	defer p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, "", "", nil, err
	}
	if p.closed {
		return nil, "", "", nil, errors.New("LSP pool is closed")
	}
	entry := p.entries[key]
	if entry == nil {
		server, err := p.constructor(ctx, root, language)
		if err != nil {
			return nil, "", "", nil, err
		}
		entry = &lspPoolEntry{server: server}
		p.entries[key] = entry
	} else if entry.timer != nil {
		entry.timer.Stop()
		entry.timer = nil
	}
	entry.leases++
	var once sync.Once
	release := func() {
		once.Do(func() { p.release(key, entry) })
	}
	return entry.server.client, entry.server.serverName, entry.server.serverVersion, release, nil
}

func (p *lspPool) release(key lspPoolKey, entry *lspPoolEntry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.entries[key] != entry || entry.leases == 0 {
		return
	}
	entry.leases--
	if entry.leases == 0 {
		entry.timerGeneration++
		generation := entry.timerGeneration
		entry.timer = time.AfterFunc(p.idle, func() { p.closeIdle(key, entry, generation) })
	}
}

func (p *lspPool) closeIdle(key lspPoolKey, entry *lspPoolEntry, generation uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.entries[key] != entry || entry.leases != 0 || entry.timerGeneration != generation {
		return
	}
	delete(p.entries, key)
	entry.timer = nil
	if entry.server.close != nil {
		_ = entry.server.close()
	}
}

func (p *lspPool) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	entries := p.entries
	p.entries = make(map[lspPoolKey]*lspPoolEntry)
	p.mu.Unlock()

	var errs []error
	for _, entry := range entries {
		if entry.timer != nil {
			entry.timer.Stop()
		}
		if entry.server != nil && entry.server.close != nil {
			errs = append(errs, entry.server.close())
		}
	}
	return errors.Join(errs...)
}

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
	mu          sync.Mutex
	constructor func() (textEmbedder, error)
	embedder    textEmbedder
	closed      bool
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
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, errors.New("embedding pool is closed")
	}
	if p.embedder == nil {
		embedder, err := p.constructor()
		if err != nil {
			return nil, err
		}
		p.embedder = embedder
	}
	return p.embedder.Embed(text)
}

func (p *embeddingPool) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	if p.embedder != nil {
		p.embedder.Close()
	}
	return nil
}

type pooledLanguageServer struct {
	client *lsp.Client
	close  func() error
}

type lspPoolEntry struct {
	server *pooledLanguageServer
	leases int
	timer  *time.Timer
}

type lspPoolKey struct {
	root, language string
}

type lspPool struct {
	mu          sync.Mutex
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
			client, _, err := startLanguageServer(command, root)
			if err != nil {
				return nil, err
			}
			return &pooledLanguageServer{client: client, close: client.Close}, nil
		}
	}
	return &lspPool{
		idle:        idle,
		constructor: constructor,
		entries:     make(map[lspPoolKey]*lspPoolEntry),
	}
}

func (p *lspPool) Acquire(ctx context.Context, root, language string) (*lsp.Client, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, nil, err
	}
	key := lspPoolKey{root: root, language: language}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, nil, errors.New("LSP pool is closed")
	}
	entry := p.entries[key]
	if entry == nil {
		server, err := p.constructor(ctx, root, language)
		if err != nil {
			return nil, nil, err
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
	return entry.server.client, release, nil
}

func (p *lspPool) release(key lspPoolKey, entry *lspPoolEntry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.entries[key] != entry || entry.leases == 0 {
		return
	}
	entry.leases--
	if entry.leases == 0 {
		entry.timer = time.AfterFunc(p.idle, func() { p.closeIdle(key, entry) })
	}
}

func (p *lspPool) closeIdle(key lspPoolKey, entry *lspPoolEntry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.entries[key] != entry || entry.leases != 0 {
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

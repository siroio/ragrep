package main

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/siroio/ragrep/internal/lsp"
)

type blockingTextEmbedder struct {
	active, maxActive *atomic.Int32
}

func (e *blockingTextEmbedder) Embed(string) ([]float32, error) {
	active := e.active.Add(1)
	for max := e.maxActive.Load(); active > max && !e.maxActive.CompareAndSwap(max, active); max = e.maxActive.Load() {
	}
	time.Sleep(20 * time.Millisecond)
	e.active.Add(-1)
	return []float32{1}, nil
}

func (*blockingTextEmbedder) Close() {}

func TestEmbeddingPoolConstructsOnceAndSerializes(t *testing.T) {
	var constructed, active, maxActive atomic.Int32
	p := newEmbeddingPool(func() (textEmbedder, error) {
		constructed.Add(1)
		return &blockingTextEmbedder{active: &active, maxActive: &maxActive}, nil
	})

	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, text := range []string{"workspace-a", "workspace-b"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := p.Embed(context.Background(), text); err != nil {
				t.Errorf("Embed: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if constructed.Load() != 1 || maxActive.Load() != 1 {
		t.Fatalf("constructed=%d maxActive=%d", constructed.Load(), maxActive.Load())
	}
}

func TestLSPPoolReusesOverlappingLease(t *testing.T) {
	var constructed atomic.Int32
	p := newLSPPool(time.Hour, func(context.Context, string, string) (*pooledLanguageServer, error) {
		constructed.Add(1)
		return &pooledLanguageServer{client: new(lsp.Client)}, nil
	})
	t.Cleanup(func() { _ = p.Close() })

	root := t.TempDir()
	first, releaseFirst, err := p.Acquire(context.Background(), root, "go")
	if err != nil {
		t.Fatal(err)
	}
	second, releaseSecond, err := p.Acquire(context.Background(), root, "go")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseFirst()
	defer releaseSecond()

	if first != second || constructed.Load() != 1 {
		t.Fatalf("same client=%v constructed=%d", first == second, constructed.Load())
	}
}

func TestLSPPoolAcquireWithMetadataPreservesServerIdentity(t *testing.T) {
	client := new(lsp.Client)
	p := newLSPPool(time.Hour, func(context.Context, string, string) (*pooledLanguageServer, error) {
		return &pooledLanguageServer{client: client, serverName: "fake-lsp", serverVersion: "v1.2.3"}, nil
	})
	t.Cleanup(func() { _ = p.Close() })

	got, name, version, release, err := p.AcquireWithMetadata(context.Background(), t.TempDir(), "go")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if got != client || name != "fake-lsp" || version != "v1.2.3" {
		t.Fatalf("client=%p name=%q version=%q", got, name, version)
	}
}

func TestLSPPoolDoesNotShareAcrossRoots(t *testing.T) {
	var constructed atomic.Int32
	p := newLSPPool(time.Hour, func(context.Context, string, string) (*pooledLanguageServer, error) {
		constructed.Add(1)
		return &pooledLanguageServer{client: new(lsp.Client)}, nil
	})
	t.Cleanup(func() { _ = p.Close() })

	first, releaseFirst, err := p.Acquire(context.Background(), t.TempDir(), "go")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseFirst()
	second, releaseSecond, err := p.Acquire(context.Background(), t.TempDir(), "go")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseSecond()

	if first == second || constructed.Load() != 2 {
		t.Fatalf("same client=%v constructed=%d", first == second, constructed.Load())
	}
}

func TestLSPPoolRetriesConstructorFailure(t *testing.T) {
	wantErr := errors.New("transient start failure")
	var attempts atomic.Int32
	p := newLSPPool(time.Hour, func(context.Context, string, string) (*pooledLanguageServer, error) {
		if attempts.Add(1) == 1 {
			return nil, wantErr
		}
		return &pooledLanguageServer{client: new(lsp.Client)}, nil
	})
	t.Cleanup(func() { _ = p.Close() })

	root := t.TempDir()
	if _, _, err := p.Acquire(context.Background(), root, "go"); !errors.Is(err, wantErr) {
		t.Fatalf("first Acquire error=%v, want %v", err, wantErr)
	}
	_, release, err := p.Acquire(context.Background(), root, "go")
	if err != nil {
		t.Fatalf("second Acquire: %v", err)
	}
	release()
	if attempts.Load() != 2 {
		t.Fatalf("attempts=%d, want 2", attempts.Load())
	}
}

func TestLSPPoolClosesIdleClientOnce(t *testing.T) {
	var closeCount atomic.Int32
	closed := make(chan struct{}, 1)
	p := newLSPPool(20*time.Millisecond, func(context.Context, string, string) (*pooledLanguageServer, error) {
		return &pooledLanguageServer{
			client: new(lsp.Client),
			close: func() error {
				closeCount.Add(1)
				closed <- struct{}{}
				return nil
			},
		}, nil
	})

	_, release, err := p.Acquire(context.Background(), t.TempDir(), "go")
	if err != nil {
		t.Fatal(err)
	}
	release()
	select {
	case <-closed:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("idle client was not closed")
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if closeCount.Load() != 1 {
		t.Fatalf("close count=%d, want 1", closeCount.Load())
	}
}

func TestLSPPoolStaleIdleCallbackDoesNotCloseReacquiredClient(t *testing.T) {
	var closeCount atomic.Int32
	p := newLSPPool(time.Hour, func(context.Context, string, string) (*pooledLanguageServer, error) {
		return &pooledLanguageServer{
			client: new(lsp.Client),
			close: func() error {
				closeCount.Add(1)
				return nil
			},
		}, nil
	})
	t.Cleanup(func() { _ = p.Close() })

	root, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, release, err := p.Acquire(context.Background(), root, "go")
	if err != nil {
		t.Fatal(err)
	}
	release()
	key := lspPoolKey{root: root, language: "go"}
	entry := p.entries[key]
	staleGeneration := entry.timerGeneration

	_, release, err = p.Acquire(context.Background(), root, "go")
	if err != nil {
		t.Fatal(err)
	}
	release()
	p.closeIdle(key, entry, staleGeneration)

	if p.entries[key] != entry || closeCount.Load() != 0 {
		t.Fatalf("entry retained=%v close count=%d", p.entries[key] == entry, closeCount.Load())
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
}

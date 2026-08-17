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

type lifecycleTextEmbedder struct {
	started    chan struct{}
	release    chan struct{}
	startOnce  sync.Once
	closeCount atomic.Int32
}

func (e *lifecycleTextEmbedder) Embed(string) ([]float32, error) {
	e.startOnce.Do(func() { close(e.started) })
	<-e.release
	return []float32{1}, nil
}

func (e *lifecycleTextEmbedder) Close() { e.closeCount.Add(1) }

type cancellationAfterFirstCheckContext struct {
	context.Context
	checked chan struct{}
	once    sync.Once
}

func (c *cancellationAfterFirstCheckContext) Err() error {
	first := false
	c.once.Do(func() {
		first = true
		close(c.checked)
	})
	if first {
		return nil
	}
	return c.Context.Err()
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

func TestEmbeddingPoolCancellationWhileWaitingSkipsEmbedder(t *testing.T) {
	var calls atomic.Int32
	p := newEmbeddingPool(func() (textEmbedder, error) {
		return textEmbedderFunc(func(string) ([]float32, error) {
			calls.Add(1)
			return []float32{1}, nil
		}), nil
	})
	base, cancel := context.WithCancel(context.Background())
	ctx := &cancellationAfterFirstCheckContext{Context: base, checked: make(chan struct{})}
	p.mu.Lock()
	done := make(chan error, 1)
	go func() {
		_, err := p.Embed(ctx, "canceled")
		done <- err
	}()
	<-ctx.checked
	cancel()
	p.mu.Unlock()

	if err := <-done; !errors.Is(err, context.Canceled) || calls.Load() != 0 {
		t.Fatalf("error=%v embed calls=%d, want canceled without embedding", err, calls.Load())
	}
}

type textEmbedderFunc func(string) ([]float32, error)

func (f textEmbedderFunc) Embed(text string) ([]float32, error) { return f(text) }
func (textEmbedderFunc) Close()                                 {}

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

func TestWorkspaceAndServiceCloseDoNotWaitForActiveEmbedding(t *testing.T) {
	embedder := &lifecycleTextEmbedder{started: make(chan struct{}), release: make(chan struct{})}
	p := newEmbeddingPool(func() (textEmbedder, error) { return embedder, nil })
	svc := newCodeService(nil, p, nil)
	w := newTestWorkspace(t, "func BlockingEmbedding() {}")
	w.setConfirmation(func(ctx context.Context, _, _ string) error {
		_, err := p.Embed(ctx, "blocking")
		return err
	})
	<-embedder.started

	workspaceClosed := make(chan error, 1)
	go func() { workspaceClosed <- w.Close() }()
	var workspaceErr error
	workspacePrompt := false
	select {
	case workspaceErr = <-workspaceClosed:
		workspacePrompt = true
	case <-time.After(200 * time.Millisecond):
	}
	serviceClosed := make(chan error, 1)
	go func() { serviceClosed <- svc.Close() }()
	var serviceErr error
	servicePrompt := false
	select {
	case serviceErr = <-serviceClosed:
		servicePrompt = true
	case <-time.After(200 * time.Millisecond):
	}
	rejected := make(chan error, 1)
	go func() {
		_, err := p.Embed(context.Background(), "after-close")
		rejected <- err
	}()
	var rejectedErr error
	rejectedPrompt := false
	select {
	case rejectedErr = <-rejected:
		rejectedPrompt = true
	case <-time.After(200 * time.Millisecond):
	}
	if embedder.closeCount.Load() != 0 {
		t.Fatalf("active embedder close count=%d, want 0 before worker exits", embedder.closeCount.Load())
	}
	close(embedder.release)
	if !workspacePrompt {
		workspaceErr = <-workspaceClosed
	}
	if !servicePrompt {
		serviceErr = <-serviceClosed
	}
	if !rejectedPrompt {
		rejectedErr = <-rejected
	}
	if !workspacePrompt || workspaceErr != nil {
		t.Fatalf("workspace Close prompt=%v err=%v", workspacePrompt, workspaceErr)
	}
	if !servicePrompt || serviceErr != nil {
		t.Fatalf("service Close prompt=%v err=%v", servicePrompt, serviceErr)
	}
	if !rejectedPrompt || rejectedErr == nil {
		t.Fatalf("post-Close Embed prompt=%v err=%v", rejectedPrompt, rejectedErr)
	}
	deadline := time.Now().Add(time.Second)
	for embedder.closeCount.Load() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if embedder.closeCount.Load() != 1 {
		t.Fatalf("embedder close count=%d, want 1 after worker exits", embedder.closeCount.Load())
	}
	if err := svc.Close(); err != nil || embedder.closeCount.Load() != 1 {
		t.Fatalf("second Close err=%v close count=%d", err, embedder.closeCount.Load())
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

func TestLSPPoolCancellationWhileWaitingSkipsLease(t *testing.T) {
	var constructed atomic.Int32
	p := newLSPPool(time.Hour, func(context.Context, string, string) (*pooledLanguageServer, error) {
		constructed.Add(1)
		return &pooledLanguageServer{client: new(lsp.Client)}, nil
	})
	base, cancel := context.WithCancel(context.Background())
	ctx := &cancellationAfterFirstCheckContext{Context: base, checked: make(chan struct{})}
	p.mu.Lock()
	done := make(chan error, 1)
	go func() {
		_, _, err := p.Acquire(ctx, t.TempDir(), "go")
		done <- err
	}()
	<-ctx.checked
	cancel()
	p.mu.Unlock()

	if err := <-done; !errors.Is(err, context.Canceled) || constructed.Load() != 0 || len(p.entries) != 0 {
		t.Fatalf("error=%v constructed=%d entries=%d, want canceled without lease", err, constructed.Load(), len(p.entries))
	}
}

func TestWorkspaceCloseCancelsConfirmationWaitingForOtherLSPConstructor(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	constructorStarted := make(chan struct{})
	releaseConstructor := make(chan struct{})
	p := newLSPPool(time.Hour, func(_ context.Context, root, _ string) (*pooledLanguageServer, error) {
		if root == rootA {
			close(constructorStarted)
			<-releaseConstructor
		}
		return &pooledLanguageServer{client: new(lsp.Client)}, nil
	})
	first := make(chan error, 1)
	go func() {
		_, release, err := p.Acquire(context.Background(), rootA, "go")
		if err == nil {
			release()
		}
		first <- err
	}()
	<-constructorStarted

	w := newTestWorkspace(t, "func WaitingForLSP() {}")
	confirmationStarted := make(chan struct{})
	w.setConfirmation(func(ctx context.Context, _, _ string) error {
		close(confirmationStarted)
		_, release, err := p.Acquire(ctx, rootB, "go")
		if err == nil {
			release()
		}
		return err
	})
	<-confirmationStarted
	closed := make(chan error, 1)
	go func() { closed <- w.Close() }()
	prompt := false
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
		prompt = true
	case <-time.After(200 * time.Millisecond):
	}
	close(releaseConstructor)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if !prompt {
		if err := <-closed; err != nil {
			t.Fatal(err)
		}
		t.Fatal("workspace Close waited for another workspace's LSP constructor")
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
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

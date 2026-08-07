package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/siroio/ragrep/internal/codeindex"
	"github.com/siroio/ragrep/internal/codestore"
	"github.com/siroio/ragrep/internal/lsp"
)

type fakeDaemonCodeService struct {
	search func(context.Context, searchRequest) (searchResponse, error)
	get    func(context.Context, getRequest) (codeindex.Symbol, error)
}

type closeFunc func() error

func (f closeFunc) Close() error { return f() }

func (s fakeDaemonCodeService) Search(ctx context.Context, req searchRequest) (searchResponse, error) {
	return s.search(ctx, req)
}

func (s fakeDaemonCodeService) Get(ctx context.Context, req getRequest) (codeindex.Symbol, error) {
	return s.get(ctx, req)
}

func TestDaemonRejectsMissingAndWrongToken(t *testing.T) {
	h := newDaemonHandler(fakeDaemonCodeService{search: func(context.Context, searchRequest) (searchResponse, error) {
		t.Fatal("unauthenticated request reached service")
		return searchResponse{}, nil
	}}, "secret")
	for _, authorization := range []string{"", "Bearer wrong"} {
		req := httptest.NewRequest(http.MethodPost, "/v1/code/search", strings.NewReader(`{"query":"Handler"}`))
		req.Header.Set("Authorization", authorization)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("authorization=%q status=%d, want 401", authorization, rr.Code)
		}
		var got apiError
		if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil || got.Code != "unauthorized" || got.Retryable {
			t.Fatalf("authorization=%q error=%+v decode=%v", authorization, got, err)
		}
	}
}

func TestDaemonRejectsMalformedJSON(t *testing.T) {
	for _, body := range []string{`{"query":`, `{"query":"Handler"} trailing`} {
		t.Run(body, func(t *testing.T) {
			called := false
			h := newDaemonHandler(fakeDaemonCodeService{search: func(context.Context, searchRequest) (searchResponse, error) {
				called = true
				return searchResponse{}, nil
			}}, "secret")
			req := httptest.NewRequest(http.MethodPost, "/v1/code/search", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer secret")
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusBadRequest || called {
				t.Fatalf("status=%d called=%v body=%s", rr.Code, called, rr.Body.String())
			}
		})
	}
}

func TestDaemonUnknownWorkspaceReturnsTypedError(t *testing.T) {
	h := newDaemonHandler(fakeDaemonCodeService{search: func(context.Context, searchRequest) (searchResponse, error) {
		return searchResponse{}, ErrWorkspaceNotFound
	}}, "secret")
	req := httptest.NewRequest(http.MethodPost, "/v1/code/search", strings.NewReader(`{"root":"missing","query":"Handler"}`))
	req.Header.Set("Authorization", "Bearer secret")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	var got apiError
	if rr.Code != http.StatusNotFound || json.Unmarshal(rr.Body.Bytes(), &got) != nil || got.Code != "workspace_not_found" || got.Retryable {
		t.Fatalf("status=%d error=%+v", rr.Code, got)
	}
}

func TestDaemonSearchReturnsFreshGeneration(t *testing.T) {
	h := newDaemonHandler(fakeDaemonCodeService{search: func(_ context.Context, req searchRequest) (searchResponse, error) {
		if req.Root != "root" || req.Query != "Handler" || req.K != 3 || req.Mode != "text" {
			t.Fatalf("request=%+v", req)
		}
		return searchResponse{Fresh: true, Generation: 7}, nil
	}}, "secret")
	req := httptest.NewRequest(http.MethodPost, "/v1/code/search", strings.NewReader(`{"root":"root","query":"Handler","k":3,"mode":"text"}`))
	req.Header.Set("Authorization", "Bearer secret")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	var got searchResponse
	if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &got) != nil || !got.Fresh || got.Generation != 7 {
		t.Fatalf("status=%d response=%+v body=%s", rr.Code, got, rr.Body.String())
	}
}

func TestDaemonGetReturnsTypedStaleError(t *testing.T) {
	h := newDaemonHandler(fakeDaemonCodeService{
		search: func(context.Context, searchRequest) (searchResponse, error) { return searchResponse{}, nil },
		get: func(_ context.Context, req getRequest) (codeindex.Symbol, error) {
			if req.Root != "root" || req.Key != "live:old" || !req.Body {
				t.Fatalf("request=%+v", req)
			}
			return codeindex.Symbol{}, ErrStaleLiveKey
		},
	}, "secret")
	ts := httptest.NewServer(h)
	defer ts.Close()
	client := daemonClient{endpoint: ts.URL, token: "secret", client: ts.Client()}
	_, err := client.Get(context.Background(), getRequest{Root: "root", Key: "live:old", Body: true})
	var apiErr *apiError
	if !errors.As(err, &apiErr) || apiErr.Code != "stale_live_key" {
		t.Fatalf("error=%T %v", err, err)
	}
}

func TestDaemonMapsOperationalErrors(t *testing.T) {
	tests := []struct {
		err       error
		status    int
		code      string
		retryable bool
	}{
		{ErrWorkspaceSyncing, http.StatusConflict, "workspace_syncing", true},
		{ErrStaleLiveKey, http.StatusConflict, "stale_live_key", false},
		{codestore.ErrNotFound, http.StatusNotFound, "not_found", false},
		{codestore.ErrReindexRequired, http.StatusConflict, "reindex_required", false},
		{&lsp.ResponseError{Code: lsp.ErrCodeInternalError, Message: "failed"}, http.StatusBadGateway, "lsp_error", true},
	}
	for _, tt := range tests {
		t.Run(tt.code, func(t *testing.T) {
			h := newDaemonHandler(fakeDaemonCodeService{search: func(context.Context, searchRequest) (searchResponse, error) {
				return searchResponse{}, tt.err
			}}, "secret")
			req := httptest.NewRequest(http.MethodPost, "/v1/code/search", strings.NewReader(`{"query":"Handler"}`))
			req.Header.Set("Authorization", "Bearer secret")
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			var got apiError
			if rr.Code != tt.status || json.Unmarshal(rr.Body.Bytes(), &got) != nil || got.Code != tt.code || got.Retryable != tt.retryable {
				t.Fatalf("status=%d error=%+v", rr.Code, got)
			}
		})
	}
}

func TestDaemonClientPreservesTypedNotFound(t *testing.T) {
	h := newDaemonHandler(fakeDaemonCodeService{
		search: func(context.Context, searchRequest) (searchResponse, error) { return searchResponse{}, nil },
		get: func(context.Context, getRequest) (codeindex.Symbol, error) {
			return codeindex.Symbol{}, codestore.ErrNotFound
		},
	}, "secret")
	ts := httptest.NewServer(h)
	defer ts.Close()
	_, err := (daemonClient{endpoint: ts.URL, token: "secret", client: ts.Client()}).Get(context.Background(), getRequest{})
	var apiErr *apiError
	if !errors.As(err, &apiErr) || apiErr.Code != "not_found" || apiErr.Retryable {
		t.Fatalf("error=%T %v", err, err)
	}
}

func TestDaemonStopRejectsMalformedJSON(t *testing.T) {
	called := false
	h := newDaemonServerHandler(nil, "secret", nil, func() { called = true })
	req := httptest.NewRequest(http.MethodPost, "/v1/stop", strings.NewReader(`{`))
	req.Header.Set("Authorization", "Bearer secret")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest || called {
		t.Fatalf("status=%d called=%v body=%s", rr.Code, called, rr.Body.String())
	}
}

func TestDaemonStatusAndTypedClientError(t *testing.T) {
	h := newDaemonHandler(fakeDaemonCodeService{search: func(context.Context, searchRequest) (searchResponse, error) {
		return searchResponse{}, ErrWorkspaceNotFound
	}}, "secret")
	ts := httptest.NewServer(h)
	defer ts.Close()
	client := daemonClient{endpoint: ts.URL, token: "secret", client: ts.Client()}
	status, err := client.Status(context.Background())
	if err != nil || status.Status != "running" || status.PID != os.Getpid() {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	_, err = client.Search(context.Background(), searchRequest{Root: "missing", Query: "Handler"})
	var apiErr *apiError
	if !errors.As(err, &apiErr) || apiErr.Code != "workspace_not_found" {
		t.Fatalf("error=%T %v", err, err)
	}
}

func TestDaemonTokenAndDiscoveryFile(t *testing.T) {
	token, err := newDaemonToken()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(token)
	if err != nil || len(raw) != 32 {
		t.Fatalf("token bytes=%d decode=%v", len(raw), err)
	}
	path := filepath.Join(t.TempDir(), "daemon.json")
	want := daemonDiscovery{Endpoint: "http://127.0.0.1:7377", Token: token, PID: 42}
	if err := writeDaemonDiscovery(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := readDaemonDiscovery(path)
	if err != nil || got != want {
		t.Fatalf("discovery=%+v err=%v", got, err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("mode=%v err=%v", info.Mode().Perm(), err)
		}
	}
}

func TestDaemonListenerIsSingleton(t *testing.T) {
	first, err := listenDaemon("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := listenDaemon(first.Addr().String())
	if err == nil {
		second.Close()
		t.Fatal("second listener unexpectedly succeeded")
	}
}

func TestDaemonServiceSharesEmbedderWithoutCrossingWorkspaces(t *testing.T) {
	first := newTestWorkspace(t, "package sample\nfunc WorkspaceOneOnly() {}\n")
	second := newTestWorkspace(t, "package sample\nfunc WorkspaceTwoOnly() {}\n")
	var constructors atomic.Int32
	embedder := new(serviceTestEmbedder)
	service := newCodeService(func(root string) (*workspaceState, error) {
		switch root {
		case first.root:
			return first.workspaceState, nil
		case second.root:
			return second.workspaceState, nil
		default:
			return nil, ErrWorkspaceNotFound
		}
	}, newEmbeddingPool(func() (textEmbedder, error) {
		constructors.Add(1)
		return embedder, nil
	}), nil)
	t.Cleanup(func() { _ = service.Close() })

	for _, root := range []string{first.root, second.root} {
		if _, err := service.Search(context.Background(), searchRequest{Root: root, Query: "request validation"}); err != nil {
			t.Fatal(err)
		}
	}
	if constructors.Load() != 1 {
		t.Fatalf("embedder constructors=%d, want one daemon-wide instance", constructors.Load())
	}

	firstResult, err := service.Search(context.Background(), searchRequest{Root: first.root, Query: "WorkspaceOneOnly"})
	if err != nil || len(firstResult.Hits) == 0 || !firstResult.Fresh {
		t.Fatalf("first workspace result=%+v err=%v", firstResult, err)
	}
	isolated, err := service.Search(context.Background(), searchRequest{Root: second.root, Query: "WorkspaceOneOnly"})
	if err != nil || len(isolated.Hits) != 0 || !isolated.Fresh {
		t.Fatalf("second workspace leaked first result=%+v err=%v", isolated, err)
	}
}

func TestDaemonFakeLSPShutsDownWhenIdle(t *testing.T) {
	var closed atomic.Int32
	pool := newLSPPool(10*time.Millisecond, func(context.Context, string, string) (*pooledLanguageServer, error) {
		return &pooledLanguageServer{client: new(lsp.Client), close: func() error {
			closed.Add(1)
			return nil
		}}, nil
	})
	t.Cleanup(func() { _ = pool.Close() })
	_, _, _, release, err := pool.AcquireWithMetadata(context.Background(), t.TempDir(), "go")
	if err != nil {
		t.Fatal(err)
	}
	release()
	deadline := time.Now().Add(time.Second)
	for closed.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if closed.Load() != 1 {
		t.Fatalf("fake LSP close calls=%d, want 1 after idle timeout", closed.Load())
	}
}

func TestDaemonProcessHelperConfiguresChild(t *testing.T) {
	cmd := newDaemonProcessCommand(os.Args[0])
	if len(cmd.Args) != 3 || cmd.Args[0] != os.Args[0] || cmd.Args[1] != "daemon" || cmd.Args[2] != "serve" {
		t.Fatalf("args=%v", cmd.Args)
	}
	if !daemonProcessConfigured(cmd) {
		t.Fatal("daemon process was not configured for detached launch")
	}
}

func testWorkspaceRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".ragrep"), 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func testWorkspaceOpener(opened map[string]*workspaceState) workspaceOpener {
	return func(root string) (*workspaceState, error) {
		state := &workspaceState{root: root, shutdownDone: make(chan struct{})}
		opened[root] = state
		return state, nil
	}
}

func TestWorkspaceRegistryPersistsOnlyExplicitRoots(t *testing.T) {
	file := filepath.Join(t.TempDir(), "workspaces.json")
	explicitRoot := testWorkspaceRoot(t)
	autoRoot := testWorkspaceRoot(t)
	opened := map[string]*workspaceState{}
	r, err := newWorkspaceRegistry(file, 30*time.Minute, testWorkspaceOpener(opened))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if got, err := r.Add(filepath.Join(explicitRoot, ".")); err != nil || got != explicitRoot {
		t.Fatalf("add=%q err=%v", got, err)
	}
	if _, err := r.Resolve(filepath.Join(autoRoot, "subdir")); err == nil {
		// The nested path need not exist, so use the workspace root itself below.
		t.Fatal("nonexistent nested path unexpectedly resolved")
	}
	if _, err := r.Resolve(autoRoot); err != nil {
		t.Fatal(err)
	}
	r.Close()

	reloaded, err := newWorkspaceRegistry(file, 30*time.Minute, testWorkspaceOpener(map[string]*workspaceState{}))
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Close()
	got := reloaded.List()
	if len(got) != 1 || got[0] != explicitRoot {
		t.Fatalf("roots=%v, want [%s]", got, explicitRoot)
	}
}

func TestWorkspaceRegistryAutoRegistersAncestorAndEvictsIdle(t *testing.T) {
	root := testWorkspaceRoot(t)
	sub := filepath.Join(root, "pkg")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	opened := map[string]*workspaceState{}
	r, err := newWorkspaceRegistry(filepath.Join(t.TempDir(), "workspaces.json"), 20*time.Millisecond, testWorkspaceOpener(opened))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	state, err := r.Resolve(sub)
	if err != nil || state.root != root {
		t.Fatalf("root=%q err=%v", state.root, err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		state.mu.Lock()
		closed := state.closed
		state.mu.Unlock()
		if closed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("automatic workspace was not evicted")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestWorkspaceRegistryExplicitRootDoesNotAutoEvict(t *testing.T) {
	root := testWorkspaceRoot(t)
	opened := map[string]*workspaceState{}
	r, err := newWorkspaceRegistry(filepath.Join(t.TempDir(), "workspaces.json"), 20*time.Millisecond, testWorkspaceOpener(opened))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.Add(root); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	state := opened[root]
	state.mu.Lock()
	closed := state.closed
	state.mu.Unlock()
	if closed {
		t.Fatal("explicit workspace was evicted")
	}
}

func TestWorkspaceRegistryRecentAccessSurvivesOldIdleCallback(t *testing.T) {
	root := testWorkspaceRoot(t)
	r, err := newWorkspaceRegistry(filepath.Join(t.TempDir(), "workspaces.json"), 100*time.Millisecond, testWorkspaceOpener(map[string]*workspaceState{}))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	state, err := r.Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(70 * time.Millisecond)
	if _, err := r.Resolve(root); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	state.mu.Lock()
	closed := state.closed
	state.mu.Unlock()
	if closed {
		t.Fatal("old idle callback closed a recently accessed workspace")
	}
}

func TestWorkspaceRegistryRejectsOldEvictionEpoch(t *testing.T) {
	root := testWorkspaceRoot(t)
	r, err := newWorkspaceRegistry(filepath.Join(t.TempDir(), "workspaces.json"), time.Minute, testWorkspaceOpener(map[string]*workspaceState{}))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.Resolve(root); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	entry := r.entries[root]
	oldEpoch := entry.epoch
	r.scheduleEvictionLocked(root, entry)
	r.mu.Unlock()
	r.evict(root, entry, oldEpoch)
	entry.state.mu.Lock()
	closed := entry.state.closed
	entry.state.mu.Unlock()
	if closed {
		t.Fatal("old eviction epoch closed a recently accessed workspace")
	}
}

func TestDaemonRequestLeasesAutomaticWorkspace(t *testing.T) {
	root := testWorkspaceRoot(t)
	opened := map[string]*workspaceState{}
	r, err := newWorkspaceRegistry(filepath.Join(t.TempDir(), "workspaces.json"), 20*time.Millisecond, testWorkspaceOpener(opened))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	state, err := r.Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	h := newDaemonServerHandler(fakeDaemonCodeService{
		search: func(context.Context, searchRequest) (searchResponse, error) {
			time.Sleep(50 * time.Millisecond)
			state.mu.Lock()
			closed := state.closed
			state.mu.Unlock()
			if closed {
				return searchResponse{}, errors.New("workspace closed during request")
			}
			return searchResponse{Fresh: true}, nil
		},
	}, "secret", r, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/code/search", strings.NewReader(fmt.Sprintf(`{"root":%q,"query":"Handler"}`, root)))
	req.Header.Set("Authorization", "Bearer secret")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestWorkspaceRegistryRemoveWaitsForActiveRequestRelease(t *testing.T) {
	root := testWorkspaceRoot(t)
	file := filepath.Join(t.TempDir(), "workspaces.json")
	opened := map[string]*workspaceState{}
	r, err := newWorkspaceRegistry(file, time.Minute, testWorkspaceOpener(opened))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.Add(root); err != nil {
		t.Fatal(err)
	}
	state := opened[root]
	entered := make(chan struct{})
	finish := make(chan struct{})
	h := newDaemonServerHandler(fakeDaemonCodeService{
		search: func(_ context.Context, req searchRequest) (searchResponse, error) {
			close(entered)
			<-finish
			if _, err := r.Resolve(req.Root); err != nil {
				return searchResponse{}, err
			}
			return searchResponse{Fresh: true}, nil
		},
	}, "secret", r, nil)
	requestDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/code/search", strings.NewReader(fmt.Sprintf(`{"root":%q,"query":"Handler"}`, root)))
		req.Header.Set("Authorization", "Bearer secret")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		requestDone <- rr
	}()
	<-entered
	removed, err := r.Remove(root)
	if err != nil || !removed {
		t.Fatalf("removed=%v err=%v", removed, err)
	}
	if roots := r.List(); len(roots) != 0 {
		t.Fatalf("persistent roots=%v", roots)
	}
	state.mu.Lock()
	closed := state.closed
	state.mu.Unlock()
	if closed {
		t.Fatal("Remove closed workspace while request lease was active")
	}
	close(finish)
	rr := <-requestDone
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	deadline := time.Now().Add(time.Second)
	for {
		state.mu.Lock()
		closed = state.closed
		state.mu.Unlock()
		if closed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("last Release did not close removed workspace")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestWorkspaceRegistryRestoresRootsLazilyAndRemovesMissing(t *testing.T) {
	root := testWorkspaceRoot(t)
	missing := filepath.Join(t.TempDir(), "gone")
	file := filepath.Join(t.TempDir(), "workspaces.json")
	data, err := json.Marshal(workspaceRegistryFile{Roots: []string{root, missing}})
	if err != nil || os.WriteFile(file, data, 0o600) != nil {
		t.Fatal(err)
	}
	var opens atomic.Int32
	r, err := newWorkspaceRegistry(file, time.Minute, func(root string) (*workspaceState, error) {
		opens.Add(1)
		return &workspaceState{root: root, shutdownDone: make(chan struct{})}, nil
	})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	defer r.Close()
	if opens.Load() != 0 {
		t.Fatalf("restore opened %d workspaces, want lazy restore", opens.Load())
	}
	if got := r.List(); len(got) != 2 {
		t.Fatalf("roots=%v", got)
	}
	removed, err := r.Remove(missing)
	if err != nil || !removed {
		t.Fatalf("remove missing=%v err=%v", removed, err)
	}
	if _, err := r.Resolve(root); err != nil || opens.Load() != 1 {
		t.Fatalf("resolve err=%v opens=%d", err, opens.Load())
	}
}

func TestWorkspaceRegistryRestoresExplicitRootsAsynchronouslyPerRoot(t *testing.T) {
	blockedRoot := testWorkspaceRoot(t)
	workingRoot := testWorkspaceRoot(t)
	file := filepath.Join(t.TempDir(), "workspaces.json")
	data, err := json.Marshal(workspaceRegistryFile{Roots: []string{blockedRoot, workingRoot}})
	if err != nil || os.WriteFile(file, data, 0o600) != nil {
		t.Fatal(err)
	}
	unblock := make(chan struct{})
	workingOpened := make(chan struct{})
	var openedOnce sync.Once
	r, err := newWorkspaceRegistry(file, time.Minute, func(root string) (*workspaceState, error) {
		if root == blockedRoot {
			<-unblock
			return nil, errors.New("broken workspace")
		}
		openedOnce.Do(func() { close(workingOpened) })
		return &workspaceState{root: root, shutdownDone: make(chan struct{})}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	returned := make(chan struct{})
	go func() {
		r.RestoreExplicitAsync()
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("restore blocked daemon readiness")
	}
	select {
	case <-workingOpened:
	case <-time.After(time.Second):
		t.Fatal("one blocked root prevented another root from restoring")
	}
	close(unblock)
	deadline := time.Now().Add(time.Second)
	for {
		r.mu.Lock()
		blocked := r.entries[blockedRoot]
		working := r.entries[workingRoot]
		isolated := blocked != nil && blocked.state == nil && working != nil && working.state != nil
		r.mu.Unlock()
		if isolated {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("failed root was not isolated from successful restore")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestWorkspaceRegistryCloseWaitsForAsyncRestore(t *testing.T) {
	root := testWorkspaceRoot(t)
	file := filepath.Join(t.TempDir(), "workspaces.json")
	data, err := json.Marshal(workspaceRegistryFile{Roots: []string{root}})
	if err != nil || os.WriteFile(file, data, 0o600) != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	unblock := make(chan struct{})
	r, err := newWorkspaceRegistry(file, time.Minute, func(string) (*workspaceState, error) {
		close(entered)
		<-unblock
		return nil, errors.New("restore failed")
	})
	if err != nil {
		t.Fatal(err)
	}
	r.RestoreExplicitAsync()
	<-entered
	closed := make(chan struct{})
	go func() {
		_ = r.Close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("registry Close returned while restore was still active")
	case <-time.After(30 * time.Millisecond):
	}
	close(unblock)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("registry Close did not finish after restore completed")
	}
}

func TestWorkspaceRegistryCloseDeadlineRemovesDiscoveryAndClosesLateRestore(t *testing.T) {
	root := testWorkspaceRoot(t)
	file := filepath.Join(t.TempDir(), "workspaces.json")
	data, err := json.Marshal(workspaceRegistryFile{Roots: []string{root}})
	if err != nil || os.WriteFile(file, data, 0o600) != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	unblock := make(chan struct{})
	produced := make(chan *workspaceState)
	r, err := newWorkspaceRegistry(file, time.Minute, func(root string) (*workspaceState, error) {
		close(entered)
		<-unblock
		state := &workspaceState{root: root, shutdownDone: make(chan struct{})}
		produced <- state
		return state, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	r.restoreTimeout = 20 * time.Millisecond
	r.RestoreExplicitAsync()
	<-entered
	discoveryPath := filepath.Join(t.TempDir(), "daemon.json")
	if err := os.WriteFile(discoveryPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	cleaned := make(chan struct{})
	go func() {
		cleanupDaemon(discoveryPath, r)
		close(cleaned)
	}()
	select {
	case <-cleaned:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("registry Close exceeded its restore deadline")
	}
	if _, err := os.Stat(discoveryPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("discovery still exists after cleanup: %v", err)
	}
	close(unblock)
	state := <-produced
	select {
	case <-state.shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("late restored workspace was not closed")
	}
	r.mu.Lock()
	entry := r.entries[root]
	closed := r.closed
	r.mu.Unlock()
	if !closed || entry != nil {
		t.Fatalf("closed=%v late entry=%v", closed, entry)
	}
}

func TestWorkspaceRegistryAddFailureRollsBackNewEntry(t *testing.T) {
	root := testWorkspaceRoot(t)
	parentFile := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(parentFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	opened := map[string]*workspaceState{}
	r, err := newWorkspaceRegistry(filepath.Join(parentFile, "workspaces.json"), time.Minute, testWorkspaceOpener(opened))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.Add(root); err == nil {
		t.Fatal("Add unexpectedly succeeded")
	}
	if got := r.List(); len(got) != 0 {
		t.Fatalf("roots=%v", got)
	}
	state := opened[root]
	state.mu.Lock()
	closed := state.closed
	state.mu.Unlock()
	if !closed {
		t.Fatal("failed Add leaked its newly opened workspace")
	}
}

func TestWorkspaceRegistryAddFailureRestoresAutomaticTimer(t *testing.T) {
	root := testWorkspaceRoot(t)
	opened := map[string]*workspaceState{}
	r, err := newWorkspaceRegistry(filepath.Join(t.TempDir(), "workspaces.json"), 20*time.Millisecond, testWorkspaceOpener(opened))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	state, err := r.Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	parentFile := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(parentFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.file = filepath.Join(parentFile, "workspaces.json")
	if _, err := r.Add(root); err == nil {
		t.Fatal("Add unexpectedly succeeded")
	}
	deadline := time.Now().Add(time.Second)
	for {
		state.mu.Lock()
		closed := state.closed
		state.mu.Unlock()
		if closed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("failed Add did not restore automatic eviction")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestDaemonStopWaitsForDiscoveryAfterAuthenticatedStatusFails(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("LOCALAPPDATA", cache)
	t.Setenv("XDG_CACHE_HOME", cache)
	var alive atomic.Bool
	alive.Store(true)
	var statusCalls atomic.Int32
	var discoveryPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v1/stop":
			writeJSON(w, http.StatusOK, map[string]bool{"stopping": true})
			time.AfterFunc(60*time.Millisecond, func() { alive.Store(false) })
			time.AfterFunc(130*time.Millisecond, func() { _ = os.Remove(discoveryPath) })
		case "/v1/status":
			statusCalls.Add(1)
			if !alive.Load() {
				writeAPIError(w, http.StatusServiceUnavailable, &apiError{Code: "stopped", Message: "stopped"})
				return
			}
			writeJSON(w, http.StatusOK, daemonStatus{Status: "running", PID: 42})
		}
	}))
	defer ts.Close()
	path, err := daemonDiscoveryPath()
	discoveryPath = path
	if err != nil || writeDaemonDiscovery(path, daemonDiscovery{Endpoint: ts.URL, Token: "secret", PID: 42}) != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if code := daemonStop(); code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if elapsed := time.Since(started); elapsed < 100*time.Millisecond || statusCalls.Load() == 0 {
		t.Fatalf("elapsed=%v status calls=%d", elapsed, statusCalls.Load())
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("discovery still exists: %v", err)
	}
}

func TestWaitForDaemonStopTimesOutWhileDiscoveryRemains(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeAPIError(w, http.StatusServiceUnavailable, &apiError{Code: "stopped", Message: "stopped"})
	}))
	defer ts.Close()
	path := filepath.Join(t.TempDir(), "daemon.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	err := waitForDaemonStop(ctx, daemonClient{endpoint: ts.URL}, path)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error=%v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("discovery disappeared: %v", err)
	}
}

func TestDaemonStopOutlastsBlockedRestoreCleanup(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("LOCALAPPDATA", cache)
	t.Setenv("XDG_CACHE_HOME", cache)
	root := testWorkspaceRoot(t)
	registryFile := filepath.Join(t.TempDir(), "workspaces.json")
	data, err := json.Marshal(workspaceRegistryFile{Roots: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(registryFile, data, 0o600); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	unblock := make(chan struct{})
	registry, err := newWorkspaceRegistry(registryFile, time.Minute, func(string) (*workspaceState, error) {
		close(entered)
		<-unblock
		return nil, errors.New("restore failed")
	})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	service := newCodeService(registry.Resolve, nil, nil)
	stop := make(chan struct{}, 1)
	server := newDaemonHTTPServer(newDaemonServerHandler(service, "secret", registry, func() {
		select {
		case stop <- struct{}{}:
		default:
		}
	}))
	go func() {
		<-stop
		shutdownDaemonServer(server, daemonShutdownTimeout)
	}()
	discoveryPath, err := daemonDiscoveryPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := writeDaemonDiscovery(discoveryPath, daemonDiscovery{
		Endpoint: "http://" + listener.Addr().String(),
		Token:    "secret",
		PID:      42,
	}); err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() {
		err := server.Serve(listener)
		cleanupDaemon(discoveryPath, listener, service, registry)
		served <- err
	}()
	registry.RestoreExplicitAsync()
	<-entered
	code := daemonStop()
	_, discoveryErr := os.Stat(discoveryPath)
	close(unblock)
	registry.restore.Wait()
	select {
	case err := <-served:
		if !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("serve error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("daemon cleanup did not finish after restore unblocked")
	}
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if !errors.Is(discoveryErr, os.ErrNotExist) {
		t.Fatalf("discovery existed when stop returned: %v", discoveryErr)
	}
}

func TestDaemonServerForcesCloseAfterShutdownDeadline(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	server := newDaemonHTTPServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(entered)
		<-release
	}))
	if server.ReadHeaderTimeout <= 0 {
		t.Fatal("ReadHeaderTimeout is disabled")
	}
	go server.Serve(listener)
	requestDone := make(chan struct{})
	go func() {
		_, _ = http.Get("http://" + listener.Addr().String())
		close(requestDone)
	}()
	<-entered
	started := time.Now()
	shutdownDaemonServer(server, 20*time.Millisecond)
	if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
		t.Fatalf("shutdown took %v", elapsed)
	}
	close(release)
	select {
	case <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("forced Close did not release client")
	}
}

func TestCleanupDaemonRemovesDiscoveryAfterResourcesClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	var order []string
	closer := func(name string) closeFunc {
		return func() error {
			if _, err := os.Stat(path); err != nil {
				t.Errorf("discovery missing during %s cleanup: %v", name, err)
			}
			order = append(order, name)
			return nil
		}
	}
	cleanupDaemon(path, closer("listener"), closer("service"), closer("registry"))
	if got := strings.Join(order, ","); got != "listener,service,registry" {
		t.Fatalf("cleanup order=%s", got)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("discovery remains after cleanup: %v", err)
	}
}

func BenchmarkWorkspaceBarrier(b *testing.B) {
	workspace, store := newDaemonBenchmarkWorkspace(b, "func BarrierCandidate() {}\n")
	defer workspace.Close()
	defer store.Close()
	if _, err := workspace.Barrier(context.Background()); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for range b.N {
		if _, err := workspace.Barrier(context.Background()); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkExactSearch(b *testing.B) {
	workspace, store := newDaemonBenchmarkWorkspace(b, "func ExactSearchCandidate() {}\n")
	defer workspace.Close()
	defer store.Close()
	embedder := new(serviceTestEmbedder)
	service := newCodeService(func(string) (*workspaceState, error) { return workspace, nil }, newEmbeddingPool(func() (textEmbedder, error) {
		return embedder, nil
	}), nil)
	defer service.Close()
	if _, err := service.Search(context.Background(), searchRequest{Root: workspace.root, Query: "ExactSearchCandidate", Mode: "auto"}); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for range b.N {
		result, err := service.Search(context.Background(), searchRequest{Root: workspace.root, Query: "ExactSearchCandidate", Mode: "auto"})
		if err != nil || len(result.Hits) == 0 || !result.Fresh {
			b.Fatalf("result=%+v err=%v", result, err)
		}
	}
	b.StopTimer()
	if embedder.calls.Load() != 0 {
		b.Fatalf("exact auto search used vector inference %d times", embedder.calls.Load())
	}
}

func BenchmarkHybridSearch(b *testing.B) {
	workspace, store := newDaemonBenchmarkWorkspace(b, "func ValidateRequest() {}\n")
	defer workspace.Close()
	defer store.Close()
	symbol := serviceSymbol("service.go", "ValidateRequest", "func ValidateRequest() {}")
	if _, err := store.UpsertSymbols(symbol.Path, codeindex.FileHash([]byte(symbol.Body)), []codeindex.Symbol{symbol}, 0, fakeCodeEmbed); err != nil {
		b.Fatal(err)
	}
	service := newCodeService(func(string) (*workspaceState, error) { return workspace, nil }, newEmbeddingPool(func() (textEmbedder, error) {
		return new(serviceTestEmbedder), nil
	}), nil)
	defer service.Close()
	if _, err := service.Search(context.Background(), searchRequest{Root: workspace.root, Query: "request validation", Mode: "hybrid"}); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for range b.N {
		result, err := service.Search(context.Background(), searchRequest{Root: workspace.root, Query: "request validation", Mode: "hybrid"})
		if err != nil || len(result.Hits) == 0 || !result.UsedVector || !result.Fresh {
			b.Fatalf("result=%+v err=%v", result, err)
		}
	}
}

func BenchmarkWorkspaceBarrierTwoWorkspaceIsolation(b *testing.B) {
	first, firstStore := newDaemonBenchmarkWorkspace(b, "func FirstWorkspaceOnly() {}\n")
	second, secondStore := newDaemonBenchmarkWorkspace(b, "func SecondWorkspaceOnly() {}\n")
	defer first.Close()
	defer second.Close()
	defer firstStore.Close()
	defer secondStore.Close()
	service := newCodeService(func(root string) (*workspaceState, error) {
		if root == first.root {
			return first, nil
		}
		return second, nil
	}, newEmbeddingPool(func() (textEmbedder, error) { return new(serviceTestEmbedder), nil }), nil)
	defer service.Close()
	b.ResetTimer()
	for range b.N {
		own, err := service.Search(context.Background(), searchRequest{Root: first.root, Query: "FirstWorkspaceOnly"})
		if err != nil || len(own.Hits) == 0 {
			b.Fatalf("own=%+v err=%v", own, err)
		}
		other, err := service.Search(context.Background(), searchRequest{Root: second.root, Query: "FirstWorkspaceOnly"})
		if err != nil || len(other.Hits) != 0 {
			b.Fatalf("other=%+v err=%v", other, err)
		}
	}
}

func newDaemonBenchmarkWorkspace(b *testing.B, body string) (*workspaceState, *codestore.Store) {
	b.Helper()
	root, err := filepath.Abs(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "service.go"), []byte("package sample\n"+body), 0o644); err != nil {
		b.Fatal(err)
	}
	store, err := codestore.Open(filepath.Join(b.TempDir(), "code.db"), "test-model", codeEmbedDim)
	if err != nil {
		b.Fatal(err)
	}
	workspace, err := newWorkspaceState(root, store, []string{"."}, "go")
	if err != nil {
		store.Close()
		b.Fatal(err)
	}
	return workspace, store
}

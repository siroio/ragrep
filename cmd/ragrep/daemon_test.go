package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

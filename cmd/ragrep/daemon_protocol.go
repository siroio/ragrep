package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/siroio/ragrep/internal/codeindex"
	"github.com/siroio/ragrep/internal/codestore"
	"github.com/siroio/ragrep/internal/lsp"
)

type apiError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

func (e *apiError) Error() string { return e.Message }

type daemonStatus struct {
	Status string `json:"status"`
	PID    int    `json:"pid"`
}

type daemonSearchRequest struct {
	Root  string `json:"root"`
	Query string `json:"query"`
	Mode  string `json:"mode,omitempty"`
	K     int    `json:"k,omitempty"`
}

type daemonSearchResponse struct {
	Hits       any    `json:"hits"`
	Fresh      bool   `json:"fresh"`
	Generation uint64 `json:"generation"`
	Degraded   string `json:"degraded,omitempty"`
	UsedVector bool   `json:"used_vector"`
}

type daemonGetRequest struct {
	Root string `json:"root"`
	Key  string `json:"key"`
	Body bool   `json:"body"`
}

type daemonCodeSearcher interface {
	Search(context.Context, searchRequest) (searchResponse, error)
}

type daemonCodeGetter interface {
	Get(context.Context, getRequest) (codeindex.Symbol, error)
}

type daemonHandler struct {
	service  daemonCodeSearcher
	token    string
	registry *workspaceRegistry
	stop     func()
}

func newDaemonHandler(service daemonCodeSearcher, token string) http.Handler {
	return &daemonHandler{service: service, token: token}
}

func newDaemonServerHandler(service daemonCodeSearcher, token string, registry *workspaceRegistry, stop func()) http.Handler {
	return &daemonHandler{service: service, token: token, registry: registry, stop: stop}
}

func (h *daemonHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+h.token)) != 1 {
		writeAPIError(w, http.StatusUnauthorized, &apiError{Code: "unauthorized", Message: "unauthorized"})
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/status":
		writeJSON(w, http.StatusOK, daemonStatus{Status: "running", PID: os.Getpid()})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/stop":
		if err := decodeJSON(r, &struct{}{}); err != nil {
			writeAPIError(w, http.StatusBadRequest, &apiError{Code: "bad_request", Message: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"stopping": true})
		if h.stop != nil {
			h.stop()
		}
	case r.Method == http.MethodPost && r.URL.Path == "/v1/code/search":
		h.search(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/code/get":
		h.get(w, r)
	case r.URL.Path == "/v1/workspaces":
		h.workspaces(w, r)
	default:
		writeAPIError(w, http.StatusNotFound, &apiError{Code: "not_found", Message: "not found"})
	}
}

func (h *daemonHandler) get(w http.ResponseWriter, r *http.Request) {
	var req daemonGetRequest
	if err := decodeJSON(r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, &apiError{Code: "bad_request", Message: err.Error()})
		return
	}
	release, ok := h.acquireWorkspace(w, req.Root)
	if !ok {
		return
	}
	defer release()
	getter, ok := h.service.(daemonCodeGetter)
	if !ok {
		writeAPIError(w, http.StatusInternalServerError, &apiError{Code: "internal_error", Message: "code service unavailable"})
		return
	}
	symbol, err := getter.Get(r.Context(), getRequest{Root: req.Root, Key: req.Key, Body: req.Body})
	if err != nil {
		status, apiErr := classifyAPIError(err)
		writeAPIError(w, status, apiErr)
		return
	}
	writeJSON(w, http.StatusOK, symbol)
}

func (h *daemonHandler) search(w http.ResponseWriter, r *http.Request) {
	var req daemonSearchRequest
	if err := decodeJSON(r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, &apiError{Code: "bad_request", Message: err.Error()})
		return
	}
	if h.service == nil {
		writeAPIError(w, http.StatusInternalServerError, &apiError{Code: "internal_error", Message: "code service unavailable"})
		return
	}
	release, ok := h.acquireWorkspace(w, req.Root)
	if !ok {
		return
	}
	defer release()
	resp, err := h.service.Search(r.Context(), searchRequest{Root: req.Root, Query: req.Query, Mode: req.Mode, K: req.K})
	if err != nil {
		status, apiErr := classifyAPIError(err)
		writeAPIError(w, status, apiErr)
		return
	}
	writeJSON(w, http.StatusOK, daemonSearchResponse{
		Hits: resp.Hits, Fresh: resp.Fresh, Generation: resp.Generation,
		Degraded: resp.Degraded, UsedVector: resp.UsedVector,
	})
}

func (h *daemonHandler) acquireWorkspace(w http.ResponseWriter, root string) (func(), bool) {
	if h.registry == nil {
		return func() {}, true
	}
	release, err := h.registry.Acquire(root)
	if err != nil {
		status, apiErr := classifyAPIError(err)
		writeAPIError(w, status, apiErr)
		return nil, false
	}
	return release, true
}

func (h *daemonHandler) workspaces(w http.ResponseWriter, r *http.Request) {
	if h.registry == nil {
		writeAPIError(w, http.StatusNotFound, &apiError{Code: "not_found", Message: "not found"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, struct {
			Roots []string `json:"roots"`
		}{Roots: h.registry.List()})
	case http.MethodPost, http.MethodDelete:
		var req struct {
			Path string `json:"path"`
		}
		if err := decodeJSON(r, &req); err != nil {
			writeAPIError(w, http.StatusBadRequest, &apiError{Code: "bad_request", Message: err.Error()})
			return
		}
		if r.Method == http.MethodPost {
			root, err := h.registry.Add(req.Path)
			if err != nil {
				status, apiErr := classifyAPIError(err)
				writeAPIError(w, status, apiErr)
				return
			}
			writeJSON(w, http.StatusOK, struct {
				Root string `json:"root"`
			}{Root: root})
			return
		}
		removed, err := h.registry.Remove(req.Path)
		if err != nil {
			status, apiErr := classifyAPIError(err)
			writeAPIError(w, status, apiErr)
			return
		}
		if !removed {
			writeAPIError(w, http.StatusNotFound, &apiError{Code: "workspace_not_found", Message: "workspace not found"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"removed": true})
	default:
		writeAPIError(w, http.StatusMethodNotAllowed, &apiError{Code: "method_not_allowed", Message: "method not allowed"})
	}
}

func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		if err != nil {
			return err
		}
		return errors.New("multiple JSON values")
	}
	return nil
}

func classifyAPIError(err error) (int, *apiError) {
	switch {
	case errors.Is(err, ErrWorkspaceNotFound):
		return http.StatusNotFound, &apiError{Code: "workspace_not_found", Message: err.Error()}
	case errors.Is(err, ErrWorkspaceSyncing):
		return http.StatusConflict, &apiError{Code: "workspace_syncing", Message: err.Error(), Retryable: true}
	case errors.Is(err, ErrStaleLiveKey):
		return http.StatusConflict, &apiError{Code: "stale_live_key", Message: err.Error()}
	case errors.Is(err, codestore.ErrNotFound):
		return http.StatusNotFound, &apiError{Code: "not_found", Message: err.Error()}
	case errors.Is(err, codestore.ErrReindexRequired):
		return http.StatusConflict, &apiError{Code: "reindex_required", Message: err.Error()}
	}
	var lspErr *lsp.ResponseError
	if errors.As(err, &lspErr) || strings.HasPrefix(err.Error(), "lsp:") {
		return http.StatusBadGateway, &apiError{Code: "lsp_error", Message: err.Error(), Retryable: true}
	}
	return http.StatusInternalServerError, &apiError{Code: "internal_error", Message: err.Error()}
}

func writeAPIError(w http.ResponseWriter, status int, err *apiError) { writeJSON(w, status, err) }

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

type daemonClient struct {
	endpoint string
	token    string
	client   *http.Client
}

func (c daemonClient) Status(ctx context.Context) (daemonStatus, error) {
	var status daemonStatus
	err := c.do(ctx, http.MethodGet, "/v1/status", nil, &status)
	return status, err
}

func (c daemonClient) Search(ctx context.Context, req searchRequest) (searchResponse, error) {
	var wire struct {
		Hits       []codestore.SymbolHit `json:"hits"`
		Fresh      bool                  `json:"fresh"`
		Generation uint64                `json:"generation"`
		Degraded   string                `json:"degraded"`
		UsedVector bool                  `json:"used_vector"`
	}
	err := c.do(ctx, http.MethodPost, "/v1/code/search", daemonSearchRequest{Root: req.Root, Query: req.Query, Mode: req.Mode, K: req.K}, &wire)
	return searchResponse{Hits: wire.Hits, Fresh: wire.Fresh, Generation: wire.Generation, Degraded: wire.Degraded, UsedVector: wire.UsedVector}, err
}

func (c daemonClient) Get(ctx context.Context, req getRequest) (codeindex.Symbol, error) {
	var symbol codeindex.Symbol
	err := c.do(ctx, http.MethodPost, "/v1/code/get", daemonGetRequest{Root: req.Root, Key: req.Key, Body: req.Body}, &symbol)
	return symbol, err
}

func (c daemonClient) Stop(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/v1/stop", struct{}{}, nil)
}

func (c daemonClient) WorkspaceAdd(ctx context.Context, path string) (string, error) {
	var resp struct {
		Root string `json:"root"`
	}
	err := c.do(ctx, http.MethodPost, "/v1/workspaces", map[string]string{"path": path}, &resp)
	return resp.Root, err
}

func (c daemonClient) WorkspaceRemove(ctx context.Context, path string) error {
	return c.do(ctx, http.MethodDelete, "/v1/workspaces", map[string]string{"path": path}, nil)
}

func (c daemonClient) WorkspaceList(ctx context.Context) ([]string, error) {
	var resp struct {
		Roots []string `json:"roots"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/workspaces", nil, &resp)
	return resp.Roots, err
}

func (c daemonClient) do(ctx context.Context, method, path string, body, dst any) error {
	var reader *strings.Reader
	if body == nil {
		reader = strings.NewReader("")
	} else {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = strings.NewReader(string(data))
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.endpoint, "/")+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	client := c.client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var apiErr apiError
		if err := json.NewDecoder(resp.Body).Decode(&apiErr); err != nil {
			return fmt.Errorf("daemon: HTTP %d", resp.StatusCode)
		}
		return &apiErr
	}
	if dst != nil {
		return json.NewDecoder(resp.Body).Decode(dst)
	}
	return nil
}

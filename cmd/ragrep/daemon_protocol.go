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
	"path/filepath"
	"strings"

	"github.com/siroio/ragrep/internal/codeindex"
	"github.com/siroio/ragrep/internal/coderetrieval"
	"github.com/siroio/ragrep/internal/codestore"
	"github.com/siroio/ragrep/internal/lsp"
	"github.com/siroio/ragrep/internal/store"
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
	DB    string `json:"db"`
	Query string `json:"query"`
	Mode  string `json:"mode,omitempty"`
	K     int    `json:"k,omitempty"`
}

type daemonDocumentSearchRequest struct {
	DB    string   `json:"db"`
	Query string   `json:"query"`
	Mode  string   `json:"mode"`
	K     int      `json:"k"`
	Tags  []string `json:"tags,omitempty"`
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
	DB   string `json:"db"`
	Key  string `json:"key"`
	Body bool   `json:"body"`
}

type daemonIndexRequest struct {
	Root     string   `json:"root"`
	DB       string   `json:"db"`
	Language string   `json:"language"`
	Roots    []string `json:"roots"`
}

type daemonExpandRequest struct {
	Root     string `json:"root"`
	DB       string `json:"db"`
	Key      string `json:"key"`
	Relation string `json:"relation"`
}

type daemonPackRequest struct {
	Root         string   `json:"root"`
	DB           string   `json:"db"`
	Query        string   `json:"query"`
	K            int      `json:"k,omitempty"`
	Budget       int      `json:"budget"`
	SelectedKeys []string `json:"selected_keys,omitempty"`
}

type daemonVerifyRequest struct {
	Root     string                 `json:"root"`
	DB       string                 `json:"db"`
	Manifest coderetrieval.Manifest `json:"manifest"`
}

type daemonCodeSearcher interface {
	Search(context.Context, searchRequest) (searchResponse, error)
}

type daemonCodeGetter interface {
	Get(context.Context, getRequest) (codeindex.Symbol, error)
}

type daemonCodeIndexer interface {
	Index(context.Context, indexRequest) (indexResult, error)
}

type daemonCodeExpander interface {
	Expand(context.Context, expandRequest) ([]codeExpandTarget, error)
}

type daemonCodePacker interface {
	Pack(context.Context, packRequest) (codePackOutput, error)
}

type daemonCodeVerifier interface {
	Verify(context.Context, verifyRequest) (codeVerifyOutput, error)
}

type daemonHandler struct {
	service         daemonCodeSearcher
	documentService documentSearcher
	token           string
	registry        *workspaceRegistry
	stop            func()
}

func newDaemonHandler(service daemonCodeSearcher, token string) http.Handler {
	return newDaemonHandlerWithDocuments(service, nil, token, nil, nil)
}

func newDaemonServerHandler(service daemonCodeSearcher, token string, registry *workspaceRegistry, stop func()) http.Handler {
	return newDaemonHandlerWithDocuments(service, nil, token, registry, stop)
}

func newDaemonHandlerWithDocuments(service daemonCodeSearcher, documents documentSearcher, token string, registry *workspaceRegistry, stop func()) http.Handler {
	return &daemonHandler{service: service, documentService: documents, token: token, registry: registry, stop: stop}
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
	case r.Method == http.MethodPost && r.URL.Path == "/v1/search":
		h.searchDocuments(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/code/get":
		h.get(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/code/index":
		h.index(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/code/expand":
		h.expand(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/code/pack":
		h.pack(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/code/verify":
		h.verify(w, r)
	case r.URL.Path == "/v1/workspaces":
		h.workspaces(w, r)
	default:
		writeAPIError(w, http.StatusNotFound, &apiError{Code: "not_found", Message: "not found"})
	}
}

func (h *daemonHandler) searchDocuments(w http.ResponseWriter, r *http.Request) {
	var req daemonDocumentSearchRequest
	if err := decodeJSON(r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, &apiError{Code: "bad_request", Message: err.Error()})
		return
	}
	if req.DB == "" || !filepath.IsAbs(req.DB) {
		writeAPIError(w, http.StatusBadRequest, &apiError{Code: "bad_request", Message: "db must be an absolute path"})
		return
	}
	if strings.TrimSpace(req.Query) == "" {
		writeAPIError(w, http.StatusBadRequest, &apiError{Code: "bad_request", Message: "query must not be empty"})
		return
	}
	if req.Mode != "text" && req.Mode != "vector" && req.Mode != "hybrid" {
		writeAPIError(w, http.StatusBadRequest, &apiError{Code: "bad_request", Message: fmt.Sprintf("unsupported search mode %q", req.Mode)})
		return
	}
	if req.K <= 0 {
		writeAPIError(w, http.StatusBadRequest, &apiError{Code: "bad_request", Message: "k must be positive"})
		return
	}
	if h.documentService == nil {
		writeAPIError(w, http.StatusInternalServerError, &apiError{Code: "internal_error", Message: "document service unavailable"})
		return
	}
	hits, err := h.documentService.SearchDocuments(r.Context(), documentSearchRequest{DB: req.DB, Query: req.Query, Mode: req.Mode, K: req.K, Tags: req.Tags})
	if err != nil {
		status, apiErr := classifyAPIError(err)
		writeAPIError(w, status, apiErr)
		return
	}
	writeJSON(w, http.StatusOK, hits)
}

func (h *daemonHandler) pack(w http.ResponseWriter, r *http.Request) {
	var req daemonPackRequest
	if err := decodeJSON(r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, &apiError{Code: "bad_request", Message: err.Error()})
		return
	}
	if len(req.SelectedKeys) > 3 {
		writeAPIError(w, http.StatusBadRequest, &apiError{Code: "bad_request", Message: fmt.Sprintf("selected keys accepts at most 3 keys, got %d", len(req.SelectedKeys))})
		return
	}
	release, ok := h.acquireWorkspace(w, req.Root, req.DB)
	if !ok {
		return
	}
	defer release()
	packer, ok := h.service.(daemonCodePacker)
	if !ok {
		writeAPIError(w, http.StatusInternalServerError, &apiError{Code: "internal_error", Message: "code service unavailable"})
		return
	}
	out, err := packer.Pack(r.Context(), packRequest{
		Root: req.Root, DB: req.DB, Query: req.Query, K: req.K, Budget: req.Budget, SelectedKeys: req.SelectedKeys,
	})
	if err != nil {
		status, apiErr := classifyAPIError(err)
		writeAPIError(w, status, apiErr)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *daemonHandler) verify(w http.ResponseWriter, r *http.Request) {
	var req daemonVerifyRequest
	if err := decodeJSON(r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, &apiError{Code: "bad_request", Message: err.Error()})
		return
	}
	release, ok := h.acquireWorkspace(w, req.Root, req.DB)
	if !ok {
		return
	}
	defer release()
	verifier, ok := h.service.(daemonCodeVerifier)
	if !ok {
		writeAPIError(w, http.StatusInternalServerError, &apiError{Code: "internal_error", Message: "code service unavailable"})
		return
	}
	out, err := verifier.Verify(r.Context(), verifyRequest{Root: req.Root, DB: req.DB, Manifest: req.Manifest})
	if err != nil {
		status, apiErr := classifyAPIError(err)
		writeAPIError(w, status, apiErr)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *daemonHandler) index(w http.ResponseWriter, r *http.Request) {
	var req daemonIndexRequest
	if err := decodeJSON(r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, &apiError{Code: "bad_request", Message: err.Error()})
		return
	}
	release, ok := h.acquireWorkspace(w, req.Root, req.DB)
	if !ok {
		return
	}
	defer release()
	indexer, ok := h.service.(daemonCodeIndexer)
	if !ok {
		writeAPIError(w, http.StatusInternalServerError, &apiError{Code: "internal_error", Message: "code service unavailable"})
		return
	}
	result, err := indexer.Index(r.Context(), indexRequest{Root: req.Root, DB: req.DB, Language: req.Language, Roots: req.Roots})
	if err != nil {
		status, apiErr := classifyAPIError(err)
		writeAPIError(w, status, apiErr)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *daemonHandler) expand(w http.ResponseWriter, r *http.Request) {
	var req daemonExpandRequest
	if err := decodeJSON(r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, &apiError{Code: "bad_request", Message: err.Error()})
		return
	}
	release, ok := h.acquireWorkspace(w, req.Root, req.DB)
	if !ok {
		return
	}
	defer release()
	expander, ok := h.service.(daemonCodeExpander)
	if !ok {
		writeAPIError(w, http.StatusInternalServerError, &apiError{Code: "internal_error", Message: "code service unavailable"})
		return
	}
	targets, err := expander.Expand(r.Context(), expandRequest{Root: req.Root, DB: req.DB, Key: req.Key, Relation: req.Relation})
	if err != nil {
		status, apiErr := classifyAPIError(err)
		writeAPIError(w, status, apiErr)
		return
	}
	writeJSON(w, http.StatusOK, targets)
}

func (h *daemonHandler) get(w http.ResponseWriter, r *http.Request) {
	var req daemonGetRequest
	if err := decodeJSON(r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, &apiError{Code: "bad_request", Message: err.Error()})
		return
	}
	release, ok := h.acquireWorkspace(w, req.Root, req.DB)
	if !ok {
		return
	}
	defer release()
	getter, ok := h.service.(daemonCodeGetter)
	if !ok {
		writeAPIError(w, http.StatusInternalServerError, &apiError{Code: "internal_error", Message: "code service unavailable"})
		return
	}
	symbol, err := getter.Get(r.Context(), getRequest{Root: req.Root, DB: req.DB, Key: req.Key, Body: req.Body})
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
	release, ok := h.acquireWorkspace(w, req.Root, req.DB)
	if !ok {
		return
	}
	defer release()
	resp, err := h.service.Search(r.Context(), searchRequest{Root: req.Root, DB: req.DB, Query: req.Query, Mode: req.Mode, K: req.K})
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

func (h *daemonHandler) acquireWorkspace(w http.ResponseWriter, root, db string) (func(), bool) {
	if h.registry == nil {
		return func() {}, true
	}
	release, err := h.registry.AcquireCode(root, db)
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
	var unsupported *codeExpandUnsupportedError
	if errors.As(err, &unsupported) {
		return http.StatusNotImplemented, &apiError{Code: "not_supported", Message: err.Error()}
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

type codeDaemonClient interface {
	Search(context.Context, searchRequest) (searchResponse, error)
	Get(context.Context, getRequest) (codeindex.Symbol, error)
	Index(context.Context, indexRequest) (indexResult, error)
	Expand(context.Context, expandRequest) ([]codeExpandTarget, error)
	Pack(context.Context, packRequest) (codePackOutput, error)
	Verify(context.Context, verifyRequest) (codeVerifyOutput, error)
}

type documentDaemonClient interface {
	SearchDocuments(context.Context, documentSearchRequest) ([]store.Hit, error)
}

var codeDaemonClientFactory = loadCodeDaemonClient
var documentDaemonClientFactory = loadDocumentDaemonClient

func loadDocumentDaemonClient() (documentDaemonClient, error) {
	client, _, err := loadDaemonClient()
	return client, err
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
	err := c.do(ctx, http.MethodPost, "/v1/code/search", daemonSearchRequest{Root: req.Root, DB: req.DB, Query: req.Query, Mode: req.Mode, K: req.K}, &wire)
	return searchResponse{Hits: wire.Hits, Fresh: wire.Fresh, Generation: wire.Generation, Degraded: wire.Degraded, UsedVector: wire.UsedVector}, err
}

func (c daemonClient) SearchDocuments(ctx context.Context, req documentSearchRequest) ([]store.Hit, error) {
	var hits []store.Hit
	err := c.do(ctx, http.MethodPost, "/v1/search", daemonDocumentSearchRequest{DB: req.DB, Query: req.Query, Mode: req.Mode, K: req.K, Tags: req.Tags}, &hits)
	return hits, err
}

func (c daemonClient) Get(ctx context.Context, req getRequest) (codeindex.Symbol, error) {
	var symbol codeindex.Symbol
	err := c.do(ctx, http.MethodPost, "/v1/code/get", daemonGetRequest{Root: req.Root, DB: req.DB, Key: req.Key, Body: req.Body}, &symbol)
	return symbol, err
}

func (c daemonClient) Index(ctx context.Context, req indexRequest) (indexResult, error) {
	var result indexResult
	err := c.do(ctx, http.MethodPost, "/v1/code/index", daemonIndexRequest{Root: req.Root, DB: req.DB, Language: req.Language, Roots: req.Roots}, &result)
	return result, err
}

func (c daemonClient) Expand(ctx context.Context, req expandRequest) ([]codeExpandTarget, error) {
	var targets []codeExpandTarget
	err := c.do(ctx, http.MethodPost, "/v1/code/expand", daemonExpandRequest{Root: req.Root, DB: req.DB, Key: req.Key, Relation: req.Relation}, &targets)
	return targets, err
}

func (c daemonClient) Pack(ctx context.Context, req packRequest) (codePackOutput, error) {
	var out codePackOutput
	err := c.do(ctx, http.MethodPost, "/v1/code/pack", daemonPackRequest{
		Root: req.Root, DB: req.DB, Query: req.Query, K: req.K, Budget: req.Budget, SelectedKeys: req.SelectedKeys,
	}, &out)
	return out, err
}

func (c daemonClient) Verify(ctx context.Context, req verifyRequest) (codeVerifyOutput, error) {
	var out codeVerifyOutput
	err := c.do(ctx, http.MethodPost, "/v1/code/verify", daemonVerifyRequest{
		Root: req.Root, DB: req.DB, Manifest: req.Manifest,
	}, &out)
	return out, err
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

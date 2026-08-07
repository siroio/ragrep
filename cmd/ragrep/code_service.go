package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/siroio/ragrep/internal/codeindex"
	"github.com/siroio/ragrep/internal/coderetrieval"
	"github.com/siroio/ragrep/internal/codestore"
	"github.com/siroio/ragrep/internal/config"
	"github.com/siroio/ragrep/internal/lsp"
)

var ErrStaleLiveKey = errors.New("stale_live_key")

type searchRequest struct {
	Root, DB, Query, Mode string
	K                     int
}

type searchResponse struct {
	Hits       []codestore.SymbolHit
	Fresh      bool
	Generation uint64
	Degraded   string
	UsedVector bool
}

type getRequest struct {
	Root, DB, Key string
	Body          bool
}

type packRequest struct {
	Root, DB, Query string
	K, Budget       int
	SelectedKeys    []string
}

type verifyRequest struct {
	Root, DB string
	Manifest coderetrieval.Manifest
}

type indexRequest struct {
	Root, DB, Language string
	Roots              []string
}

type indexResult struct {
	Indexed []string `json:"indexed"`
	Scanned int      `json:"scanned"`
	Pruned  []string `json:"pruned"`
}

type expandRequest struct {
	Root, DB, Key, Relation string
}

type codeExpandUnsupportedError struct{ Server string }

func (e *codeExpandUnsupportedError) Error() string {
	return "not supported by server " + e.Server
}

type workspaceResolver func(root string) (*workspaceState, error)
type workspaceDBResolver func(root, db string) (*workspaceState, error)

type codeService struct {
	resolve    workspaceDBResolver
	embeddings *embeddingPool
	lsps       *lspPool
	closeOnce  sync.Once
	closeErr   error
}

func newCodeService(resolve workspaceResolver, embeddings *embeddingPool, lsps *lspPool) *codeService {
	var resolveDB workspaceDBResolver
	if resolve != nil {
		resolveDB = func(root, _ string) (*workspaceState, error) { return resolve(root) }
	}
	return newCodeServiceForDB(resolveDB, embeddings, lsps)
}

func newCodeServiceForDB(resolve workspaceDBResolver, embeddings *embeddingPool, lsps *lspPool) *codeService {
	if embeddings == nil {
		embeddings = newEmbeddingPool(nil)
	}
	if lsps == nil {
		lsps = newLSPPool(0, nil)
	}
	return &codeService{resolve: resolve, embeddings: embeddings, lsps: lsps}
}

func (s *codeService) Search(ctx context.Context, req searchRequest) (searchResponse, error) {
	ws, err := s.workspace(req.Root, req.DB)
	if err != nil {
		return searchResponse{}, err
	}
	ws.setConfirmation(func(path, hash string) {
		_ = s.confirmPathAtDB(context.Background(), ws.root, req.DB, path, hash)
	})
	generation, err := codeServiceBarrier(ctx, ws)
	if err != nil {
		return searchResponse{}, err
	}

	k := req.K
	if k <= 0 || k > 5 {
		k = 5
	}
	live, err := ws.store.SearchLiveText(req.Query, 50)
	if err != nil {
		return searchResponse{}, err
	}

	var durable []codestore.SymbolHit
	usedVector := false
	var vectorErr error
	vector := func() ([]float32, error) {
		usedVector = true
		var v []float32
		v, vectorErr = s.embeddings.Embed(ctx, req.Query)
		return v, vectorErr
	}
	switch req.Mode {
	case "", "auto":
		textOnly := hasExactHit(live)
		if !textOnly {
			textOnly, err = ws.store.HasSymbolsForPath(req.Query)
			if err != nil {
				return searchResponse{}, err
			}
		}
		if textOnly {
			durable, err = ws.store.SearchSymbolsTextExact(req.Query, 50)
		} else {
			durable, usedVector, err = ws.store.SearchSymbolsAuto(req.Query, 50, vector)
		}
	case "text":
		durable, err = ws.store.SearchSymbolsTextExact(req.Query, 50)
	case "vector":
		var v []float32
		v, err = vector()
		if err == nil {
			durable, err = ws.store.SearchSymbolsVector(v, 50)
		}
	case "hybrid":
		var v []float32
		v, err = vector()
		if err == nil {
			durable, err = ws.store.SearchSymbolsHybrid(req.Query, v, 50)
		}
	default:
		return searchResponse{}, fmt.Errorf("unsupported search mode %q", req.Mode)
	}
	degraded := ""
	if err != nil && vectorErr != nil {
		durable, err = ws.store.SearchSymbolsTextExact(req.Query, 50)
		degraded = "vector_unavailable"
		usedVector = false
	}
	if err != nil {
		return searchResponse{}, err
	}
	return searchResponse{
		Hits:       mergeServiceHits(durable, live, k),
		Fresh:      true,
		Generation: generation,
		Degraded:   degraded,
		UsedVector: usedVector,
	}, nil
}

func (s *codeService) Pack(ctx context.Context, req packRequest) (codePackOutput, error) {
	if len(req.SelectedKeys) > 3 {
		return codePackOutput{}, fmt.Errorf("selected keys accepts at most 3 keys, got %d", len(req.SelectedKeys))
	}
	search, err := s.Search(ctx, searchRequest{Root: req.Root, DB: req.DB, Query: req.Query, Mode: "hybrid", K: req.K})
	if err != nil {
		return codePackOutput{}, err
	}
	ws, err := s.workspace(req.Root, req.DB)
	if err != nil {
		return codePackOutput{}, err
	}
	out, err := runCodePack(ws.store, search.Hits, req.Budget, req.SelectedKeys, func(key string) (codeindex.Symbol, error) {
		return s.Get(ctx, getRequest{Root: req.Root, DB: req.DB, Key: key, Body: true})
	})
	if err != nil {
		return codePackOutput{}, err
	}
	out.Fresh = search.Fresh
	out.Generation = search.Generation
	return out, nil
}

func (s *codeService) Verify(ctx context.Context, req verifyRequest) (codeVerifyOutput, error) {
	ws, err := s.workspace(req.Root, req.DB)
	if err != nil {
		return codeVerifyOutput{}, err
	}
	if _, err := codeServiceBarrier(ctx, ws); err != nil {
		return codeVerifyOutput{}, err
	}
	return runCodeVerify(ws.store, req.Manifest, ws.root)
}

func codeServiceBarrier(ctx context.Context, ws *workspaceState) (uint64, error) {
	barrierCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	return ws.Barrier(barrierCtx)
}

func (s *codeService) Get(_ context.Context, req getRequest) (codeindex.Symbol, error) {
	ws, err := s.workspace(req.Root, req.DB)
	if err != nil {
		return codeindex.Symbol{}, err
	}
	var sym codeindex.Symbol
	if strings.HasPrefix(req.Key, "live:") {
		sym, err = ws.store.GetLiveFile(req.Key)
		if errors.Is(err, codestore.ErrStaleLiveKey) {
			return codeindex.Symbol{}, ErrStaleLiveKey
		}
	} else {
		sym, err = ws.store.GetSymbol(req.Key)
	}
	if err != nil {
		return codeindex.Symbol{}, err
	}
	if !req.Body {
		sym.Body = ""
	}
	return sym, nil
}

func (s *codeService) Index(ctx context.Context, req indexRequest) (indexResult, error) {
	if err := ctx.Err(); err != nil {
		return indexResult{}, err
	}
	ws, err := s.workspace(req.Root, req.DB)
	if err != nil {
		return indexResult{}, err
	}
	ext, ok := codeLangExt[req.Language]
	if !ok {
		return indexResult{}, fmt.Errorf("unsupported language %q", req.Language)
	}
	cfg, err := config.Load(ws.root)
	if err != nil {
		return indexResult{}, err
	}
	serverCmd, err := cfg.ServerCommand(req.Language)
	if err != nil {
		return indexResult{}, err
	}

	var files []string
	for _, root := range req.Roots {
		found, err := discoverCodeFilesContext(ctx, filepath.Join(ws.root, filepath.FromSlash(root)), ext)
		if err != nil {
			return indexResult{}, err
		}
		files = append(files, found...)
	}
	result := indexResult{Scanned: len(files)}
	if len(files) == 0 {
		if err := ctx.Err(); err != nil {
			return indexResult{}, err
		}
		result.Pruned, err = pruneCodeSymbolPathsContext(ctx, ws.store, req.Roots, nil)
		return result, err
	}

	client, serverName, serverVersion, release, err := s.lsps.AcquireWithMetadata(ctx, ws.root, req.Language)
	if err != nil {
		return indexResult{}, err
	}
	defer release()
	if !client.Supports(lsp.FeatureDocumentSymbol) {
		return indexResult{}, fmt.Errorf("language server %q does not support textDocument/documentSymbol (required relation: document symbols for indexing)", serverCmd)
	}
	if err := ctx.Err(); err != nil {
		return indexResult{}, err
	}
	runID, err := ws.store.RecordIndexRun("index:"+strings.Join(req.Roots, ","), gitRevision(ws.root), req.Language, serverName, serverVersion, codeModelID, time.Now())
	if err != nil {
		return indexResult{}, err
	}

	seen := make(map[string]bool, len(files))
	for _, path := range files {
		rel, err := normPath(path, ws.root)
		if err != nil {
			return indexResult{}, err
		}
		seen[rel] = true
		content, err := os.ReadFile(path)
		if err != nil {
			return indexResult{}, err
		}
		docSyms, err := client.DocumentSymbol(ctx, lsp.DocumentSymbolParams{
			TextDocument: lsp.TextDocumentIdentifier{URI: fileURI(path)},
		})
		if err != nil {
			return indexResult{}, fmt.Errorf("%s: %w", rel, err)
		}
		symbols, err := codeindex.Extract(req.Language, rel, content, docSyms)
		if err != nil {
			return indexResult{}, err
		}
		if err := ctx.Err(); err != nil {
			return indexResult{}, err
		}
		changed, err := ws.store.UpsertSymbols(rel, codeindex.FileHash(content), symbols, runID, func(text string) ([]float32, error) {
			return s.embeddings.Embed(ctx, text)
		})
		if err != nil {
			return indexResult{}, fmt.Errorf("%s: %w", rel, err)
		}
		if changed {
			result.Indexed = append(result.Indexed, rel)
		}
	}
	if err := ctx.Err(); err != nil {
		return indexResult{}, err
	}
	result.Pruned, err = pruneCodeSymbolPathsContext(ctx, ws.store, req.Roots, seen)
	return result, err
}

func (s *codeService) Expand(ctx context.Context, req expandRequest) ([]codeExpandTarget, error) {
	feature, ok := codeExpandFeature[req.Relation]
	if !ok {
		return nil, fmt.Errorf("unsupported relation %q", req.Relation)
	}
	ws, err := s.workspace(req.Root, req.DB)
	if err != nil {
		return nil, err
	}
	sym, err := ws.store.GetSymbol(req.Key)
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(ws.root)
	if err != nil {
		return nil, err
	}
	serverCmd, err := cfg.ServerCommand(sym.Language)
	if err != nil {
		return nil, err
	}
	client, serverName, serverVersion, release, err := s.lsps.AcquireWithMetadata(ctx, ws.root, sym.Language)
	if err != nil {
		return nil, err
	}
	defer release()
	if !client.Supports(feature) {
		return nil, &codeExpandUnsupportedError{Server: serverCmd}
	}

	resolve, resolveErr := resolverFor(ws.store)
	absPath := filepath.Join(ws.root, filepath.FromSlash(sym.Path))
	content, _ := os.ReadFile(absPath)
	pos := lsp.TextDocumentPositionParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: fileURI(absPath)},
		Position:     declarationPosition(content, sym),
	}

	var relations []codeindex.Relation
	switch req.Relation {
	case "definition":
		locs, err := client.Definition(ctx, lsp.DefinitionParams(pos))
		if err != nil {
			return nil, err
		}
		relations = codeindex.DefinitionRelations(sym.Key, locsFromLSP(ws.root, locs), serverName, resolve)
	case "references", "tests":
		locs, err := client.References(ctx, lsp.ReferenceParams{
			TextDocumentPositionParams: pos,
			Context:                    lsp.ReferenceContext{IncludeDeclaration: false},
		})
		if err != nil {
			return nil, err
		}
		relations = codeindex.ReferenceRelations(sym.Key, locsFromLSP(ws.root, locs), serverName, resolve)
	case "callers", "callees":
		items, err := client.PrepareCallHierarchy(ctx, lsp.CallHierarchyPrepareParams(pos))
		if err != nil {
			return nil, err
		}
		if len(items) != 0 {
			item := items[0]
			if req.Relation == "callers" {
				calls, err := client.IncomingCalls(ctx, lsp.CallHierarchyIncomingCallsParams{Item: item})
				if err != nil {
					return nil, err
				}
				froms := make([]lsp.CallHierarchyItem, len(calls))
				for i, call := range calls {
					froms[i] = call.From
				}
				relations = codeindex.CallerRelations(sym.Key, locsFromCallHierarchyItems(ws.root, froms), serverName, resolve)
			} else {
				calls, err := client.OutgoingCalls(ctx, lsp.CallHierarchyOutgoingCallsParams{Item: item})
				if err != nil {
					return nil, err
				}
				tos := make([]lsp.CallHierarchyItem, len(calls))
				for i, call := range calls {
					tos[i] = call.To
				}
				relations = codeindex.CalleeRelations(sym.Key, locsFromCallHierarchyItems(ws.root, tos), serverName, resolve)
			}
		}
	}
	if *resolveErr != nil {
		return nil, *resolveErr
	}

	runID, err := ws.store.RecordIndexRun(fmt.Sprintf("expand:%s:%s", req.Relation, sym.Key), gitRevision(ws.root), sym.Language, serverName, serverVersion, codeModelID, time.Now())
	if err != nil {
		return nil, err
	}
	if err := ws.store.ReplaceRelations(runID, sym.Key, codeExpandReplaceGroup[req.Relation], codeindex.DedupResolvedRelations(relations)); err != nil {
		return nil, err
	}
	filtered := relations[:0:0]
	for _, relation := range relations {
		if relation.Kind == req.Relation {
			filtered = append(filtered, relation)
		}
	}
	return expandTargets(ws.store, filtered)
}

func (s *codeService) ConfirmPath(ctx context.Context, root, path, expectedHash string) error {
	return s.confirmPathAtDB(ctx, root, "", path, expectedHash)
}

func (s *codeService) confirmPathAtDB(ctx context.Context, root, db, path, expectedHash string) error {
	ws, err := s.workspace(root, db)
	if err != nil {
		return err
	}
	unlock := ws.lockConfirmation(path)
	defer unlock()
	file, err := ws.store.GetLiveFileByPath(path, expectedHash)
	if errors.Is(err, codestore.ErrStaleLiveKey) {
		return nil
	}
	if err != nil {
		return err
	}
	if file.Deleted {
		_, err = ws.store.DeleteSymbolsForPathIfLiveHash(file.Path, expectedHash)
		return err
	}

	absPath := filepath.Join(ws.root, filepath.FromSlash(file.Path))
	rel, err := normPath(absPath, ws.root)
	if err != nil {
		return err
	}
	client, release, err := s.lsps.Acquire(ctx, ws.root, ws.language)
	if err != nil {
		return err
	}
	defer release()
	documentSymbols, err := client.DocumentSymbol(ctx, lsp.DocumentSymbolParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: fileURI(absPath)},
	})
	if err != nil {
		return err
	}
	symbols, err := codeindex.Extract(ws.language, rel, []byte(file.Body), documentSymbols)
	if err != nil {
		return err
	}
	if _, err := ws.store.UpsertSymbols(rel, file.Hash, symbols, 0, func(text string) ([]float32, error) {
		return s.embeddings.Embed(ctx, text)
	}); err != nil {
		return err
	}
	_, err = ws.store.RemoveLiveFileIfHash(rel, expectedHash)
	return err
}

func (s *codeService) workspace(root, db string) (*workspaceState, error) {
	if s.resolve == nil {
		return nil, errors.New("workspace resolver is nil")
	}
	return s.resolve(root, db)
}

func (s *codeService) Close() error {
	s.closeOnce.Do(func() {
		s.closeErr = errors.Join(s.lsps.Close(), s.embeddings.Close())
	})
	return s.closeErr
}

func hasExactHit(hits []codestore.SymbolHit) bool {
	for _, hit := range hits {
		if hit.ExactMatch {
			return true
		}
	}
	return false
}

type serviceRankedHit struct {
	hit   codestore.SymbolHit
	score float64
}

func mergeServiceHits(durable, live []codestore.SymbolHit, k int) []codestore.SymbolHit {
	byKey := make(map[string]*serviceRankedHit, len(durable)+len(live))
	for _, list := range [][]codestore.SymbolHit{durable, live} {
		for rank, hit := range list {
			candidate := byKey[hit.Key]
			if candidate == nil {
				candidate = &serviceRankedHit{hit: hit}
				byKey[hit.Key] = candidate
			}
			candidate.score += 1 / float64(61+rank)
		}
	}
	ranked := make([]serviceRankedHit, 0, len(byKey))
	for _, hit := range byKey {
		ranked = append(ranked, *hit)
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].hit.ExactMatch != ranked[j].hit.ExactMatch {
			return ranked[i].hit.ExactMatch
		}
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return ranked[i].hit.Key < ranked[j].hit.Key
	})

	hits := make([]codestore.SymbolHit, 0, min(k, len(ranked)))
	for _, candidate := range ranked {
		if overlapsServiceHit(candidate.hit, hits) {
			continue
		}
		candidate.hit.Score = candidate.score
		hits = append(hits, candidate.hit)
		if len(hits) == k {
			break
		}
	}
	return hits
}

func overlapsServiceHit(candidate codestore.SymbolHit, hits []codestore.SymbolHit) bool {
	for _, hit := range hits {
		if hit.Path == candidate.Path && hit.StartLine < candidate.EndLine && candidate.StartLine < hit.EndLine {
			return true
		}
	}
	return false
}

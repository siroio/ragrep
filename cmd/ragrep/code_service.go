package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/siroio/ragrep/internal/codeindex"
	"github.com/siroio/ragrep/internal/codestore"
	"github.com/siroio/ragrep/internal/lsp"
)

var ErrStaleLiveKey = errors.New("stale_live_key")

type searchRequest struct {
	Root, Query, Mode string
	K                 int
}

type searchResponse struct {
	Hits       []codestore.SymbolHit
	Fresh      bool
	Generation uint64
	Degraded   string
	UsedVector bool
}

type getRequest struct {
	Root, Key string
	Body      bool
}

type workspaceResolver func(root string) (*workspaceState, error)

type codeService struct {
	resolve    workspaceResolver
	embeddings *embeddingPool
	lsps       *lspPool
	closeOnce  sync.Once
	closeErr   error
}

func newCodeService(resolve workspaceResolver, embeddings *embeddingPool, lsps *lspPool) *codeService {
	if embeddings == nil {
		embeddings = newEmbeddingPool(nil)
	}
	if lsps == nil {
		lsps = newLSPPool(0, nil)
	}
	return &codeService{resolve: resolve, embeddings: embeddings, lsps: lsps}
}

func (s *codeService) Search(ctx context.Context, req searchRequest) (searchResponse, error) {
	ws, err := s.workspace(req.Root)
	if err != nil {
		return searchResponse{}, err
	}
	ws.setConfirmation(func(path, hash string) {
		_ = s.ConfirmPath(context.Background(), ws.root, path, hash)
	})
	barrierCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	generation, err := ws.Barrier(barrierCtx)
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

func (s *codeService) Get(_ context.Context, req getRequest) (codeindex.Symbol, error) {
	ws, err := s.workspace(req.Root)
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

func (s *codeService) ConfirmPath(ctx context.Context, root, path, expectedHash string) error {
	ws, err := s.workspace(root)
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

func (s *codeService) workspace(root string) (*workspaceState, error) {
	if s.resolve == nil {
		return nil, errors.New("workspace resolver is nil")
	}
	return s.resolve(root)
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

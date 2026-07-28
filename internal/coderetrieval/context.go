// Package coderetrieval assembles retrieval-budgeted "context packs" for an
// LLM caller from already-indexed code (see internal/codestore,
// internal/codeindex): ranked candidate metadata, a handful of full symbol
// bodies, and their 1-hop relations, packed in priority order until a
// character budget runs out. It also defines a stale-detectable manifest
// that records which symbols a plan referenced -- by stable key, not by
// copying their bodies -- so a caller can later tell whether the underlying
// files have moved on before trusting the plan.
//
// This package has no database dependency of its own: callers that need
// live data pass small getter functions (SymbolGetter, RelationGetter,
// SymbolFinder) -- (*codestore.Store).GetSymbol satisfies SymbolGetter
// directly. Tests substitute map-backed stubs.
package coderetrieval

import (
	"encoding/json"
	"fmt"

	"github.com/siroio/ragrep/internal/codeindex"
	"github.com/siroio/ragrep/internal/codestore"
)

// SymbolGetter fetches one symbol's full record (including Body) by its
// stable key. (*codestore.Store).GetSymbol has this exact signature.
type SymbolGetter func(key string) (codeindex.Symbol, error)

// RelationGetter fetches the relations originating from key (one hop, no
// further traversal -- matches how internal/codeindex's relation builders
// already work).
type RelationGetter func(key string) ([]codeindex.Relation, error)

// AssembleOptions configures BuildContextPack.
type AssembleOptions struct {
	// Budget is the max total JSON-encoded size, in bytes, of everything
	// added to the pack (candidates + symbol bodies + relations combined).
	Budget int

	// SelectedKeys names the symbols (typically 1-3) whose full bodies
	// should be staged into the pack, in priority order. Only symbols that
	// actually fit are included; their 1-hop relations are staged next, in
	// the same order.
	SelectedKeys []string

	// GetSymbol and GetRelations are unused (nil) when SelectedKeys is
	// empty.
	GetSymbol    SymbolGetter
	GetRelations RelationGetter
}

// ContextPack is the JSON-serializable result of BuildContextPack: what was
// actually included, and, if the budget ran out, what had to be left
// behind. Truncation is always reported explicitly -- Skipped is never
// silently dropped.
type ContextPack struct {
	Candidates []codestore.SymbolHit `json:"candidates"`
	Symbols    []codeindex.Symbol    `json:"symbols,omitempty"`
	Relations  []codeindex.Relation  `json:"relations,omitempty"`

	Budget    int      `json:"budget"`
	UsedChars int      `json:"used_chars"`
	Truncated bool     `json:"truncated"`
	Skipped   []string `json:"skipped,omitempty"`
}

// BuildContextPack packs, in strict priority order, (1) every deduplicated
// candidate's metadata, (2) each selected symbol's full body, then (3) each
// included symbol's 1-hop relations -- adding an item only if it still fits
// under opts.Budget. An item that doesn't fit is recorded in Skipped and
// Truncated is set true; packing continues through the remaining items (a
// later, smaller item may still fit), so Skipped can be a strict subset of
// candidates while later stages still partially succeed.
//
// An item's cost is the length of its own JSON encoding -- the same
// encoding the caller will eventually serialize the whole pack with, so
// UsedChars tracks what the pack will actually cost on the wire, not an
// approximation of it.
//
// GetSymbol/GetRelations errors (a real lookup failure, not "didn't fit")
// abort immediately and are returned wrapped.
func BuildContextPack(candidates []codestore.SymbolHit, opts AssembleOptions) (ContextPack, error) {
	pack := ContextPack{Budget: opts.Budget}

	for _, c := range DedupCandidates(candidates) {
		if !pack.tryAdd(c, c.Key) {
			continue
		}
		pack.Candidates = append(pack.Candidates, c)
	}

	for _, key := range opts.SelectedKeys {
		sym, err := opts.GetSymbol(key)
		if err != nil {
			return ContextPack{}, fmt.Errorf("coderetrieval: fetch symbol %q: %w", key, err)
		}
		if !pack.tryAdd(sym, key) {
			continue
		}
		pack.Symbols = append(pack.Symbols, sym)
	}

	for _, sym := range pack.Symbols {
		rels, err := opts.GetRelations(sym.Key)
		if err != nil {
			return ContextPack{}, fmt.Errorf("coderetrieval: fetch relations for %q: %w", sym.Key, err)
		}
		for _, r := range rels {
			id := r.FromKey + "->" + r.ToKey + ":" + r.Kind
			if !pack.tryAdd(r, id) {
				continue
			}
			pack.Relations = append(pack.Relations, r)
		}
	}

	return pack, nil
}

// tryAdd charges item's JSON-encoded size against the remaining budget. On
// success it updates UsedChars and returns true. On failure (budget
// exhausted, or the item can't be marshaled -- which would be a bug in the
// caller's data, not a budget problem, so it's also treated as a skip
// rather than propagated) it records id in Skipped, sets Truncated, and
// returns false.
func (p *ContextPack) tryAdd(item any, id string) bool {
	b, err := json.Marshal(item)
	cost := len(b)
	if err != nil || p.UsedChars+cost > p.Budget {
		p.Truncated = true
		p.Skipped = append(p.Skipped, id)
		return false
	}
	p.UsedChars += cost
	return true
}

// DedupCandidates merges hits that share overlapping line ranges within the
// same file into a single entry: the first-seen hit (callers pass hits in
// relevance order, so this keeps the highest-ranked one) survives as the
// representative, its StartLine/EndLine widened to the union of every range
// it absorbed. This collapses the case where two ranked hits are actually
// the same declaration reached via different match paths (FTS vs. vector,
// or an overlapping nested symbol), so the metadata stage doesn't spend
// budget twice describing one location.
//
// ponytail: O(n*m) scan (m = merged groups so far); fine at typical top-k
// sizes, switch to a per-file sort+sweep if k grows into the thousands.
func DedupCandidates(hits []codestore.SymbolHit) []codestore.SymbolHit {
	out := make([]codestore.SymbolHit, 0, len(hits))
	for _, h := range hits {
		merged := false
		for i := range out {
			if out[i].Path == h.Path && out[i].StartLine <= h.EndLine && h.StartLine <= out[i].EndLine {
				if h.StartLine < out[i].StartLine {
					out[i].StartLine = h.StartLine
				}
				if h.EndLine > out[i].EndLine {
					out[i].EndLine = h.EndLine
				}
				merged = true
				break
			}
		}
		if !merged {
			out = append(out, h)
		}
	}
	return out
}

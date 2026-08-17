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

	fetched := make([]codeindex.Symbol, 0, len(opts.SelectedKeys))
	for _, key := range opts.SelectedKeys {
		sym, err := opts.GetSymbol(key)
		if err != nil {
			return ContextPack{}, fmt.Errorf("coderetrieval: fetch symbol %q: %w", key, err)
		}
		fetched = append(fetched, sym)
	}
	for _, sym := range dedupSymbols(fetched) {
		if !pack.tryAdd(sym, sym.Key) {
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
func DedupCandidates(hits []codestore.SymbolHit) []codestore.SymbolHit {
	return mergeOverlapping(hits,
		func(h codestore.SymbolHit) (path string, start, end int) { return h.Path, h.StartLine, h.EndLine },
		func(dst *codestore.SymbolHit, start, end int) { dst.StartLine, dst.EndLine = start, end })
}

// dedupSymbols applies the same overlapping-range merge as DedupCandidates
// to selected symbol bodies (BuildContextPack's stage 2): two SelectedKeys
// whose declarations overlap or nest in the same file (e.g. a type and one
// of its own methods, or two ranges a caller picked that turned out to
// intersect) collapse into one packed body instead of paying the budget
// twice for overlapping source. As with DedupCandidates, the first-seen
// symbol's Body is what survives; its Range widens to the union.
func dedupSymbols(symbols []codeindex.Symbol) []codeindex.Symbol {
	return mergeOverlapping(symbols,
		func(s codeindex.Symbol) (path string, start, end int) { return s.Path, s.Range.Start.Line, s.Range.End.Line },
		func(dst *codeindex.Symbol, start, end int) { dst.Range.Start.Line, dst.Range.End.Line = start, end })
}

// mergeOverlapping merges items that share a path and an overlapping
// [start,end] line range (get extracts that triple; widen applies a
// unioned range to the surviving item), keeping the first-seen item in
// each group as the representative. Merging cascades to a fixpoint: after
// each item is folded in, every pair of surviving groups is re-checked, so
// a later item that bridges two previously-disjoint groups (e.g. groups
// [0,5] and [20,25], then an item [4,22] that overlaps both) correctly
// collapses all three into one instead of leaving the bridged pair
// unmerged.
//
// ponytail: O(n^3) worst case (fixpoint re-scan after every merge); fine at
// typical top-k / selected-symbol sizes (single digits to low hundreds),
// switch to a union-find over a per-file sort if that ever changes.
func mergeOverlapping[T any](items []T, get func(T) (path string, start, end int), widen func(dst *T, start, end int)) []T {
	out := make([]T, 0, len(items))
	for _, item := range items {
		out = append(out, item)
		out = collapseOverlaps(out, get, widen)
	}
	return out
}

func collapseOverlaps[T any](out []T, get func(T) (path string, start, end int), widen func(dst *T, start, end int)) []T {
	for {
		mergedAny := false
		for i := 0; i < len(out) && !mergedAny; i++ {
			pi, si, ei := get(out[i])
			for j := i + 1; j < len(out); j++ {
				pj, sj, ej := get(out[j])
				if pi != pj || si > ej || sj > ei {
					continue
				}
				if sj < si {
					si = sj
				}
				if ej > ei {
					ei = ej
				}
				widen(&out[i], si, ei)
				out = append(out[:j], out[j+1:]...)
				mergedAny = true
				break
			}
		}
		if !mergedAny {
			return out
		}
	}
}

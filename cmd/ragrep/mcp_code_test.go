package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/siroio/ragrep/internal/codeindex"
	"github.com/siroio/ragrep/internal/coderetrieval"
	"github.com/siroio/ragrep/internal/codestore"
)

type fakeCodeQueryBackend struct {
	search  func(context.Context, mcpWorkspace, searchCodeInput) (searchCodeData, error)
	read    func(context.Context, mcpWorkspace, readCodeSymbolInput) (readCodeSymbolData, error)
	inspect func(context.Context, mcpWorkspace, inspectCodeRelationInput) (inspectCodeRelationData, error)
}

func (b fakeCodeQueryBackend) SearchCode(ctx context.Context, ws mcpWorkspace, in searchCodeInput) (searchCodeData, error) {
	return b.search(ctx, ws, in)
}

func (b fakeCodeQueryBackend) ReadCodeSymbol(ctx context.Context, ws mcpWorkspace, in readCodeSymbolInput) (readCodeSymbolData, error) {
	return b.read(ctx, ws, in)
}

func (b fakeCodeQueryBackend) InspectCodeRelation(ctx context.Context, ws mcpWorkspace, in inspectCodeRelationInput) (inspectCodeRelationData, error) {
	return b.inspect(ctx, ws, in)
}

func newMCPCodeWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".ragrep"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func connectCodeTools(t *testing.T, tools codeQueryTools) *mcp.ClientSession {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.1.0"}, nil)
	registerCodeQueryTools(server, tools)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { serverSession.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0.1.0"}, nil)
	session, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

func TestSearchCodeDefaultsEmptyResultsFieldsAndOptionalRoot(t *testing.T) {
	root := newMCPCodeWorkspace(t)
	otherRoot := newMCPCodeWorkspace(t)
	calls := 0
	backend := fakeCodeQueryBackend{search: func(_ context.Context, ws mcpWorkspace, in searchCodeInput) (searchCodeData, error) {
		calls++
		wantRoot := root
		if calls == 2 {
			wantRoot = otherRoot
		}
		if ws.Root != wantRoot || in.Query != "Needle" || in.Mode != "auto" || in.Limit != 5 {
			t.Fatalf("call %d workspace=%+v input=%+v, want root=%q query=Needle mode=auto limit=5", calls, ws, in, wantRoot)
		}
		if calls == 2 {
			return searchCodeData{}, nil
		}
		return searchCodeData{
			Hits:  []codestore.SymbolHit{{Key: "go:pkg/foo.go:Foo", Name: "Foo"}},
			Fresh: false, Generation: 42, Degraded: "vector_unavailable", UsedVector: true,
		}, nil
	}}
	tools := codeQueryTools{defaultRoot: root, backend: backend}

	_, out, err := tools.searchCode(context.Background(), searchCodeInput{Query: "Needle"})
	if err != nil || out.Error != nil || out.Data == nil {
		t.Fatalf("search result=%+v error=%v", out, err)
	}
	if len(out.Data.Hits) != 1 || out.Data.Hits[0].Key != "go:pkg/foo.go:Foo" || out.Data.Fresh || out.Data.Generation != 42 || out.Data.Degraded != "vector_unavailable" || !out.Data.UsedVector {
		t.Fatalf("search data=%+v", out.Data)
	}

	_, out, err = tools.searchCode(context.Background(), searchCodeInput{Query: "Needle", Root: otherRoot})
	if err != nil || out.Error != nil || out.Data == nil || out.Data.Hits == nil || len(out.Data.Hits) != 0 {
		t.Fatalf("empty search output=%+v error=%v", out, err)
	}
}

func TestSearchCodeValidationCancellationAndMissingIndex(t *testing.T) {
	root := newMCPCodeWorkspace(t)
	called := false
	tools := codeQueryTools{defaultRoot: root, backend: fakeCodeQueryBackend{search: func(context.Context, mcpWorkspace, searchCodeInput) (searchCodeData, error) {
		called = true
		return searchCodeData{}, nil
	}}}
	for _, input := range []searchCodeInput{
		{Query: "   "},
		{Query: "q", Mode: "vector"},
		{Query: "q", Mode: "bad"},
		{Query: "q", Limit: -1},
		{Query: "q", Limit: 11},
	} {
		result, out, err := tools.searchCode(context.Background(), input)
		if err != nil || !result.IsError || out.Error == nil || out.Error.Code != "invalid_argument" {
			t.Fatalf("input=%+v result=%+v output=%+v error=%v", input, result, out, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, out, err := tools.searchCode(ctx, searchCodeInput{Query: "q"})
	if err != nil || !result.IsError || out.Error == nil || out.Error.Code != "partial_failure" {
		t.Fatalf("canceled result=%+v output=%+v error=%v", result, out, err)
	}
	if called {
		t.Fatal("backend called for invalid or canceled search")
	}

	missing, err := (productionMCPBackend{}).SearchCode(context.Background(), mcpWorkspace{Root: root, CodeDB: filepath.Join(root, ".ragrep", "missing.db")}, searchCodeInput{Query: "q", Mode: "auto", Limit: 5})
	if err == nil {
		t.Fatalf("missing code index data=%+v, want error", missing)
	}
	_, failure, failureErr := mcpToolFailure[searchCodeData](err)
	if failureErr != nil || failure.Error == nil || failure.Error.Code != "index_required" {
		t.Fatalf("missing code index error=%v failure=%+v", err, failure)
	}
}

func TestSearchCodeProductionBackendPropagatesExactDaemonRequestAndTypedError(t *testing.T) {
	root := newMCPCodeWorkspace(t)
	db := filepath.Join(root, ".ragrep", "code.db")
	if err := os.WriteFile(db, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	want := searchRequest{Root: root, DB: db, Query: "Needle", Mode: "hybrid", K: 10}
	client := fakeCodeDaemonClient{search: func(ctx context.Context, got searchRequest) (searchResponse, error) {
		if ctx.Value(codeContextKey{}) != "kept" || got != want {
			t.Fatalf("context/request=%v %+v, want kept %+v", ctx.Value(codeContextKey{}), got, want)
		}
		return searchResponse{Hits: []codestore.SymbolHit{}, Fresh: true, Generation: 9, UsedVector: true}, nil
	}}
	injectCodeDaemonClient(t, &client)
	ctx := context.WithValue(context.Background(), codeContextKey{}, "kept")
	data, err := (productionMCPBackend{}).SearchCode(ctx, mcpWorkspace{Root: root, CodeDB: db}, searchCodeInput{Query: "Needle", Mode: "hybrid", Limit: 10})
	if err != nil || !data.Fresh || data.Generation != 9 || !data.UsedVector {
		t.Fatalf("data=%+v error=%v", data, err)
	}

	client.search = func(context.Context, searchRequest) (searchResponse, error) {
		return searchResponse{}, &apiError{Code: "workspace_syncing", Message: "private details", Retryable: true}
	}
	_, err = (productionMCPBackend{}).SearchCode(context.Background(), mcpWorkspace{Root: root, CodeDB: db}, searchCodeInput{Query: "Needle", Mode: "auto", Limit: 5})
	_, failure, failureErr := mcpToolFailure[searchCodeData](err)
	if failureErr != nil || failure.Error == nil || failure.Error.Code != "workspace_syncing" || !failure.Error.Retryable {
		t.Fatalf("typed daemon error=%v failure=%+v", err, failure)
	}
}

type codeContextKey struct{}

func TestReadCodeSymbolBodyRootCancellationAndStaleRecovery(t *testing.T) {
	root := newMCPCodeWorkspace(t)
	otherRoot := newMCPCodeWorkspace(t)
	sym := testSymbol()
	backend := fakeCodeQueryBackend{read: func(ctx context.Context, ws mcpWorkspace, in readCodeSymbolInput) (readCodeSymbolData, error) {
		if err := ctx.Err(); err != nil {
			return readCodeSymbolData{}, err
		}
		if ws.Root != otherRoot || in.Key != sym.Key {
			t.Fatalf("workspace=%+v input=%+v, want root=%q key=%q", ws, in, otherRoot, sym.Key)
		}
		return readCodeSymbolData{Symbol: newCodeSymbolOutput(sym, true)}, nil
	}}
	tools := codeQueryTools{defaultRoot: root, backend: backend}
	_, out, err := tools.readCodeSymbol(context.Background(), readCodeSymbolInput{Key: sym.Key, Root: otherRoot})
	if err != nil || out.Error != nil || out.Data == nil || out.Data.Symbol.Body != sym.Body {
		t.Fatalf("output=%+v error=%v", out, err)
	}
	result, invalid, err := tools.readCodeSymbol(context.Background(), readCodeSymbolInput{Key: "  "})
	if err != nil || !result.IsError || invalid.Error == nil || invalid.Error.Code != "invalid_argument" {
		t.Fatalf("blank key result=%+v output=%+v error=%v", result, invalid, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, canceled, err := tools.readCodeSymbol(ctx, readCodeSymbolInput{Key: sym.Key})
	if err != nil || !result.IsError || canceled.Error == nil || canceled.Error.Code != "partial_failure" {
		t.Fatalf("canceled result=%+v output=%+v error=%v", result, canceled, err)
	}

	staleTools := codeQueryTools{defaultRoot: root, backend: fakeCodeQueryBackend{read: func(context.Context, mcpWorkspace, readCodeSymbolInput) (readCodeSymbolData, error) {
		return readCodeSymbolData{}, ErrStaleLiveKey
	}}}
	result, stale, err := staleTools.readCodeSymbol(context.Background(), readCodeSymbolInput{Key: sym.Key})
	if err != nil || !result.IsError || stale.Error == nil || stale.Error.Code != "stale_live_key" || stale.Error.Recovery == "" {
		t.Fatalf("stale result=%+v output=%+v error=%v", result, stale, err)
	}
}

func TestReadCodeSymbolProductionBackendPropagatesExactRequestAndBody(t *testing.T) {
	root := newMCPCodeWorkspace(t)
	db := filepath.Join(root, ".ragrep", "code.db")
	if err := os.WriteFile(db, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	sym := testSymbol()
	want := getRequest{Root: root, DB: db, Key: sym.Key, Body: true}
	injectCodeDaemonClient(t, fakeCodeDaemonClient{get: func(_ context.Context, got getRequest) (codeindex.Symbol, error) {
		if got != want {
			t.Fatalf("request=%+v, want %+v", got, want)
		}
		return sym, nil
	}})
	data, err := (productionMCPBackend{}).ReadCodeSymbol(context.Background(), mcpWorkspace{Root: root, CodeDB: db}, readCodeSymbolInput{Key: sym.Key})
	if err != nil || data.Symbol.Body != sym.Body {
		t.Fatalf("data=%+v error=%v", data, err)
	}
}

func TestCodeProductionBackendMapsMissingKeysToNotFound(t *testing.T) {
	root := newMCPCodeWorkspace(t)
	db := filepath.Join(root, ".ragrep", "code.db")
	if err := os.WriteFile(db, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	notFound := &apiError{Code: "not_found", Message: "private daemon path and key"}
	injectCodeDaemonClient(t, fakeCodeDaemonClient{
		get: func(context.Context, getRequest) (codeindex.Symbol, error) {
			return codeindex.Symbol{}, notFound
		},
		expand: func(context.Context, expandRequest) ([]codeExpandTarget, error) {
			return nil, notFound
		},
	})
	tools := codeQueryTools{defaultRoot: root, backend: productionMCPBackend{}}

	readResult, readOut, err := tools.readCodeSymbol(context.Background(), readCodeSymbolInput{Key: "missing-key"})
	if err != nil || !readResult.IsError || readOut.Error == nil || readOut.Error.Code != "not_found" || readOut.Error.Message != "not found" {
		t.Fatalf("read result=%+v output=%+v error=%v", readResult, readOut, err)
	}
	relationResult, relationOut, err := tools.inspectCodeRelation(context.Background(), inspectCodeRelationInput{Key: "missing-key", Relation: "references"})
	if err != nil || !relationResult.IsError || relationOut.Error == nil || relationOut.Error.Code != "not_found" || relationOut.Error.Message != "not found" {
		t.Fatalf("relation result=%+v output=%+v error=%v", relationResult, relationOut, err)
	}
}

func TestInspectCodeRelationAcceptsExactlyFiveRelationsAndReturnsTargets(t *testing.T) {
	root := newMCPCodeWorkspace(t)
	wantRelations := []string{"definition", "references", "callers", "callees", "tests"}
	for _, relation := range wantRelations {
		t.Run(relation, func(t *testing.T) {
			backend := fakeCodeQueryBackend{inspect: func(_ context.Context, ws mcpWorkspace, in inspectCodeRelationInput) (inspectCodeRelationData, error) {
				if ws.Root != root || in.Key != "symbol-key" || in.Relation != relation {
					t.Fatalf("workspace=%+v input=%+v", ws, in)
				}
				return inspectCodeRelationData{Targets: []codeExpandTarget{
					{Relation: relation, Resolved: true, Key: "target", Path: "pkg/target.go", StartLine: 4, EndLine: 8},
					{Relation: relation, Resolved: false, Path: "pkg/missing.go", Line: 12, Character: 3},
				}}, nil
			}}
			_, out, err := (codeQueryTools{defaultRoot: root, backend: backend}).inspectCodeRelation(context.Background(), inspectCodeRelationInput{Key: "symbol-key", Relation: relation})
			if err != nil || out.Error != nil || out.Data == nil || len(out.Data.Targets) != 2 || !out.Data.Targets[0].Resolved || out.Data.Targets[1].Resolved {
				t.Fatalf("output=%+v error=%v", out, err)
			}
		})
	}
	tools := codeQueryTools{defaultRoot: root, backend: fakeCodeQueryBackend{inspect: func(context.Context, mcpWorkspace, inspectCodeRelationInput) (inspectCodeRelationData, error) {
		t.Fatal("backend called for invalid relation")
		return inspectCodeRelationData{}, nil
	}}}
	for _, input := range []inspectCodeRelationInput{{Key: "", Relation: "definition"}, {Key: "key", Relation: ""}, {Key: "key", Relation: "implements"}} {
		result, out, err := tools.inspectCodeRelation(context.Background(), input)
		if err != nil || !result.IsError || out.Error == nil || out.Error.Code != "invalid_argument" {
			t.Fatalf("input=%+v result=%+v output=%+v error=%v", input, result, out, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, out, err := tools.inspectCodeRelation(ctx, inspectCodeRelationInput{Key: "key", Relation: "definition"})
	if err != nil || !result.IsError || out.Error == nil || out.Error.Code != "partial_failure" {
		t.Fatalf("canceled result=%+v output=%+v error=%v", result, out, err)
	}
}

func TestInspectCodeRelationProductionBackendPropagatesExactRequest(t *testing.T) {
	root := newMCPCodeWorkspace(t)
	db := filepath.Join(root, ".ragrep", "code.db")
	if err := os.WriteFile(db, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	want := expandRequest{Root: root, DB: db, Key: "symbol-key", Relation: "callers"}
	wantTargets := []codeExpandTarget{{Relation: "callers", Resolved: true, Key: "caller", Path: "pkg/caller.go"}}
	injectCodeDaemonClient(t, fakeCodeDaemonClient{expand: func(_ context.Context, got expandRequest) ([]codeExpandTarget, error) {
		if got != want {
			t.Fatalf("request=%+v, want %+v", got, want)
		}
		return wantTargets, nil
	}})
	data, err := (productionMCPBackend{}).InspectCodeRelation(context.Background(), mcpWorkspace{Root: root, CodeDB: db}, inspectCodeRelationInput{Key: "symbol-key", Relation: "callers"})
	if err != nil || len(data.Targets) != 1 || data.Targets[0].Key != "caller" {
		t.Fatalf("data=%+v error=%v", data, err)
	}
}

func TestCodeHandlersRegisterReadOnlyClosedWorldTools(t *testing.T) {
	backend := fakeCodeQueryBackend{
		search: func(context.Context, mcpWorkspace, searchCodeInput) (searchCodeData, error) {
			return searchCodeData{}, errors.New("unused")
		},
		read: func(context.Context, mcpWorkspace, readCodeSymbolInput) (readCodeSymbolData, error) {
			return readCodeSymbolData{}, errors.New("unused")
		},
		inspect: func(context.Context, mcpWorkspace, inspectCodeRelationInput) (inspectCodeRelationData, error) {
			return inspectCodeRelationData{}, errors.New("unused")
		},
	}
	session := connectCodeTools(t, codeQueryTools{defaultRoot: newMCPCodeWorkspace(t), backend: backend})
	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"search_code": true, "read_code_symbol": true, "inspect_code_relation": true}
	for _, tool := range listed.Tools {
		if !want[tool.Name] {
			t.Fatalf("unexpected tool %q", tool.Name)
		}
		delete(want, tool.Name)
		a := tool.Annotations
		if a == nil || !a.ReadOnlyHint || a.OpenWorldHint == nil || *a.OpenWorldHint {
			t.Fatalf("tool %q annotations=%+v", tool.Name, a)
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing tools: %v", want)
	}
}

type fakeCodeContextBackend struct {
	build  func(context.Context, mcpWorkspace, buildCodeContextInput) (buildCodeContextData, error)
	verify func(context.Context, mcpWorkspace, verifyCodeContextInput) (verifyCodeContextData, error)
}

func (b fakeCodeContextBackend) BuildCodeContext(ctx context.Context, ws mcpWorkspace, in buildCodeContextInput) (buildCodeContextData, error) {
	return b.build(ctx, ws, in)
}

func (b fakeCodeContextBackend) VerifyCodeContext(ctx context.Context, ws mcpWorkspace, in verifyCodeContextInput) (verifyCodeContextData, error) {
	return b.verify(ctx, ws, in)
}

func TestBuildCodeContextDefaultsAndPreservesOrderedSelection(t *testing.T) {
	root := newMCPCodeWorkspace(t)
	otherRoot := newMCPCodeWorkspace(t)
	wantKeys := []string{"first", "second", "third"}
	wantData := codePackOutput{
		Pack: coderetrieval.ContextPack{
			Candidates: []codestore.SymbolHit{{Key: "first", Name: "First"}},
			Budget:     20000, UsedChars: 123, Truncated: true, Skipped: []string{"third"},
		},
		Manifest: coderetrieval.Manifest{IndexRevision: "rev", Symbols: []coderetrieval.SymbolRef{{Key: "first", Path: "pkg/a.go"}}},
		Fresh:    false, Generation: 7,
	}
	backend := fakeCodeContextBackend{build: func(ctx context.Context, ws mcpWorkspace, in buildCodeContextInput) (buildCodeContextData, error) {
		if ctx.Err() != nil || ws.Root != otherRoot || in.Query != "Needle" || in.Budget != 20000 || !reflect.DeepEqual(in.SelectedKeys, wantKeys) {
			t.Fatalf("context/workspace/input=%v %+v %+v", ctx.Err(), ws, in)
		}
		return wantData, nil
	}}
	tools := codeContextTools{defaultRoot: root, backend: backend}
	_, out, err := tools.buildCodeContext(context.Background(), buildCodeContextInput{Query: "Needle", SelectedKeys: wantKeys, Root: otherRoot})
	if err != nil || out.Error != nil || out.Data == nil || !reflect.DeepEqual(*out.Data, wantData) {
		t.Fatalf("output=%+v error=%v", out, err)
	}
}

func TestBuildCodeContextValidationCancellationAndStaleKey(t *testing.T) {
	root := newMCPCodeWorkspace(t)
	called := false
	tools := codeContextTools{defaultRoot: root, backend: fakeCodeContextBackend{build: func(context.Context, mcpWorkspace, buildCodeContextInput) (buildCodeContextData, error) {
		called = true
		return buildCodeContextData{}, nil
	}}}
	for _, input := range []buildCodeContextInput{
		{Query: " "},
		{Query: "q", Budget: -1},
		{Query: "q", SelectedKeys: []string{"a", "b", "c", "d"}},
	} {
		result, out, err := tools.buildCodeContext(context.Background(), input)
		if err != nil || !result.IsError || out.Error == nil || out.Error.Code != "invalid_argument" {
			t.Fatalf("input=%+v result=%+v output=%+v error=%v", input, result, out, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, out, err := tools.buildCodeContext(ctx, buildCodeContextInput{Query: "q"})
	if err != nil || !result.IsError || out.Error == nil || out.Error.Code != "partial_failure" {
		t.Fatalf("canceled result=%+v output=%+v error=%v", result, out, err)
	}
	if called {
		t.Fatal("backend called for invalid or canceled input")
	}

	staleTools := codeContextTools{defaultRoot: root, backend: fakeCodeContextBackend{build: func(context.Context, mcpWorkspace, buildCodeContextInput) (buildCodeContextData, error) {
		return buildCodeContextData{}, ErrStaleLiveKey
	}}}
	result, out, err = staleTools.buildCodeContext(context.Background(), buildCodeContextInput{Query: "q", SelectedKeys: []string{"stale"}})
	if err != nil || !result.IsError || out.Error == nil || out.Error.Code != "stale_live_key" || out.Error.Recovery == "" {
		t.Fatalf("stale result=%+v output=%+v error=%v", result, out, err)
	}
}

func TestBuildCodeContextProductionBackendPropagatesPackRequest(t *testing.T) {
	root := newMCPCodeWorkspace(t)
	db := filepath.Join(root, ".ragrep", "code.db")
	if err := os.WriteFile(db, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	want := packRequest{Root: root, DB: db, Query: "Needle", K: 10, Budget: 4321, SelectedKeys: []string{"a", "b"}}
	wantData := codePackOutput{Pack: coderetrieval.ContextPack{Budget: 4321}, Fresh: true, Generation: 11}
	injectCodeDaemonClient(t, fakeCodeDaemonClient{pack: func(ctx context.Context, got packRequest) (codePackOutput, error) {
		if ctx.Value(codeContextKey{}) != "kept" || !reflect.DeepEqual(got, want) {
			t.Fatalf("context/request=%v %+v, want kept %+v", ctx.Value(codeContextKey{}), got, want)
		}
		return wantData, nil
	}})
	ctx := context.WithValue(context.Background(), codeContextKey{}, "kept")
	data, err := (productionMCPBackend{}).BuildCodeContext(ctx, mcpWorkspace{Root: root, CodeDB: db}, buildCodeContextInput{Query: "Needle", Budget: 4321, SelectedKeys: []string{"a", "b"}})
	if err != nil || !reflect.DeepEqual(data, wantData) {
		t.Fatalf("data=%+v error=%v", data, err)
	}
}

func TestVerifyCodeContextPassesManifestInMemoryAndCleanFalseIsData(t *testing.T) {
	root := newMCPCodeWorkspace(t)
	otherRoot := newMCPCodeWorkspace(t)
	wantManifest := coderetrieval.Manifest{IndexRevision: "rev", Symbols: []coderetrieval.SymbolRef{{Key: "a", Path: "pkg/a.go", FileHash: "hash"}}}
	before, err := os.ReadDir(otherRoot)
	if err != nil {
		t.Fatal(err)
	}
	backend := fakeCodeContextBackend{verify: func(ctx context.Context, ws mcpWorkspace, in verifyCodeContextInput) (verifyCodeContextData, error) {
		if ctx.Err() != nil || ws.Root != otherRoot || !reflect.DeepEqual(in.Manifest, wantManifest) {
			t.Fatalf("context/workspace/input=%v %+v %+v", ctx.Err(), ws, in)
		}
		return codeVerifyOutput{Entries: []codeVerifyEntry{{Key: "a", Path: "pkg/a.go", Stale: true, Resolved: true, ResolvedKey: "a"}}, Clean: false}, nil
	}}
	_, out, err := (codeContextTools{defaultRoot: root, backend: backend}).verifyCodeContext(context.Background(), verifyCodeContextInput{Manifest: wantManifest, Root: otherRoot})
	if err != nil || out.Error != nil || out.Data == nil || out.Data.Clean || len(out.Data.Entries) != 1 {
		t.Fatalf("output=%+v error=%v", out, err)
	}
	after, err := os.ReadDir(otherRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("workspace entries changed: before=%v after=%v", before, after)
	}
}

func TestVerifyCodeContextRejectsEmptyManifestAndCancellation(t *testing.T) {
	root := newMCPCodeWorkspace(t)
	called := false
	tools := codeContextTools{defaultRoot: root, backend: fakeCodeContextBackend{verify: func(context.Context, mcpWorkspace, verifyCodeContextInput) (verifyCodeContextData, error) {
		called = true
		return verifyCodeContextData{}, nil
	}}}
	result, out, err := tools.verifyCodeContext(context.Background(), verifyCodeContextInput{})
	if err != nil || !result.IsError || out.Error == nil || out.Error.Code != "invalid_argument" {
		t.Fatalf("empty manifest result=%+v output=%+v error=%v", result, out, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, out, err = tools.verifyCodeContext(ctx, verifyCodeContextInput{Manifest: coderetrieval.Manifest{Symbols: []coderetrieval.SymbolRef{{Key: "a"}}}})
	if err != nil || !result.IsError || out.Error == nil || out.Error.Code != "partial_failure" {
		t.Fatalf("canceled result=%+v output=%+v error=%v", result, out, err)
	}
	if called {
		t.Fatal("backend called for empty or canceled input")
	}
}

func TestVerifyCodeContextProductionBackendPropagatesManifestDirectly(t *testing.T) {
	root := newMCPCodeWorkspace(t)
	db := filepath.Join(root, ".ragrep", "code.db")
	if err := os.WriteFile(db, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := coderetrieval.Manifest{Symbols: []coderetrieval.SymbolRef{{Key: "a", Path: "pkg/a.go"}}}
	want := verifyRequest{Root: root, DB: db, Manifest: manifest}
	injectCodeDaemonClient(t, fakeCodeDaemonClient{verify: func(ctx context.Context, got verifyRequest) (codeVerifyOutput, error) {
		if ctx.Value(codeContextKey{}) != "kept" || !reflect.DeepEqual(got, want) {
			t.Fatalf("context/request=%v %+v, want kept %+v", ctx.Value(codeContextKey{}), got, want)
		}
		return codeVerifyOutput{Clean: false}, nil
	}})
	ctx := context.WithValue(context.Background(), codeContextKey{}, "kept")
	data, err := (productionMCPBackend{}).VerifyCodeContext(ctx, mcpWorkspace{Root: root, CodeDB: db}, verifyCodeContextInput{Manifest: manifest})
	if err != nil || data.Clean {
		t.Fatalf("data=%+v error=%v", data, err)
	}
}

func TestContextHandlersRegisterExactToolProtocol(t *testing.T) {
	server, err := newRagrepMCPServer(newMCPCodeWorkspace(t), mcpBackends{})
	if err != nil {
		t.Fatal(err)
	}
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { serverSession.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0.1.0"}, nil)
	session, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(listed.Tools))
	descriptionParts := map[string][]string{
		"search_documents":      {"Use to find", "read_document"},
		"read_document":         {"Use after search_documents", "search_documents or build_code_context"},
		"add_document":          {"Use only when", "search_documents or read_document"},
		"reindex_documents":     {"Use when", "search_documents"},
		"search_code":           {"Use to find", "read_code_symbol"},
		"read_code_symbol":      {"Use after search_code", "inspect_code_relation or build_code_context"},
		"inspect_code_relation": {"Use after read_code_symbol", "read_code_symbol or build_code_context"},
		"build_code_context":    {"Use after search_code", "verify_code_context"},
		"verify_code_context":   {"Use immediately before", "build_code_context again"},
	}
	readOnly := map[string]bool{
		"search_documents": true, "read_document": true,
		"search_code": true, "read_code_symbol": true, "inspect_code_relation": true,
		"build_code_context": true, "verify_code_context": true,
	}
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
		schema, ok := tool.InputSchema.(map[string]any)
		if !ok || schema["type"] != "object" {
			t.Fatalf("tool %q input schema=%#v, want object", tool.Name, tool.InputSchema)
		}
		outputSchema, ok := tool.OutputSchema.(map[string]any)
		if !ok || outputSchema["type"] != "object" {
			t.Fatalf("tool %q output schema=%#v, want object", tool.Name, tool.OutputSchema)
		}
		if tool.Annotations == nil || tool.Annotations.OpenWorldHint == nil || *tool.Annotations.OpenWorldHint {
			t.Fatalf("tool %q description/annotations=%q %+v", tool.Name, tool.Description, tool.Annotations)
		}
		for _, part := range descriptionParts[tool.Name] {
			if !strings.Contains(tool.Description, part) {
				t.Fatalf("tool %q description=%q, want transition part %q", tool.Name, tool.Description, part)
			}
		}
		if tool.Annotations.ReadOnlyHint != readOnly[tool.Name] {
			t.Fatalf("tool %q readOnly=%v, want %v", tool.Name, tool.Annotations.ReadOnlyHint, readOnly[tool.Name])
		}
		if tool.Name == "add_document" && (tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint || tool.Annotations.IdempotentHint) {
			t.Fatalf("add_document annotations=%+v", tool.Annotations)
		}
		if tool.Name == "reindex_documents" && (tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint || !tool.Annotations.IdempotentHint) {
			t.Fatalf("reindex_documents annotations=%+v", tool.Annotations)
		}
	}
	sort.Strings(names)
	want := []string{"add_document", "build_code_context", "inspect_code_relation", "read_code_symbol", "read_document", "reindex_documents", "search_code", "search_documents", "verify_code_context"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("tool names=%v, want %v", names, want)
	}
}

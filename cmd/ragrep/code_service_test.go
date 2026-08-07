package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/siroio/ragrep/internal/codeindex"
	"github.com/siroio/ragrep/internal/codestore"
)

type testCodeServiceWorkspace struct {
	*workspaceState
	root  string
	store *codestore.Store
}

type serviceTestEmbedder struct {
	calls   atomic.Int32
	err     error
	started chan struct{}
	release <-chan struct{}
}

func (e *serviceTestEmbedder) Embed(text string) ([]float32, error) {
	if e.calls.Add(1) == 1 && e.started != nil {
		close(e.started)
		<-e.release
	}
	if e.err != nil {
		return nil, e.err
	}
	return fakeCodeEmbed(text)
}

func (*serviceTestEmbedder) Close() {}

func newTestCodeService(t *testing.T) (*codeService, *testCodeServiceWorkspace, *serviceTestEmbedder) {
	t.Helper()
	root, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, err := codestore.Open(filepath.Join(t.TempDir(), "code.db"), "test-model", codeEmbedDim)
	if err != nil {
		t.Fatal(err)
	}
	state, err := newWorkspaceState(root, store, []string{"."}, "go")
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	ws := &testCodeServiceWorkspace{workspaceState: state, root: root, store: store}
	embedder := new(serviceTestEmbedder)
	svc := newCodeService(func(got string) (*workspaceState, error) {
		if got != root {
			return nil, fmt.Errorf("unknown workspace %q", got)
		}
		return state, nil
	}, newEmbeddingPool(func() (textEmbedder, error) { return embedder, nil }), nil)
	t.Cleanup(func() {
		_ = svc.Close()
		_ = state.Close()
		_ = store.Close()
	})
	return svc, ws, embedder
}

func (w *testCodeServiceWorkspace) save(t *testing.T, rel, body string) string {
	t.Helper()
	writeWorkspaceFile(t, w.root, rel, body)
	return codeindex.FileHash([]byte(body))
}

func serviceSymbol(path, name, body string) codeindex.Symbol {
	sym := codeindex.Symbol{
		Key:           path + ":" + name,
		Language:      "go",
		Kind:          "function",
		Name:          name,
		QualifiedName: name,
		Signature:     "func " + name + "()",
		Path:          path,
		Range:         codeindex.Range{End: codeindex.Position{Line: 1}},
		Body:          body,
		BodyHash:      codeindex.FileHash([]byte(body)),
	}
	sym.EmbeddingText = codeindex.RenderEmbeddingText(sym)
	return sym
}

func putServiceSymbol(t *testing.T, store *codestore.Store, sym codeindex.Symbol, fileHash string) {
	t.Helper()
	if _, err := store.UpsertSymbols(sym.Path, fileHash, []codeindex.Symbol{sym}, 0, fakeCodeEmbed); err != nil {
		t.Fatal(err)
	}
}

func TestCodeServiceSearchReturnsFreshGeneration(t *testing.T) {
	svc, ws, embedder := newTestCodeService(t)
	ws.save(t, "service.go", "package service\nfunc CurrentHandler() {}")

	resp, err := svc.Search(context.Background(), searchRequest{Root: ws.root, Query: "CurrentHandler"})
	if err != nil || !resp.Fresh || resp.Generation == 0 || len(resp.Hits) != 1 {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	if !resp.Hits[0].Live || embedder.calls.Load() != 0 || resp.UsedVector {
		t.Fatalf("hit=%+v embed calls=%d usedVector=%v", resp.Hits[0], embedder.calls.Load(), resp.UsedVector)
	}
}

func TestCodeServiceSearchWaitsForBarrier(t *testing.T) {
	svc, ws, _ := newTestCodeService(t)
	ws.save(t, "service.go", "package service\nfunc WaitingHandler() {}")
	unblock := make(chan struct{})
	original := ws.enumerate
	ws.enumerate = func(root, ext string) ([]string, error) {
		<-unblock
		return original(root, ext)
	}
	t.Cleanup(func() { close(unblock) })

	resp, err := svc.Search(context.Background(), searchRequest{Root: ws.root, Query: "WaitingHandler"})
	if !errors.Is(err, ErrWorkspaceSyncing) || len(resp.Hits) != 0 || resp.Fresh {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
}

func TestCodeServiceSearchDefaultsAndCapsKAtFive(t *testing.T) {
	svc, ws, _ := newTestCodeService(t)
	for i := range 6 {
		ws.save(t, fmt.Sprintf("handler%d.go", i), fmt.Sprintf("package service\nfunc Handler%d() { CommonOperation() }", i))
	}

	for _, k := range []int{0, 99} {
		resp, err := svc.Search(context.Background(), searchRequest{Root: ws.root, Query: "CommonOperation", K: k})
		if err != nil || len(resp.Hits) != 5 {
			t.Fatalf("K=%d hits=%d err=%v", k, len(resp.Hits), err)
		}
	}
}

func TestCodeServiceSearchUsesEmbeddingForAmbiguousQuery(t *testing.T) {
	svc, ws, embedder := newTestCodeService(t)
	ws.save(t, "service.go", "package service\nfunc HandleRequest() { ValidateToken() }")

	resp, err := svc.Search(context.Background(), searchRequest{Root: ws.root, Query: "request validation"})
	if err != nil || embedder.calls.Load() != 1 || !resp.UsedVector {
		t.Fatalf("resp=%+v embed calls=%d err=%v", resp, embedder.calls.Load(), err)
	}
}

func TestCodeServiceSearchDegradesToTextWhenEmbeddingFails(t *testing.T) {
	svc, ws, embedder := newTestCodeService(t)
	embedder.err = errors.New("embedding unavailable")
	ws.save(t, "service.go", "package service\nfunc HandleRequest() { ValidateToken() }")

	resp, err := svc.Search(context.Background(), searchRequest{Root: ws.root, Query: "Handle request"})
	if err != nil || len(resp.Hits) != 1 || resp.Degraded != "vector_unavailable" || resp.UsedVector {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
}

func TestCodeServiceSearchExactDurableHitSkipsEmbedding(t *testing.T) {
	svc, ws, embedder := newTestCodeService(t)
	body := "func DurableHandler() {}"
	putServiceSymbol(t, ws.store, serviceSymbol("durable.go", "DurableHandler", body), ws.save(t, "durable.go", body))

	resp, err := svc.Search(context.Background(), searchRequest{Root: ws.root, Query: "DurableHandler"})
	if err != nil || len(resp.Hits) != 1 || !resp.Hits[0].ExactMatch || embedder.calls.Load() != 0 || resp.UsedVector {
		t.Fatalf("resp=%+v embed calls=%d err=%v", resp, embedder.calls.Load(), err)
	}
}

func TestCodeServiceSearchMarksLiveAndDurableExactHits(t *testing.T) {
	svc, ws, embedder := newTestCodeService(t)
	durableBody := "func SharedExact() {}"
	putServiceSymbol(t, ws.store, serviceSymbol("durable.go", "SharedExact", durableBody), ws.save(t, "durable.go", durableBody))
	ws.save(t, "live.go", "package service\nfunc SharedExact() { changed() }")

	resp, err := svc.Search(context.Background(), searchRequest{Root: ws.root, Query: "SharedExact"})
	if err != nil || len(resp.Hits) != 2 || !resp.Hits[0].ExactMatch || !resp.Hits[1].ExactMatch || embedder.calls.Load() != 0 {
		t.Fatalf("resp=%+v embed calls=%d err=%v", resp, embedder.calls.Load(), err)
	}
}

func TestCodeServiceSearchExistingPathSkipsEmbedding(t *testing.T) {
	svc, ws, embedder := newTestCodeService(t)
	body := "func PathHandler() {}"
	putServiceSymbol(t, ws.store, serviceSymbol("pkg/service.go", "PathHandler", body), ws.save(t, "pkg/service.go", body))

	resp, err := svc.Search(context.Background(), searchRequest{Root: ws.root, Query: "pkg/service.go"})
	if err != nil || embedder.calls.Load() != 0 || resp.UsedVector {
		t.Fatalf("resp=%+v embed calls=%d err=%v", resp, embedder.calls.Load(), err)
	}
}

func TestCodeServiceSearchDurableExactPathReturnsPinnedSymbols(t *testing.T) {
	svc, ws, embedder := newTestCodeService(t)
	body := "func FirstPathHandler() {}\nfunc SecondPathHandler() {}"
	first := serviceSymbol("pkg/service.go", "FirstPathHandler", "func FirstPathHandler() {}")
	second := serviceSymbol("pkg/service.go", "SecondPathHandler", "func SecondPathHandler() {}")
	second.Range = codeindex.Range{Start: codeindex.Position{Line: 1}, End: codeindex.Position{Line: 2}}
	if _, err := ws.store.UpsertSymbols("pkg/service.go", ws.save(t, "pkg/service.go", body), []codeindex.Symbol{first, second}, 0, fakeCodeEmbed); err != nil {
		t.Fatal(err)
	}

	resp, err := svc.Search(context.Background(), searchRequest{Root: ws.root, Query: "pkg/service.go"})
	if err != nil || len(resp.Hits) != 2 || resp.Hits[0].Path != "pkg/service.go" || resp.Hits[1].Path != "pkg/service.go" || !resp.Hits[0].ExactMatch || !resp.Hits[1].ExactMatch || embedder.calls.Load() != 0 || resp.UsedVector {
		t.Fatalf("resp=%+v embed calls=%d err=%v", resp, embedder.calls.Load(), err)
	}
}

func TestCodeServiceSearchJapaneseLiveNaturalQueryUsesVector(t *testing.T) {
	svc, ws, embedder := newTestCodeService(t)
	ws.save(t, "service.go", "package service\n// 認証処理を確認する\nfunc ValidateToken() {}")

	resp, err := svc.Search(context.Background(), searchRequest{Root: ws.root, Query: "認証処理を確認する"})
	if err != nil || len(resp.Hits) != 1 || embedder.calls.Load() != 1 || !resp.UsedVector {
		t.Fatalf("resp=%+v embed calls=%d err=%v", resp, embedder.calls.Load(), err)
	}
}

func TestCodeServiceSearchMergesLiveAndDurableHits(t *testing.T) {
	svc, ws, _ := newTestCodeService(t)
	body := "func DurableShared() { SharedOperation() }"
	putServiceSymbol(t, ws.store, serviceSymbol("durable.go", "DurableShared", body), ws.save(t, "durable.go", body))
	ws.save(t, "live.go", "package service\nfunc LiveShared() { SharedOperation() }")

	resp, err := svc.Search(context.Background(), searchRequest{Root: ws.root, Query: "SharedOperation"})
	if err != nil || len(resp.Hits) != 2 {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	paths := resp.Hits[0].Path + "," + resp.Hits[1].Path
	if !strings.Contains(paths, "durable.go") || !strings.Contains(paths, "live.go") {
		t.Fatalf("paths=%q", paths)
	}
}

func TestCodeServiceSearchNeverReturnsDurableHitFromLivePath(t *testing.T) {
	svc, ws, _ := newTestCodeService(t)
	putServiceSymbol(t, ws.store, serviceSymbol("service.go", "OldHandler", "func OldHandler() { SharedOperation() }"), "old-hash")
	ws.save(t, "service.go", "package service\nfunc NewHandler() { SharedOperation() }")

	resp, err := svc.Search(context.Background(), searchRequest{Root: ws.root, Query: "SharedOperation"})
	if err != nil || len(resp.Hits) != 1 || !resp.Hits[0].Live || resp.Hits[0].Path != "service.go" {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
}

func TestCodeServiceSearchKeepsAdjacentDurableRanges(t *testing.T) {
	svc, ws, _ := newTestCodeService(t)
	body := "func First() { SharedAdjacent() }\nfunc Second() { SharedAdjacent() }"
	first := serviceSymbol("adjacent.go", "First", "func First() { SharedAdjacent() }")
	second := serviceSymbol("adjacent.go", "Second", "func Second() { SharedAdjacent() }")
	second.Range = codeindex.Range{Start: codeindex.Position{Line: 1}, End: codeindex.Position{Line: 2}}
	if _, err := ws.store.UpsertSymbols("adjacent.go", ws.save(t, "adjacent.go", body), []codeindex.Symbol{first, second}, 0, fakeCodeEmbed); err != nil {
		t.Fatal(err)
	}

	resp, err := svc.Search(context.Background(), searchRequest{Root: ws.root, Query: "SharedAdjacent"})
	if err != nil || len(resp.Hits) != 2 {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
}

func TestCodeServiceGetRoutesLiveAndDurableKeys(t *testing.T) {
	svc, ws, _ := newTestCodeService(t)
	durable := serviceSymbol("durable.go", "DurableGet", "func DurableGet() {}")
	putServiceSymbol(t, ws.store, durable, ws.save(t, durable.Path, durable.Body))
	if got, err := svc.Get(context.Background(), getRequest{Root: ws.root, Key: durable.Key}); err != nil || got.Body != "" {
		t.Fatalf("durable metadata-only got=%+v err=%v", got, err)
	}
	if got, err := svc.Get(context.Background(), getRequest{Root: ws.root, Key: durable.Key, Body: true}); err != nil || got.Body != durable.Body {
		t.Fatalf("durable with body got=%+v err=%v", got, err)
	}

	ws.save(t, "live.go", "package service\nfunc LiveGet() {}")
	search, err := svc.Search(context.Background(), searchRequest{Root: ws.root, Query: "LiveGet"})
	if err != nil || len(search.Hits) != 1 {
		t.Fatalf("search=%+v err=%v", search, err)
	}
	if got, err := svc.Get(context.Background(), getRequest{Root: ws.root, Key: search.Hits[0].Key, Body: true}); err != nil || !strings.Contains(got.Body, "LiveGet") {
		t.Fatalf("live got=%+v err=%v", got, err)
	}
}

func TestCodeServiceGetMapsStaleLiveKeyWithoutFallback(t *testing.T) {
	svc, ws, _ := newTestCodeService(t)
	ws.save(t, "service.go", "package service\nfunc FirstVersion() {}")
	first, err := svc.Search(context.Background(), searchRequest{Root: ws.root, Query: "FirstVersion"})
	if err != nil || len(first.Hits) != 1 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	ws.save(t, "service.go", "package service\nfunc SecondVersion() {}")
	if _, err := svc.Search(context.Background(), searchRequest{Root: ws.root, Query: "SecondVersion"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Get(context.Background(), getRequest{Root: ws.root, Key: first.Hits[0].Key, Body: true}); !errors.Is(err, ErrStaleLiveKey) {
		t.Fatalf("Get stale key err=%v, want stale_live_key", err)
	}
}

func TestCodeServiceGetRejectsDurableKeyAfterSave(t *testing.T) {
	svc, ws, _ := newTestCodeService(t)
	body := "package service\nfunc DurableBeforeSave() {}"
	durable := serviceSymbol("service.go", "DurableBeforeSave", "func DurableBeforeSave() {}")
	putServiceSymbol(t, ws.store, durable, ws.save(t, "service.go", body))

	search, err := svc.Search(context.Background(), searchRequest{Root: ws.root, Query: durable.Name})
	if err != nil || len(search.Hits) != 1 || search.Hits[0].Key != durable.Key {
		t.Fatalf("search=%+v err=%v", search, err)
	}
	ws.save(t, "service.go", "package service\nfunc ChangedAfterSearch() {}")

	if _, err := svc.Get(context.Background(), getRequest{Root: ws.root, Key: durable.Key, Body: true}); !errors.Is(err, ErrStaleLiveKey) {
		t.Fatalf("Get durable key after save err=%v, want stale_live_key", err)
	}
}

func TestCodeServiceSearchSnapshotBlocksPartialWatcherGeneration(t *testing.T) {
	svc, ws, embedder := newTestCodeService(t)
	oldBody := "package service\nfunc OldHandler() { SharedBehavior() }"
	ws.save(t, "service.go", oldBody)
	generation := barrierGeneration(t, ws.workspaceState)
	started := make(chan struct{})
	release := make(chan struct{})
	embedder.started = started
	embedder.release = release

	searched := make(chan struct {
		resp searchResponse
		err  error
	}, 1)
	go func() {
		resp, err := svc.Search(context.Background(), searchRequest{Root: ws.root, Query: "SharedBehavior details"})
		searched <- struct {
			resp searchResponse
			err  error
		}{resp, err}
	}()
	<-started
	ws.save(t, "service.go", "package service\nfunc NewHandler() {}")
	refreshed := make(chan error, 1)
	go func() { refreshed <- ws.refreshPaths([]string{filepath.Join(ws.root, "service.go")}) }()
	select {
	case err := <-refreshed:
		close(release)
		t.Fatalf("watcher refresh crossed active search snapshot: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	result := <-searched
	if result.err != nil || result.resp.Generation != generation || len(result.resp.Hits) != 1 || result.resp.Hits[0].Generation != generation {
		t.Fatalf("resp=%+v err=%v generation=%d", result.resp, result.err, generation)
	}
	if err := <-refreshed; err != nil {
		t.Fatal(err)
	}
}

func TestCodeServiceSearchBarrierUsesTwoHundredMillisecondTimeout(t *testing.T) {
	svc, ws, _ := newTestCodeService(t)
	unblock := make(chan struct{})
	ws.enumerate = func(string, string) ([]string, error) {
		<-unblock
		return nil, nil
	}
	t.Cleanup(func() { close(unblock) })
	started := time.Now()
	_, err := svc.Search(context.Background(), searchRequest{Root: ws.root, Query: "anything"})
	if !errors.Is(err, ErrWorkspaceSyncing) || time.Since(started) < 150*time.Millisecond {
		t.Fatalf("elapsed=%v err=%v", time.Since(started), err)
	}
}

const blockingDocumentSymbolServerSrc = `package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/textproto"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

var writeMu sync.Mutex
var documentRequests atomic.Int32

func reply(id json.RawMessage, result string) {
	resp := fmt.Sprintf("{\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":%s}", id, result)
	writeMu.Lock()
	defer writeMu.Unlock()
	fmt.Fprintf(os.Stdout, "Content-Length: %d\r\n\r\n%s", len(resp), resp)
}

func main() {
	tp := textproto.NewReader(bufio.NewReader(os.Stdin))
	for {
		hdr, err := tp.ReadMIMEHeader()
		if err != nil { return }
		n, err := strconv.Atoi(hdr.Get("Content-Length"))
		if err != nil { return }
		body := make([]byte, n)
		if _, err := io.ReadFull(tp.R, body); err != nil { return }
		var msg map[string]json.RawMessage
		if err := json.Unmarshal(body, &msg); err != nil { return }
		id, hasID := msg["id"]
		if !hasID { continue }
		var method string
		_ = json.Unmarshal(msg["method"], &method)
		result := "null"
		switch method {
		case "initialize":
			result = ` + "`" + `{"capabilities":{"documentSymbolProvider":true}}` + "`" + `
		case "textDocument/documentSymbol":
			first := documentRequests.Add(1) == 1
			go func() {
				if first {
					_ = os.WriteFile("document-symbol.started", []byte("started"), 0644)
					for {
						if _, err := os.Stat("document-symbol.continue"); err == nil { break }
						time.Sleep(5 * time.Millisecond)
					}
				}
				reply(id, ` + "`" + `[{"name":"PromotedHandler","kind":12,"range":{"start":{"line":2,"character":0},"end":{"line":4,"character":1}},"selectionRange":{"start":{"line":2,"character":5},"end":{"line":2,"character":20}}}]` + "`" + `)
			}()
			continue
		}
		reply(id, result)
	}
}
`

func useBlockingDocumentSymbolServer(t *testing.T, svc *codeService, root string) (started, release string) {
	t.Helper()
	exe := buildFakeLSPServerFromSrc(t, t.TempDir(), "blocking-document-symbol", blockingDocumentSymbolServerSrc)
	_ = svc.lsps.Close()
	svc.lsps = newLSPPool(time.Hour, func(context.Context, string, string) (*pooledLanguageServer, error) {
		client, _, err := startLanguageServer(exe, root)
		if err != nil {
			return nil, err
		}
		return &pooledLanguageServer{client: client, close: client.Close}, nil
	})
	return filepath.Join(root, "document-symbol.started"), filepath.Join(root, "document-symbol.continue")
}

func waitForTestPath(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

func TestConfirmPathOlderPromotionCannotOverwriteNewerCompletion(t *testing.T) {
	svc, ws, _ := newTestCodeService(t)
	started, release := useBlockingDocumentSymbolServer(t, svc, ws.root)
	bodyA := "package service\n\nfunc PromotedHandler() {\n\t// version A\n}"
	bodyB := "package service\n\nfunc PromotedHandler() {\n\t// version B\n}"
	hashA := ws.save(t, "service.go", bodyA)
	barrierGeneration(t, ws.workspaceState)

	confirmedA := make(chan error, 1)
	go func() {
		confirmedA <- svc.ConfirmPath(context.Background(), ws.root, "service.go", hashA)
	}()
	waitForTestPath(t, started)
	ws.save(t, "service.go", bodyB)
	if err := ws.refreshPaths([]string{filepath.Join(ws.root, "service.go")}); err != nil {
		t.Fatal(err)
	}
	confirmedB := make(chan error, 1)
	go func() {
		confirmedB <- svc.ConfirmPath(context.Background(), ws.root, "service.go", codeindex.FileHash([]byte(bodyB)))
	}()
	bFinished := false
	select {
	case err := <-confirmedB:
		if err != nil {
			t.Fatal(err)
		}
		bFinished = true
	case <-time.After(100 * time.Millisecond):
	}
	if err := os.WriteFile(release, []byte("continue"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := <-confirmedA; err != nil {
		t.Fatal(err)
	}
	if !bFinished {
		if err := <-confirmedB; err != nil {
			t.Fatal(err)
		}
	}
	live, err := ws.store.SearchLiveText("PromotedHandler", 5)
	if err != nil || len(live) != 0 {
		t.Fatalf("live after B confirmation=%+v err=%v", live, err)
	}
	durable, err := ws.store.SearchSymbolsText("PromotedHandler", 5)
	if err != nil || len(durable) != 1 {
		t.Fatalf("durable=%+v err=%v", durable, err)
	}
	sym, err := ws.store.GetSymbol(durable[0].Key)
	if err != nil || !strings.Contains(sym.Body, "version B") {
		t.Fatalf("promoted symbol=%+v err=%v", sym, err)
	}
	search, err := svc.Search(context.Background(), searchRequest{Root: ws.root, Query: "PromotedHandler"})
	if err != nil || len(search.Hits) != 1 || search.Hits[0].Live {
		t.Fatalf("search after promotion=%+v err=%v", search, err)
	}
}

func TestConfirmPathDeletionRemovesDurableSymbolsAndTombstone(t *testing.T) {
	svc, ws, _ := newTestCodeService(t)
	sym := serviceSymbol("service.go", "DeletedHandler", "func DeletedHandler() {}")
	putServiceSymbol(t, ws.store, sym, "deleted-hash")
	if err := ws.store.PutLiveDeletion("service.go", "deleted-hash", 1); err != nil {
		t.Fatal(err)
	}

	if err := svc.ConfirmPath(context.Background(), ws.root, "service.go", "deleted-hash"); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.store.GetSymbol(sym.Key); !errors.Is(err, codestore.ErrNotFound) {
		t.Fatalf("GetSymbol after deletion err=%v", err)
	}
	states, err := ws.store.ListFileStates()
	if err != nil || len(states) != 0 {
		t.Fatalf("states=%+v err=%v", states, err)
	}
}

func TestConfirmPathMutationWaitsForActiveSnapshot(t *testing.T) {
	svc, ws, _ := newTestCodeService(t)
	started, release := useBlockingDocumentSymbolServer(t, svc, ws.root)
	body := "package service\n\nfunc PromotedHandler() {\n\t// current\n}"
	hash := codeindex.FileHash([]byte(body))
	if err := ws.store.PutLiveFile("service.go", hash, body, 1); err != nil {
		t.Fatal(err)
	}

	ws.updateMu.Lock()
	locked := true
	defer func() {
		if locked {
			ws.updateMu.Unlock()
		}
	}()
	confirmed := make(chan error, 1)
	go func() { confirmed <- svc.ConfirmPath(context.Background(), ws.root, "service.go", hash) }()
	waitForTestPath(t, started)
	if err := os.WriteFile(release, []byte("continue"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-confirmed:
		t.Fatalf("confirmation mutated store during active snapshot: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	ws.updateMu.Unlock()
	locked = false
	if err := <-confirmed; err != nil {
		t.Fatal(err)
	}
}

func TestCodeServiceSearchSchedulesConfirmationWithoutBlockingBarrier(t *testing.T) {
	svc, ws, _ := newTestCodeService(t)
	started, release := useBlockingDocumentSymbolServer(t, svc, ws.root)
	body := "package service\n\nfunc PromotedHandler() {\n\t// current\n}"
	ws.save(t, "service.go", body)

	resp, err := svc.Search(context.Background(), searchRequest{Root: ws.root, Query: "PromotedHandler"})
	if err != nil || len(resp.Hits) != 1 || !resp.Hits[0].Live {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	waitForTestPath(t, started)
	if err := os.WriteFile(release, []byte("continue"), 0o644); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		hits, err := ws.store.SearchSymbolsText("PromotedHandler", 1)
		if err != nil {
			t.Fatal(err)
		}
		live, err := ws.store.SearchLiveText("PromotedHandler", 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) == 1 && len(live) == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("scheduled confirmation did not promote live file")
}

func TestCodeServiceFailedConfirmationRetriesOnNextRead(t *testing.T) {
	svc, ws, _ := newTestCodeService(t)
	exe := buildFakeLSPServerFromSrc(t, t.TempDir(), "fakelsp-confirm-retry", fakeLSPServerDocumentSymbolSrc)
	_ = svc.lsps.Close()
	var attempts atomic.Int32
	svc.lsps = newLSPPool(time.Hour, func(ctx context.Context, root, _ string) (*pooledLanguageServer, error) {
		if attempts.Add(1) == 1 {
			return nil, errors.New("transient LSP start failure")
		}
		client, _, err := startLanguageServer(exe, root)
		if err != nil {
			return nil, err
		}
		return &pooledLanguageServer{client: client, close: client.Close}, nil
	})
	ws.save(t, "service.go", "package service\nfunc Foo() {\n}\n")

	first, err := svc.Search(context.Background(), searchRequest{Root: ws.root, Query: "Foo"})
	if err != nil || len(first.Hits) != 1 || !first.Hits[0].Live {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	deadline := time.Now().Add(time.Second)
	for attempts.Load() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	if _, err := svc.Search(context.Background(), searchRequest{Root: ws.root, Query: "Foo"}); err != nil {
		t.Fatal(err)
	}

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		durable, err := ws.store.SearchSymbolsText("Foo", 1)
		if err != nil {
			t.Fatal(err)
		}
		live, err := ws.store.SearchLiveText("Foo", 1)
		if err != nil {
			t.Fatal(err)
		}
		if attempts.Load() == 2 && len(durable) == 1 && len(live) == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("failed confirmation was not retried: attempts=%d", attempts.Load())
}

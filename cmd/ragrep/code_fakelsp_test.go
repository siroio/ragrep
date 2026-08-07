package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/siroio/ragrep/internal/codeindex"
	"github.com/siroio/ragrep/internal/codestore"
)

type cancelAfterErrCalls struct {
	context.Context
	after int32
	calls atomic.Int32
	done  chan struct{}
	once  sync.Once
}

func newCancelAfterErrCalls(after int32) *cancelAfterErrCalls {
	return &cancelAfterErrCalls{Context: context.Background(), after: after, done: make(chan struct{})}
}

func (c *cancelAfterErrCalls) Done() <-chan struct{} { return c.done }

func (c *cancelAfterErrCalls) Err() error {
	if c.calls.Add(1) >= c.after {
		c.once.Do(func() { close(c.done) })
		return context.Canceled
	}
	return nil
}

// fakeLSPServerSrc is a standalone (no internal/lsp, no internal/lsp/testdata
// -- both are off-limits to modify for this task, and testdata's fake server
// always advertises documentSymbolProvider:true) minimal language server: it
// answers "initialize" with capabilities that deliberately omit
// documentSymbolProvider, and otherwise just drains stdin until killed. It's
// compiled into a real executable per test run (see buildFakeLSPServer) so it
// can be registered as a plain `servers` command string like any other
// language server -- config.Servers has no slot for extra args/env, so a
// re-exec-self-as-subtest trick (as internal/lsp's own tests use) doesn't fit
// here.
const fakeLSPServerSrc = `package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/textproto"
	"os"
	"strconv"
)

func main() {
	tp := textproto.NewReader(bufio.NewReader(os.Stdin))
	for {
		hdr, err := tp.ReadMIMEHeader()
		if err != nil {
			return
		}
		n, err := strconv.Atoi(hdr.Get("Content-Length"))
		if err != nil {
			return
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(tp.R, body); err != nil {
			return
		}
		var msg map[string]json.RawMessage
		if err := json.Unmarshal(body, &msg); err != nil {
			return
		}
		idRaw, hasID := msg["id"]
		var method string
		if m, ok := msg["method"]; ok {
			json.Unmarshal(m, &method)
		}
		if method == "initialize" && hasID {
			resp := fmt.Sprintf("{\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"capabilities\":{\"definitionProvider\":true}}}", string(idRaw))
			fmt.Fprintf(os.Stdout, "Content-Length: %d\r\n\r\n%s", len(resp), resp)
		}
		// "initialized" (a notification) and anything else: no reply is
		// expected or needed for this test's scenario.
	}
}
`

// buildFakeLSPServer compiles fakeLSPServerSrc into a standalone executable
// in dir and returns its path. Skips the test if the "go" toolchain isn't on
// PATH (it always is wherever `go test` itself runs, but this keeps the
// dependency explicit and the test skippable rather than failing somewhere
// unusual).
func buildFakeLSPServer(t *testing.T, dir string) string {
	t.Helper()
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not on PATH; skipping fake-LSP-server test")
	}

	src := filepath.Join(dir, "fakelsp.go")
	if err := os.WriteFile(src, []byte(fakeLSPServerSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	exePath := filepath.Join(dir, "fakelsp.exe")
	cmd := exec.Command(goBin, "build", "-o", exePath, src)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("building fake LSP server: %v: %s", err, stderr.String())
	}
	return exePath
}

// code index must fail clearly, and index nothing, when the configured
// language server doesn't advertise textDocument/documentSymbol support --
// verified against a real (if minimal) LSP-speaking subprocess, not a mock
// of internal/lsp's Client.
func TestCmdCodeIndexCapabilityGate(t *testing.T) {
	root, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	exePath := buildFakeLSPServer(t, root)

	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ragrepDir := filepath.Join(root, ".ragrep")
	if err := os.MkdirAll(ragrepDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgJSON, err := json.Marshal(map[string]any{"servers": map[string]string{"go": exePath}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ragrepDir, "config.json"), cfgJSON, 0o644); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(ragrepDir, "code.db")
	injectCodeServiceDaemon(t, root, filepath.Join(t.TempDir(), "code.db"))

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	code := run([]string{"code", "index", "--db", db, "--language", "go", root})
	w.Close()
	os.Stderr = old
	var buf bytes.Buffer
	buf.ReadFrom(r)

	if code != 1 {
		t.Fatalf("capability gate: exit=%d, want 1, stderr=%q", code, buf.String())
	}
	if !strings.Contains(buf.String(), "documentSymbol") {
		t.Fatalf("stderr=%q, want a clear message naming the missing documentSymbol capability", buf.String())
	}

	// The capability check happens before the codestore is ever opened, so
	// nothing should have been indexed -- not even an empty code.db file.
	if _, err := os.Stat(db); !os.IsNotExist(err) {
		t.Fatalf("code.db must not be created when the capability gate fails, stat err=%v", err)
	}
}

func TestCodeServiceIndexPreservesIndexingSemantics(t *testing.T) {
	svc, ws, embedder := newTestCodeService(t)
	src := strings.Replace(fakeLSPServerDocumentSymbolSrc,
		`{"capabilities":{"documentSymbolProvider":true}}`,
		`{"capabilities":{"documentSymbolProvider":true},"serverInfo":{"name":"fake-index","version":"v1"}}`, 1)
	exe := buildFakeLSPServerFromSrc(t, t.TempDir(), "fakelsp-service-index", src)
	writeRagrepConfig(t, ws.root, `{"servers":{"go":"`+filepath.ToSlash(exe)+`"}}`)

	body := "package p\n\nfunc Foo() {\n\n}\n"
	ws.save(t, "a.go", body)
	ws.save(t, "b.go", body)
	req := indexRequest{Root: ws.root, Language: "go", Roots: []string{"."}}

	first, err := svc.Index(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first.Indexed, []string{"a.go", "b.go"}) || first.Scanned != 2 || len(first.Pruned) != 0 {
		t.Fatalf("first result=%+v", first)
	}
	if embedder.calls.Load() != 2 {
		t.Fatalf("first embed calls=%d, want 2", embedder.calls.Load())
	}
	run, err := ws.store.LatestIndexRun()
	if err != nil || run.Scope != "index:." || run.ServerName != "fake-index" || run.ServerVersion != "v1" {
		t.Fatalf("latest run=%+v err=%v", run, err)
	}

	second, err := svc.Index(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Indexed) != 0 || second.Scanned != 2 || len(second.Pruned) != 0 || embedder.calls.Load() != 2 {
		t.Fatalf("unchanged result=%+v embed calls=%d", second, embedder.calls.Load())
	}

	if err := os.Remove(filepath.Join(ws.root, "b.go")); err != nil {
		t.Fatal(err)
	}
	third, err := svc.Index(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(third.Pruned, []string{"b.go"}) || third.Scanned != 1 {
		t.Fatalf("delete result=%+v", third)
	}
	paths, err := ws.store.ListPaths()
	if err != nil || !reflect.DeepEqual(paths, []string{"a.go"}) {
		t.Fatalf("paths=%v err=%v", paths, err)
	}
}

func TestCodeServiceIndexCanceledBeforeStartDoesNotMutate(t *testing.T) {
	svc, ws, _ := newTestCodeService(t)
	putServiceSymbol(t, ws.store, serviceSymbol("stale.go", "Stale", "func Stale() {}"), "stale-hash")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := svc.Index(ctx, indexRequest{Root: ws.root, Language: "go", Roots: []string{"."}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Index error=%v, want context.Canceled", err)
	}
	if _, err := ws.store.LatestIndexRun(); !errors.Is(err, codestore.ErrNotFound) {
		t.Fatalf("LatestIndexRun error=%v, want ErrNotFound", err)
	}
	paths, err := ws.store.ListPaths()
	if err != nil || !reflect.DeepEqual(paths, []string{"stale.go"}) {
		t.Fatalf("paths=%v err=%v", paths, err)
	}
}

func TestCodeServiceIndexCanceledDuringDiscoveryDoesNotMutate(t *testing.T) {
	svc, ws, _ := newTestCodeService(t)
	putServiceSymbol(t, ws.store, serviceSymbol("stale.go", "Stale", "func Stale() {}"), "stale-hash")
	ws.save(t, "a.go", "package p\n")
	ws.save(t, "b.go", "package p\n")
	writeRagrepConfig(t, ws.root, `{"servers":{"go":"server-must-not-start"}}`)

	_, err := svc.Index(newCancelAfterErrCalls(4), indexRequest{Root: ws.root, Language: "go", Roots: []string{"."}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Index error=%v, want context.Canceled", err)
	}
	if _, err := ws.store.LatestIndexRun(); !errors.Is(err, codestore.ErrNotFound) {
		t.Fatalf("LatestIndexRun error=%v, want ErrNotFound", err)
	}
	paths, err := ws.store.ListPaths()
	if err != nil || !reflect.DeepEqual(paths, []string{"stale.go"}) {
		t.Fatalf("paths=%v err=%v", paths, err)
	}
}

func TestCodeServiceIndexCanceledBeforeIndexRunDoesNotMutate(t *testing.T) {
	svc, ws, _ := newTestCodeService(t)
	ws.save(t, "a.go", "package p\n\nfunc Foo() {}\n")
	exe := buildFakeLSPServerFromSrc(t, t.TempDir(), "fakelsp-cancel-run", fakeLSPServerDocumentSymbolSrc)
	writeRagrepConfig(t, ws.root, `{"servers":{"go":"`+filepath.ToSlash(exe)+`"}}`)

	_, err := svc.Index(newCancelAfterErrCalls(5), indexRequest{Root: ws.root, Language: "go", Roots: []string{"."}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Index error=%v, want context.Canceled", err)
	}
	if _, err := ws.store.LatestIndexRun(); !errors.Is(err, codestore.ErrNotFound) {
		t.Fatalf("LatestIndexRun error=%v, want ErrNotFound", err)
	}
	paths, err := ws.store.ListPaths()
	if err != nil || len(paths) != 0 {
		t.Fatalf("paths=%v err=%v, want no upsert", paths, err)
	}
}

func TestCodeServiceIndexCanceledBeforeUpsertDoesNotMutateFile(t *testing.T) {
	svc, ws, _ := newTestCodeService(t)
	putServiceSymbol(t, ws.store, serviceSymbol("stale.go", "Stale", "func Stale() {}"), "stale-hash")
	ws.save(t, "a.go", "package p\n\nfunc Foo() {\n\n}\n")
	exe := buildFakeLSPServerFromSrc(t, t.TempDir(), "fakelsp-cancel-upsert", fakeLSPServerDocumentSymbolSrc)
	writeRagrepConfig(t, ws.root, `{"servers":{"go":"`+filepath.ToSlash(exe)+`"}}`)

	_, err := svc.Index(newCancelAfterErrCalls(8), indexRequest{Root: ws.root, Language: "go", Roots: []string{"."}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Index error=%v, want context.Canceled", err)
	}
	paths, err := ws.store.ListPaths()
	if err != nil || !reflect.DeepEqual(paths, []string{"stale.go"}) {
		t.Fatalf("paths=%v err=%v, want no upsert or prune", paths, err)
	}
}

func TestCodeServiceIndexCanceledBeforeDeleteDoesNotPrune(t *testing.T) {
	svc, ws, _ := newTestCodeService(t)
	putServiceSymbol(t, ws.store, serviceSymbol("stale.go", "Stale", "func Stale() {}"), "stale-hash")
	writeRagrepConfig(t, ws.root, `{"servers":{"go":"server-must-not-start"}}`)

	_, err := svc.Index(newCancelAfterErrCalls(7), indexRequest{Root: ws.root, Language: "go", Roots: []string{"."}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Index error=%v, want context.Canceled", err)
	}
	paths, err := ws.store.ListPaths()
	if err != nil || !reflect.DeepEqual(paths, []string{"stale.go"}) {
		t.Fatalf("paths=%v err=%v, want no prune", paths, err)
	}
}

func TestCodeServiceIndexLSPErrorDoesNotPrune(t *testing.T) {
	svc, ws, _ := newTestCodeService(t)
	putServiceSymbol(t, ws.store, serviceSymbol("stale.go", "Stale", "func Stale() {}"), "stale-hash")
	ws.save(t, "a.go", "package p\n\nfunc Foo() {}\n")
	src := strings.Replace(fakeLSPServerDocumentSymbolSrc,
		`case "textDocument/documentSymbol":`,
		`case "textDocument/documentSymbol":
			return`, 1)
	exe := buildFakeLSPServerFromSrc(t, t.TempDir(), "fakelsp-index-error", src)
	writeRagrepConfig(t, ws.root, `{"servers":{"go":"`+filepath.ToSlash(exe)+`"}}`)

	if _, err := svc.Index(context.Background(), indexRequest{Root: ws.root, Language: "go", Roots: []string{"."}}); err == nil {
		t.Fatal("Index error=nil, want LSP failure")
	}
	paths, err := ws.store.ListPaths()
	if err != nil || !reflect.DeepEqual(paths, []string{"stale.go"}) {
		t.Fatalf("paths=%v err=%v, want no upsert or prune", paths, err)
	}
}

func TestCodeServiceIndexRemovesMatchingLiveOverlayUnderUpdateLock(t *testing.T) {
	svc, ws, _ := newTestCodeService(t)
	started, release := useBlockingDocumentSymbolServer(t, svc, ws.root)
	writeRagrepConfig(t, ws.root, `{"servers":{"go":"injected-test-server"}}`)
	body := "package service\n\nfunc PromotedHandler() {\n\t// indexed\n}"
	hash := ws.save(t, "service.go", body)
	if err := ws.store.PutLiveFile("service.go", hash, body, 1); err != nil {
		t.Fatal(err)
	}
	ws.setGeneration(1)

	indexed := make(chan error, 1)
	go func() {
		_, err := svc.Index(context.Background(), indexRequest{Root: ws.root, Language: "go", Roots: []string{"."}})
		indexed <- err
	}()
	waitForTestPath(t, started)
	ws.updateMu.Lock()
	locked := true
	defer func() {
		if locked {
			ws.updateMu.Unlock()
		}
	}()
	if err := os.WriteFile(release, []byte("continue"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-indexed:
		t.Fatalf("Index mutated store during active snapshot: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	ws.updateMu.Unlock()
	locked = false
	if err := <-indexed; err != nil {
		t.Fatal(err)
	}
	states, err := ws.store.ListFileStates()
	if err != nil || len(states) != 0 {
		t.Fatalf("live states after matching Index=%+v err=%v", states, err)
	}
}

func TestCodeServiceIndexCanceledWhileWaitingForUpdateLockDoesNotMutate(t *testing.T) {
	svc, ws, _ := newTestCodeService(t)
	started, release := useBlockingDocumentSymbolServer(t, svc, ws.root)
	writeRagrepConfig(t, ws.root, `{"servers":{"go":"injected-test-server"}}`)
	body := "package service\n\nfunc PromotedHandler() {\n\t// indexed\n}"
	ws.save(t, "service.go", body)

	ws.updateMu.Lock()
	locked := true
	defer func() {
		if locked {
			ws.updateMu.Unlock()
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	indexed := make(chan error, 1)
	go func() {
		_, err := svc.Index(ctx, indexRequest{Root: ws.root, Language: "go", Roots: []string{"."}})
		indexed <- err
	}()
	waitForTestPath(t, started)
	if err := os.WriteFile(release, []byte("continue"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-indexed:
		t.Fatalf("Index finished before update lock was released: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()

	select {
	case err := <-indexed:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Index error=%v, want context.Canceled", err)
		}
	case <-time.After(200 * time.Millisecond):
		ws.updateMu.Unlock()
		locked = false
		err := <-indexed
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Index error after forced unlock=%v, want context.Canceled", err)
		}
		t.Fatal("Index ignored cancellation while waiting for update lock")
	}
	if _, err := ws.store.LatestIndexRun(); !errors.Is(err, codestore.ErrNotFound) {
		t.Fatalf("LatestIndexRun error=%v, want ErrNotFound", err)
	}
	paths, err := ws.store.ListPaths()
	if err != nil || len(paths) != 0 {
		t.Fatalf("paths=%v err=%v, want no upsert or delete", paths, err)
	}
}

func TestCodeServiceIndexDoesNotPromoteSameHashTombstone(t *testing.T) {
	svc, ws, _ := newTestCodeService(t)
	started, release := useBlockingDocumentSymbolServer(t, svc, ws.root)
	writeRagrepConfig(t, ws.root, `{"servers":{"go":"injected-test-server"}}`)
	body := "package service\n\nfunc PromotedHandler() {\n\t// deleted while preparing\n}"
	hash := ws.save(t, "service.go", body)
	if err := ws.store.PutLiveFile("service.go", hash, body, 1); err != nil {
		t.Fatal(err)
	}

	indexed := make(chan error, 1)
	go func() {
		_, err := svc.Index(context.Background(), indexRequest{Root: ws.root, Language: "go", Roots: []string{"."}})
		indexed <- err
	}()
	waitForTestPath(t, started)
	if err := ws.store.PutLiveDeletion("service.go", hash, 2); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(release, []byte("continue"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := <-indexed; err != nil {
		t.Fatal(err)
	}

	states, err := ws.store.ListFileStates()
	if err != nil || len(states) != 1 || !states[0].Deleted || states[0].Hash != hash {
		t.Fatalf("live states=%+v err=%v, want same-hash tombstone", states, err)
	}
	durable, err := ws.store.SearchSymbolsText("PromotedHandler", 5)
	if err != nil || len(durable) != 0 {
		t.Fatalf("durable=%+v err=%v, want no resurrection", durable, err)
	}
}

func TestCodeServiceIndexKeepsNewerLiveOverlay(t *testing.T) {
	svc, ws, _ := newTestCodeService(t)
	started, release := useBlockingDocumentSymbolServer(t, svc, ws.root)
	writeRagrepConfig(t, ws.root, `{"servers":{"go":"injected-test-server"}}`)
	oldBody := "package service\n\nfunc PromotedHandler() {\n\t// old\n}"
	oldHash := ws.save(t, "service.go", oldBody)
	if err := ws.store.PutLiveFile("service.go", oldHash, oldBody, 1); err != nil {
		t.Fatal(err)
	}
	ws.setGeneration(1)

	indexed := make(chan error, 1)
	go func() {
		_, err := svc.Index(context.Background(), indexRequest{Root: ws.root, Language: "go", Roots: []string{"."}})
		indexed <- err
	}()
	waitForTestPath(t, started)
	newBody := "package service\n\nfunc PromotedHandler() {\n\t// newer\n}"
	newHash := ws.save(t, "service.go", newBody)
	if err := ws.refreshPaths([]string{filepath.Join(ws.root, "service.go")}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(release, []byte("continue"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := <-indexed; err != nil {
		t.Fatal(err)
	}
	file, err := ws.store.GetLiveFileByPath("service.go", newHash)
	if err != nil || file.Body != newBody {
		t.Fatalf("newer live file=%+v err=%v", file, err)
	}
}

func TestCodeServiceExpandPreservesRelationSemantics(t *testing.T) {
	svc, ws, _ := newTestCodeService(t)
	body := "package main\n\nfunc Callee() {}\n\nfunc Caller() {\n\tCallee()\n\tCallee()\n}\n"
	ws.save(t, "main.go", body)
	target := serviceSymbol("main.go", "Callee", "func Callee() {}")
	target.Key = "target-key"
	target.Range = codeindex.Range{Start: codeindex.Position{Line: 2}, End: codeindex.Position{Line: 2, Character: 16}}
	caller := serviceSymbol("main.go", "Caller", "func Caller() {\n\tCallee()\n\tCallee()\n}")
	caller.Key = "caller-key"
	caller.Range = codeindex.Range{Start: codeindex.Position{Line: 4}, End: codeindex.Position{Line: 7, Character: 1}}
	if _, err := ws.store.UpsertSymbols("main.go", codeindex.FileHash([]byte(body)), []codeindex.Symbol{target, caller}, 0, fakeCodeEmbed); err != nil {
		t.Fatal(err)
	}
	indexRun, err := ws.store.RecordIndexRun("index:.", "index-rev", "go", "fake-index", "v0", codeModelID, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	locations := "[" + strings.Join([]string{
		lspLocationJSON(filepath.Join(ws.root, "main.go"), 5),
		lspLocationJSON(filepath.Join(ws.root, "main.go"), 6),
		lspLocationJSON(filepath.Join(ws.root, "missing.go"), 0),
	}, ",") + "]"
	src := strings.Replace(fakeLSPServerReferencesSrcTemplate,
		`{"capabilities":{"referencesProvider":true}}`,
		`{"capabilities":{"referencesProvider":true},"serverInfo":{"name":"fake-expand","version":"v2"}}`, 1)
	src = strings.ReplaceAll(src, "LOCATIONS_JSON_PLACEHOLDER", locations)
	exe := buildFakeLSPServerFromSrc(t, t.TempDir(), "fakelsp-service-expand", src)
	writeRagrepConfig(t, ws.root, `{"servers":{"go":"`+filepath.ToSlash(exe)+`"}}`)

	targets, err := svc.Expand(context.Background(), expandRequest{Root: ws.root, Key: target.Key, Relation: "references"})
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 3 || !targets[0].Resolved || targets[0].Key != caller.Key || !targets[1].Resolved || targets[2].Resolved || targets[2].Path != "missing.go" {
		t.Fatalf("targets=%+v", targets)
	}
	relations, err := ws.store.RelationsFrom(target.Key)
	if err != nil || len(relations) != 1 || relations[0].ToKey != caller.Key || relations[0].Source != "fake-expand" {
		t.Fatalf("relations=%+v err=%v", relations, err)
	}
	after, err := ws.store.LatestIndexRun()
	if err != nil || after.ID != indexRun {
		t.Fatalf("latest index run=%+v err=%v, want id %d", after, err, indexRun)
	}
}

func TestCodeServiceExpandRejectsDurableKeyAfterSave(t *testing.T) {
	svc, ws, _ := newTestCodeService(t)
	body := "package service\nfunc DurableBeforeExpand() {}"
	sym := serviceSymbol("service.go", "DurableBeforeExpand", "func DurableBeforeExpand() {}")
	putServiceSymbol(t, ws.store, sym, ws.save(t, "service.go", body))
	exe := buildFakeLSPServerFromSrc(t, t.TempDir(), "fakelsp-stale-expand", fakeLSPServerDocumentSymbolSrc)
	writeRagrepConfig(t, ws.root, `{"servers":{"go":"`+filepath.ToSlash(exe)+`"}}`)

	search, err := svc.Search(context.Background(), searchRequest{Root: ws.root, Query: sym.Name})
	if err != nil || len(search.Hits) != 1 || search.Hits[0].Key != sym.Key {
		t.Fatalf("search=%+v err=%v", search, err)
	}
	ws.save(t, "service.go", "package service\nfunc ChangedBeforeExpand() {}")

	if _, err := svc.Expand(context.Background(), expandRequest{Root: ws.root, Key: sym.Key, Relation: "references"}); !errors.Is(err, ErrStaleLiveKey) {
		t.Fatalf("Expand durable key after save err=%v, want stale_live_key", err)
	}
}

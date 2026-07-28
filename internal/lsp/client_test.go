package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/textproto"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/siroio/ragrep/internal/lsp/testdata"
)

// TestMain lets this test binary re-exec itself as a fake language server
// (see TestHelperProcess below) — the same os/exec_test.go
// GO_WANT_HELPER_PROCESS pattern the stdlib uses for testing subprocess
// behavior. This keeps every test in this file independent of gopls (or
// any real language server) being installed.
func TestMain(m *testing.M) {
	os.Exit(m.Run())
}

// TestHelperProcess is not a real test. When re-exec'd with
// GO_WANT_HELPER_PROCESS=1 (see startFakeServer), it becomes the fake
// language server process instead of running as part of the test suite.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	err := testdata.Run(os.Stdin, os.Stdout)
	if err != nil {
		os.Stderr.WriteString(err.Error())
		os.Exit(1)
	}
	os.Exit(0)
}

// startFakeServer starts this test binary as a subprocess running the fake
// server from internal/lsp/testdata.
func startFakeServer(t *testing.T) *Client {
	t.Helper()
	c, err := Start(os.Args[0], []string{"-test.run=^TestHelperProcess$"},
		WithEnv(append(os.Environ(), "GO_WANT_HELPER_PROCESS=1")))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// mustInitialize drives initialize+initialized with the given fake-server
// scenario options and fails the test on error.
func mustInitialize(t *testing.T, c *Client, opts testdata.Options) *InitializeResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := c.Initialize(ctx, InitializeParams{
		Capabilities:          DefaultClientCapabilities(),
		InitializationOptions: opts,
	})
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := c.Initialized(); err != nil {
		t.Fatalf("Initialized: %v", err)
	}
	return res
}

func TestLifecycleAndRequestRoundTrip(t *testing.T) {
	c := startFakeServer(t)

	res := mustInitialize(t, c, testdata.Options{})
	if !c.Supports(FeatureDocumentSymbol) || !c.Supports(FeatureDefinition) ||
		!c.Supports(FeatureReferences) || !c.Supports(FeatureCallHierarchy) {
		t.Fatalf("expected all features supported, got capabilities=%+v", res.Capabilities)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	doc := DocumentSymbolParams{TextDocument: TextDocumentIdentifier{URI: "file:///fake/foo.go"}}
	symbols, err := c.DocumentSymbol(ctx, doc)
	if err != nil {
		t.Fatalf("DocumentSymbol: %v", err)
	}
	if len(symbols) != 1 || symbols[0].Name != "Foo" || symbols[0].Kind != 12 {
		t.Fatalf("DocumentSymbol result = %+v, want one symbol named Foo kind 12", symbols)
	}
	if len(symbols[0].Children) != 0 {
		t.Fatalf("DocumentSymbol children = %+v, want empty", symbols[0].Children)
	}

	defPos := TextDocumentPositionParams{TextDocument: TextDocumentIdentifier{URI: "file:///fake/foo.go"}, Position: Position{Line: 1, Character: 5}}
	defs, err := c.Definition(ctx, defPos)
	if err != nil {
		t.Fatalf("Definition: %v", err)
	}
	if len(defs) != 1 || defs[0].URI != "file:///fake/bar.go" || defs[0].Range.Start.Line != 10 {
		t.Fatalf("Definition result = %+v", defs)
	}

	refs, err := c.References(ctx, ReferenceParams{TextDocumentPositionParams: defPos, Context: ReferenceContext{IncludeDeclaration: true}})
	if err != nil {
		t.Fatalf("References: %v", err)
	}
	if len(refs) != 2 || refs[1].URI != "file:///fake/baz.go" {
		t.Fatalf("References result = %+v", refs)
	}

	items, err := c.PrepareCallHierarchy(ctx, defPos)
	if err != nil {
		t.Fatalf("PrepareCallHierarchy: %v", err)
	}
	if len(items) != 1 || items[0].Name != "Foo" {
		t.Fatalf("PrepareCallHierarchy result = %+v", items)
	}

	inCalls, err := c.IncomingCalls(ctx, CallHierarchyIncomingCallsParams{Item: items[0]})
	if err != nil {
		t.Fatalf("IncomingCalls: %v", err)
	}
	if len(inCalls) != 1 || inCalls[0].From.Name != "Caller" || len(inCalls[0].FromRanges) != 1 {
		t.Fatalf("IncomingCalls result = %+v", inCalls)
	}

	outCalls, err := c.OutgoingCalls(ctx, CallHierarchyOutgoingCallsParams{Item: items[0]})
	if err != nil {
		t.Fatalf("OutgoingCalls: %v", err)
	}
	if len(outCalls) != 1 || outCalls[0].To.Name != "Callee" {
		t.Fatalf("OutgoingCalls result = %+v", outCalls)
	}

	if err := c.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := c.Exit(); err != nil {
		t.Fatalf("Exit: %v", err)
	}
	if err := c.Wait(ctx); err != nil {
		t.Fatalf("Wait after graceful exit: %v", err)
	}
}

// TestCapabilitiesGateUnsupportedMethods checks that a server which
// declines a capability at initialize time is recorded as such, so a
// caller can skip invoking it rather than calling blind.
func TestCapabilitiesGateUnsupportedMethods(t *testing.T) {
	c := startFakeServer(t)
	mustInitialize(t, c, testdata.Options{DisableCallHierarchy: true})

	if c.Supports(FeatureCallHierarchy) {
		t.Fatal("expected callHierarchy unsupported")
	}
	if !c.Supports(FeatureDocumentSymbol) {
		t.Fatal("expected documentSymbol still supported")
	}
	if c.Supports("madeUpFeature") {
		t.Fatal("unrecognized feature name should report unsupported, not panic or default true")
	}

	// A well-behaved caller checks Supports before calling; demonstrate
	// that here rather than invoking prepareCallHierarchy against a server
	// that just told us it doesn't implement it.
	if c.Supports(FeatureCallHierarchy) {
		t.Fatal("should not reach a call to an unsupported feature")
	}
}

// TestRequestTimeout verifies a context deadline shorter than the server's
// response time surfaces as a prompt error, not a hang, and that the
// pending entry is cleaned up (checked indirectly: a later call on the
// same client still works normally).
func TestRequestTimeout(t *testing.T) {
	c := startFakeServer(t)
	mustInitialize(t, c, testdata.Options{
		DelayOnMethod: "textDocument/documentSymbol",
		DelayMillis:   2000,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := c.DocumentSymbol(ctx, DocumentSymbolParams{TextDocument: TextDocumentIdentifier{URI: "file:///fake/foo.go"}})
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("DocumentSymbol err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("DocumentSymbol took %v, expected to return promptly at the ~100ms deadline", elapsed)
	}

	// The client itself must still be usable after a timed-out call.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	if err := c.Shutdown(ctx2); err != nil {
		t.Fatalf("Shutdown after a prior timeout: %v", err)
	}
}

// TestJSONRPCErrorSurfaced verifies a JSON-RPC error response comes back
// as a Go error the caller can inspect via errors.As.
func TestJSONRPCErrorSurfaced(t *testing.T) {
	c := startFakeServer(t)
	mustInitialize(t, c, testdata.Options{ErrorOnMethod: "textDocument/definition"})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := c.Definition(ctx, TextDocumentPositionParams{TextDocument: TextDocumentIdentifier{URI: "file:///fake/foo.go"}})
	if err == nil {
		t.Fatal("expected an error")
	}
	var rpcErr *ResponseError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("err = %v (%T), want *ResponseError", err, err)
	}
	if rpcErr.Code != -32000 {
		t.Fatalf("rpcErr.Code = %d, want -32000", rpcErr.Code)
	}
}

// TestServerProcessDeath verifies that when the server process dies
// mid-request, the in-flight call fails promptly (rather than hanging
// forever waiting for a response that will never come), and that the
// client stays usable enough to report errors on subsequent calls too.
func TestServerProcessDeath(t *testing.T) {
	c := startFakeServer(t)
	mustInitialize(t, c, testdata.Options{CrashOnMethod: "textDocument/references"})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := c.References(ctx, ReferenceParams{
			TextDocumentPositionParams: TextDocumentPositionParams{TextDocument: TextDocumentIdentifier{URI: "file:///fake/foo.go"}},
		})
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error when the server process dies mid-request")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("References call hung after the server process died")
	}

	// A second call on the now-dead client must also fail fast, not hang.
	done2 := make(chan error, 1)
	go func() {
		_, err := c.DocumentSymbol(ctx, DocumentSymbolParams{TextDocument: TextDocumentIdentifier{URI: "file:///fake/foo.go"}})
		done2 <- err
	}()
	select {
	case err := <-done2:
		if err == nil {
			t.Fatal("expected an error calling a dead client")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("DocumentSymbol call hung on an already-dead client")
	}
}

// TestCloseTerminatesProcess exercises Windows process termination: Close
// kills the subprocess without a graceful shutdown/exit handshake, Wait
// completes (proving the OS process was reaped), and further calls fail
// fast rather than hang.
func TestCloseTerminatesProcess(t *testing.T) {
	c := startFakeServer(t)
	mustInitialize(t, c, testdata.Options{})

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if c.cmd.ProcessState == nil {
		t.Fatal("expected cmd.ProcessState to be set after Close (process should have been waited on)")
	}

	// Idempotent: calling Close again must not hang or panic.
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := c.DocumentSymbol(context.Background(), DocumentSymbolParams{})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error calling a closed client")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("call hung after Close")
	}
}

// TestWaitCancelsAndKillsOnContextDone exercises context cancellation on
// Windows: a caller waiting for graceful exit that instead cancels its
// context must get ctx.Err() promptly, with the process force-killed
// rather than left running.
func TestWaitCancelsAndKillsOnContextDone(t *testing.T) {
	c := startFakeServer(t)
	mustInitialize(t, c, testdata.Options{})
	// Deliberately do not call Shutdown/Exit — Wait should time out and
	// force a kill rather than block forever on a server that never exits
	// on its own.

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := c.Wait(ctx)
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Wait took %v, expected to return promptly at the ~100ms deadline", elapsed)
	}
	if c.cmd.ProcessState == nil {
		t.Fatal("expected the process to have been killed and reaped by Wait's deadline path")
	}
}

// TestServerRequestGetsMethodNotFound is a white-box test (constructing a
// Client directly over an in-memory pipe, bypassing Start/exec) that
// precisely verifies the wire-level reply to a server-to-client request:
// a JSON-RPC error with code -32601 (MethodNotFound), carrying the same
// id, so a real server like gopls doesn't stall waiting for it.
func TestServerRequestGetsMethodNotFound(t *testing.T) {
	serverToClientR, serverToClientW := io.Pipe() // server "sends" on W, client's readLoop reads R
	clientToServerR, clientToServerW := io.Pipe() // client writes its stdin to W, test reads replies on R

	c := &Client{
		cmd:     exec.Command("go"), // never started; only used by Close's cleanup path
		stdin:   clientToServerW,
		pending: make(map[int64]chan rpcResult),
		done:    make(chan struct{}),
	}
	go c.readLoop(serverToClientR)
	t.Cleanup(func() {
		serverToClientW.Close()
		clientToServerR.Close()
	})

	req := []byte(`{"jsonrpc":"2.0","id":9001,"method":"workspace/configuration","params":[{}]}`)
	go func() {
		fmt.Fprintf(serverToClientW, "Content-Length: %d\r\n\r\n%s", len(req), req)
	}()

	tp := textproto.NewReader(bufio.NewReader(clientToServerR))
	hdr, err := tp.ReadMIMEHeader()
	if err != nil {
		t.Fatalf("reading reply header: %v", err)
	}
	n, err := strconv.Atoi(hdr.Get("Content-Length"))
	if err != nil {
		t.Fatalf("bad Content-Length: %v", err)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(tp.R, body); err != nil {
		t.Fatalf("reading reply body: %v", err)
	}

	var reply struct {
		ID    int `json:"id"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &reply); err != nil {
		t.Fatalf("decoding reply: %v, body=%s", err, body)
	}
	if reply.ID != 9001 {
		t.Fatalf("reply.ID = %d, want 9001", reply.ID)
	}
	if reply.Error == nil || reply.Error.Code != ErrCodeMethodNotFound {
		t.Fatalf("reply.Error = %+v, want code %d", reply.Error, ErrCodeMethodNotFound)
	}
}

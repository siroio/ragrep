package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/textproto"
	"os/exec"
	"strconv"
	"sync"
)

// Feature names accepted by Client.Supports.
const (
	FeatureDocumentSymbol = "documentSymbol"
	FeatureDefinition     = "definition"
	FeatureReferences     = "references"
	FeatureCallHierarchy  = "callHierarchy"
)

// Option configures the server process's exec.Cmd before it starts.
type Option func(*exec.Cmd)

// WithEnv sets the child process's environment. Unlike a nil Env (which
// inherits the current process's environment), this replaces it entirely —
// pass append(os.Environ(), "FOO=bar") to extend rather than replace.
func WithEnv(env []string) Option {
	return func(cmd *exec.Cmd) { cmd.Env = env }
}

// WithDir sets the child process's working directory.
func WithDir(dir string) Option {
	return func(cmd *exec.Cmd) { cmd.Dir = dir }
}

// rpcResult is what the read loop delivers to a pending call: either a
// decoded result payload or an error (a *ResponseError from the server, or
// a transport-level failure).
type rpcResult struct {
	result json.RawMessage
	err    error
}

// Client is a minimal LSP client speaking JSON-RPC 2.0 over a language
// server subprocess's stdio. It is safe for concurrent use.
type Client struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser

	writeMu sync.Mutex // serializes writes to stdin

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan rpcResult // nil once dead
	caps    ServerCapabilities
	dead    bool
	exitErr error // reason the connection died; set together with dead

	done chan struct{} // closed once the read loop (and process wait) has finished
}

// Start launches the language server named by name with the given
// arguments and begins reading its responses in the background. The
// caller drives the initialize/initialized/.../shutdown/exit lifecycle
// explicitly and must eventually call Close.
func Start(name string, args []string, opts ...Option) (*Client, error) {
	cmd := exec.Command(name, args...)
	for _, opt := range opts {
		opt(cmd)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("lsp: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("lsp: stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("lsp: start %s: %w", name, err)
	}

	c := &Client{
		cmd:     cmd,
		stdin:   stdin,
		pending: make(map[int64]chan rpcResult),
		done:    make(chan struct{}),
	}
	go c.readLoop(stdout)
	return c, nil
}

// readLoop owns stdout until it hits EOF or a framing/decode error, then
// reaps the process and fails every pending call. It never returns early:
// the only way out is the connection dying.
func (c *Client) readLoop(stdout io.Reader) {
	readErr := c.readAll(bufio.NewReader(stdout))

	// cmd.Wait closes the pipes it created; readAll above has already read
	// stdout to EOF/error, so all reads are done and this is safe (see
	// exec.Cmd.StdoutPipe's doc comment on that ordering requirement).
	waitErr := c.cmd.Wait()

	c.mu.Lock()
	c.dead = true
	if waitErr != nil {
		c.exitErr = fmt.Errorf("lsp: server process exited: %w", waitErr)
	} else {
		c.exitErr = fmt.Errorf("lsp: server closed the connection: %w", readErr)
	}
	pending := c.pending
	c.pending = nil
	exitErr := c.exitErr
	c.mu.Unlock()

	for _, ch := range pending {
		ch <- rpcResult{err: exitErr}
	}
	close(c.done)
}

// readAll decodes Content-Length-framed JSON-RPC messages until it hits an
// error (EOF included); it always returns a non-nil error.
func (c *Client) readAll(br *bufio.Reader) error {
	tp := textproto.NewReader(br)
	for {
		hdr, err := tp.ReadMIMEHeader()
		if err != nil {
			return err
		}
		n, err := strconv.Atoi(hdr.Get("Content-Length"))
		if err != nil {
			return fmt.Errorf("lsp: bad Content-Length: %w", err)
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(tp.R, body); err != nil {
			return err
		}
		var msg wireMessage
		if err := json.Unmarshal(body, &msg); err != nil {
			return fmt.Errorf("lsp: decode message: %w", err)
		}
		c.handleMessage(&msg)
	}
}

func (c *Client) handleMessage(msg *wireMessage) {
	switch {
	case msg.ID != nil && msg.Method == "":
		// A response to one of our requests.
		var id int64
		if err := json.Unmarshal(*msg.ID, &id); err != nil {
			return // can't correlate; drop it rather than crash the loop
		}
		c.mu.Lock()
		var ch chan rpcResult
		if c.pending != nil {
			ch = c.pending[id]
			delete(c.pending, id)
		}
		c.mu.Unlock()
		if ch == nil {
			return // no longer waiting (timed out, or unknown id)
		}
		if msg.Error != nil {
			ch <- rpcResult{err: msg.Error}
		} else {
			ch <- rpcResult{result: msg.Result}
		}

	case msg.Method != "" && msg.ID != nil:
		// A request from the server. We don't implement any server-to-client
		// methods (workspace/configuration, window/workDoneProgress/create,
		// etc.) — reply with MethodNotFound so the server doesn't stall
		// waiting for a response we'll never send.
		c.replyMethodNotFound(*msg.ID)

	default:
		// A notification from the server (window/logMessage, $/progress,
		// ...). We have no use for these; ignore.
	}
}

func (c *Client) replyMethodNotFound(id json.RawMessage) {
	resp := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   *ResponseError  `json:"error"`
	}{"2.0", id, &ResponseError{Code: ErrCodeMethodNotFound, Message: "method not found"}}
	_ = c.writeMessage(resp) // best-effort: a dead connection surfaces via the read loop instead
}

func (c *Client) writeMessage(v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("lsp: marshal: %w", err)
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := fmt.Fprintf(c.stdin, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		return fmt.Errorf("lsp: write header: %w", err)
	}
	if _, err := c.stdin.Write(body); err != nil {
		return fmt.Errorf("lsp: write body: %w", err)
	}
	return nil
}

// call sends a request and blocks until a response arrives, ctx is done, or
// the connection dies. On timeout/cancellation the pending entry is
// removed so a late reply (if any) is dropped rather than leaked.
func (c *Client) call(ctx context.Context, method string, params, result any) error {
	c.mu.Lock()
	if c.dead {
		err := c.exitErr
		c.mu.Unlock()
		return err
	}
	c.nextID++
	id := c.nextID
	ch := make(chan rpcResult, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	if err := c.writeMessage(requestMessage{JSONRPC: "2.0", ID: id, Method: method, Params: params}); err != nil {
		c.mu.Lock()
		if c.pending != nil {
			delete(c.pending, id)
		}
		c.mu.Unlock()
		return err
	}

	select {
	case res := <-ch:
		if res.err != nil {
			return res.err
		}
		if result != nil && len(res.result) > 0 {
			if err := json.Unmarshal(res.result, result); err != nil {
				return fmt.Errorf("lsp: decode %s result: %w", method, err)
			}
		}
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		if c.pending != nil {
			delete(c.pending, id)
		}
		c.mu.Unlock()
		return ctx.Err()
	}
}

func (c *Client) notify(method string, params any) error {
	c.mu.Lock()
	dead, exitErr := c.dead, c.exitErr
	c.mu.Unlock()
	if dead {
		return exitErr
	}
	return c.writeMessage(notificationMessage{JSONRPC: "2.0", Method: method, Params: params})
}

// --- Lifecycle ---

// Initialize sends the initialize request and records the server's
// capabilities for later use by Supports.
func (c *Client) Initialize(ctx context.Context, params InitializeParams) (*InitializeResult, error) {
	var result InitializeResult
	if err := c.call(ctx, "initialize", params, &result); err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.caps = result.Capabilities
	c.mu.Unlock()
	return &result, nil
}

// Initialized sends the required "initialized" notification that must
// follow a successful Initialize before any other request is sent.
func (c *Client) Initialized() error {
	return c.notify("initialized", struct{}{})
}

// Shutdown asks the server to prepare for exit. Call Exit afterward.
func (c *Client) Shutdown(ctx context.Context) error {
	return c.call(ctx, "shutdown", nil, nil)
}

// Exit sends the "exit" notification and closes stdin, telling the server
// to terminate. It does not wait for the process to actually exit; use
// Wait or Close for that.
func (c *Client) Exit() error {
	if err := c.notify("exit", nil); err != nil {
		return err
	}
	return c.stdin.Close()
}

// Wait blocks until the server process has exited (following Exit, or the
// process dying on its own) or ctx is done first, in which case it kills
// the process and returns ctx.Err().
func (c *Client) Wait(ctx context.Context) error {
	select {
	case <-c.done:
		return nil
	case <-ctx.Done():
		_ = c.Close()
		return ctx.Err()
	}
}

// Close forcibly terminates the server process, if still running, and
// waits for the read loop to finish. Safe to call multiple times and after
// a graceful Exit.
func (c *Client) Close() error {
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	_ = c.stdin.Close()
	<-c.done
	return nil
}

// --- Capabilities ---

// providerEnabled interprets an LSP `boolean | <FooOptions>` capability
// field: absent, null, or false means unsupported; true or any object
// means supported.
func providerEnabled(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		return b
	}
	return string(raw) != "null"
}

// Capabilities returns the capabilities recorded from the last Initialize
// call.
func (c *Client) Capabilities() ServerCapabilities {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.caps
}

// Supports reports whether the server advertised the given feature (one of
// the Feature* constants) in its Initialize response, so callers can avoid
// invoking methods the server doesn't implement.
func (c *Client) Supports(feature string) bool {
	c.mu.Lock()
	caps := c.caps
	c.mu.Unlock()
	switch feature {
	case FeatureDocumentSymbol:
		return providerEnabled(caps.DocumentSymbolProvider)
	case FeatureDefinition:
		return providerEnabled(caps.DefinitionProvider)
	case FeatureReferences:
		return providerEnabled(caps.ReferencesProvider)
	case FeatureCallHierarchy:
		return providerEnabled(caps.CallHierarchyProvider)
	default:
		return false
	}
}

// --- Requests ---

func (c *Client) DocumentSymbol(ctx context.Context, params DocumentSymbolParams) ([]DocumentSymbol, error) {
	var result []DocumentSymbol
	if err := c.call(ctx, "textDocument/documentSymbol", params, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (c *Client) Definition(ctx context.Context, params DefinitionParams) ([]Location, error) {
	var result []Location
	if err := c.call(ctx, "textDocument/definition", params, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (c *Client) References(ctx context.Context, params ReferenceParams) ([]Location, error) {
	var result []Location
	if err := c.call(ctx, "textDocument/references", params, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (c *Client) PrepareCallHierarchy(ctx context.Context, params CallHierarchyPrepareParams) ([]CallHierarchyItem, error) {
	var result []CallHierarchyItem
	if err := c.call(ctx, "textDocument/prepareCallHierarchy", params, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (c *Client) IncomingCalls(ctx context.Context, params CallHierarchyIncomingCallsParams) ([]CallHierarchyIncomingCall, error) {
	var result []CallHierarchyIncomingCall
	if err := c.call(ctx, "callHierarchy/incomingCalls", params, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (c *Client) OutgoingCalls(ctx context.Context, params CallHierarchyOutgoingCallsParams) ([]CallHierarchyOutgoingCall, error) {
	var result []CallHierarchyOutgoingCall
	if err := c.call(ctx, "callHierarchy/outgoingCalls", params, &result); err != nil {
		return nil, err
	}
	return result, nil
}

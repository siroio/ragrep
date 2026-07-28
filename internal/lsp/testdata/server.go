// Package testdata implements a minimal fake language server speaking
// Content-Length-framed JSON-RPC over stdio, so internal/lsp's client can
// be tested end-to-end without depending on gopls (or any real language
// server) being installed. It deliberately does not import internal/lsp:
// a real language server isn't Go code from this module either, so this
// fixture has its own independent, from-scratch wire encode/decode,
// exactly like a real server would.
package testdata

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/textproto"
	"os"
	"strconv"
	"time"
)

// Options configures the fake server's behavior for a single test
// scenario. The test client passes this as initialize's
// initializationOptions, and the fake server reflects it back into how it
// answers subsequent requests.
type Options struct {
	// DisableCallHierarchy makes initialize report callHierarchyProvider:
	// false, so tests can verify Client.Supports gates on it.
	DisableCallHierarchy bool `json:"disableCallHierarchy"`
	// CrashOnMethod makes the server exit(1) instead of responding when it
	// receives a request for this method, simulating a server crash.
	CrashOnMethod string `json:"crashOnMethod"`
	// ErrorOnMethod makes the server reply with a JSON-RPC error instead of
	// the canned fixture result for this method.
	ErrorOnMethod string `json:"errorOnMethod"`
	// DelayOnMethod makes the server sleep DelayMillis before replying to
	// this method, for exercising client-side timeouts.
	DelayOnMethod string `json:"delayOnMethod"`
	DelayMillis   int    `json:"delayMillis"`
	// NotifyAfterInitialize makes the server send a window/logMessage
	// notification right after replying to initialize.
	NotifyAfterInitialize bool `json:"notifyAfterInitialize"`
	// RequestAfterInitialize makes the server send a server-to-client
	// request (which this client doesn't implement) right after replying
	// to initialize, to exercise the MethodNotFound auto-reply path.
	RequestAfterInitialize bool `json:"requestAfterInitialize"`
}

// Canned results for the request methods this server understands. Content
// doesn't need to be realistic, only decodable into internal/lsp's types
// with predictable values a test can assert against.
const (
	documentSymbolFixture = `[{"name":"Foo","detail":"func Foo()","kind":12,"range":{"start":{"line":1,"character":0},"end":{"line":3,"character":1}},"selectionRange":{"start":{"line":1,"character":5},"end":{"line":1,"character":8}},"children":[]}]`

	definitionFixture = `[{"uri":"file:///fake/bar.go","range":{"start":{"line":10,"character":2},"end":{"line":10,"character":5}}}]`

	referencesFixture = `[{"uri":"file:///fake/bar.go","range":{"start":{"line":10,"character":2},"end":{"line":10,"character":5}}},{"uri":"file:///fake/baz.go","range":{"start":{"line":20,"character":4},"end":{"line":20,"character":7}}}]`

	prepareCallHierarchyFixture = `[{"name":"Foo","kind":12,"uri":"file:///fake/foo.go","range":{"start":{"line":1,"character":0},"end":{"line":3,"character":1}},"selectionRange":{"start":{"line":1,"character":5},"end":{"line":1,"character":8}}}]`

	incomingCallsFixture = `[{"from":{"name":"Caller","kind":12,"uri":"file:///fake/caller.go","range":{"start":{"line":5,"character":0},"end":{"line":7,"character":1}},"selectionRange":{"start":{"line":5,"character":5},"end":{"line":5,"character":11}}},"fromRanges":[{"start":{"line":6,"character":2},"end":{"line":6,"character":5}}]}]`

	outgoingCallsFixture = `[{"to":{"name":"Callee","kind":12,"uri":"file:///fake/callee.go","range":{"start":{"line":9,"character":0},"end":{"line":11,"character":1}},"selectionRange":{"start":{"line":9,"character":5},"end":{"line":9,"character":11}}},"fromRanges":[{"start":{"line":2,"character":2},"end":{"line":2,"character":8}}]}]`

	initializeResultTemplate = `{"capabilities":{"documentSymbolProvider":true,"definitionProvider":true,"referencesProvider":true,"callHierarchyProvider":%v}}`
)

var fixtures = map[string]string{
	"textDocument/documentSymbol":       documentSymbolFixture,
	"textDocument/definition":           definitionFixture,
	"textDocument/references":           referencesFixture,
	"textDocument/prepareCallHierarchy": prepareCallHierarchyFixture,
	"callHierarchy/incomingCalls":       incomingCallsFixture,
	"callHierarchy/outgoingCalls":       outgoingCallsFixture,
}

type inMessage struct {
	ID     *json.RawMessage `json:"id,omitempty"`
	Method string           `json:"method,omitempty"`
	Params json.RawMessage  `json:"params,omitempty"`
}

// Run speaks the fake protocol over r/w until the client sends "exit" or
// closes the connection. A non-nil error means something other than a
// clean client-initiated shutdown happened (bad framing, write failure).
func Run(r io.Reader, w io.Writer) error {
	tp := textproto.NewReader(bufio.NewReader(r))
	var opts Options

	for {
		hdr, err := tp.ReadMIMEHeader()
		if err != nil {
			if err == io.EOF {
				return nil // client tore down the pipe without a clean exit; not our problem
			}
			return fmt.Errorf("testdata: read header: %w", err)
		}
		n, err := strconv.Atoi(hdr.Get("Content-Length"))
		if err != nil {
			return fmt.Errorf("testdata: bad Content-Length: %w", err)
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(tp.R, body); err != nil {
			return fmt.Errorf("testdata: read body: %w", err)
		}
		var msg inMessage
		if err := json.Unmarshal(body, &msg); err != nil {
			return fmt.Errorf("testdata: bad message: %w", err)
		}

		switch msg.Method {
		case "initialize":
			var p struct {
				InitializationOptions Options `json:"initializationOptions"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			opts = p.InitializationOptions
			if err := writeResult(w, msg.ID, []byte(fmt.Sprintf(initializeResultTemplate, !opts.DisableCallHierarchy))); err != nil {
				return err
			}
			if opts.NotifyAfterInitialize {
				if err := writeNotification(w, "window/logMessage", []byte(`{"type":3,"message":"fake server ready"}`)); err != nil {
					return err
				}
			}
			if opts.RequestAfterInitialize {
				if err := writeRequest(w, 9001, "workspace/configuration", []byte(`[{}]`)); err != nil {
					return err
				}
			}

		case "initialized":
			// Notification; nothing to do.

		case "shutdown":
			if err := writeResult(w, msg.ID, []byte(`null`)); err != nil {
				return err
			}

		case "exit":
			return nil

		default:
			if fixture, ok := fixtures[msg.Method]; ok {
				if err := respond(w, msg, opts, []byte(fixture)); err != nil {
					return err
				}
				continue
			}
			// Unknown method. If it's a request (has an id), a real server
			// would answer with MethodNotFound; if it's a notification,
			// silently ignored is correct too. We only expect known
			// methods from this client, so either way there's nothing
			// meaningful to do beyond not crashing.
			if msg.ID != nil {
				if err := writeError(w, msg.ID, -32601, "method not found: "+msg.Method); err != nil {
					return err
				}
			}
		}
	}
}

// respond implements the CrashOnMethod/DelayOnMethod/ErrorOnMethod
// scenarios for request methods that otherwise return a canned fixture.
func respond(w io.Writer, msg inMessage, opts Options, fixture []byte) error {
	if opts.CrashOnMethod == msg.Method {
		os.Exit(1)
	}
	if opts.DelayOnMethod == msg.Method && opts.DelayMillis > 0 {
		time.Sleep(time.Duration(opts.DelayMillis) * time.Millisecond)
	}
	if opts.ErrorOnMethod == msg.Method {
		return writeError(w, msg.ID, -32000, "simulated error for "+msg.Method)
	}
	return writeResult(w, msg.ID, fixture)
}

func writeFrame(w io.Writer, body []byte) error {
	if _, err := fmt.Fprintf(w, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		return err
	}
	_, err := w.Write(body)
	return err
}

func writeResult(w io.Writer, id *json.RawMessage, result json.RawMessage) error {
	body, err := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  json.RawMessage `json:"result"`
	}{"2.0", *id, result})
	if err != nil {
		return err
	}
	return writeFrame(w, body)
}

func writeError(w io.Writer, id *json.RawMessage, code int, message string) error {
	body, err := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{"2.0", *id, struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}{code, message}})
	if err != nil {
		return err
	}
	return writeFrame(w, body)
}

func writeNotification(w io.Writer, method string, params json.RawMessage) error {
	body, err := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}{"2.0", method, params})
	if err != nil {
		return err
	}
	return writeFrame(w, body)
}

func writeRequest(w io.Writer, id int, method string, params json.RawMessage) error {
	body, err := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      int             `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}{"2.0", id, method, params})
	if err != nil {
		return err
	}
	return writeFrame(w, body)
}

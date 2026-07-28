// Package lsp implements a minimal Language Server Protocol client over
// stdio, using JSON-RPC 2.0 with Content-Length framing. It only knows
// about the lifecycle and requests ragrep's code indexer needs
// (initialize/shutdown, documentSymbol, definition, references, and call
// hierarchy) — not the full LSP surface.
package lsp

import (
	"encoding/json"
	"fmt"
)

// Standard JSON-RPC 2.0 error codes (see the JSON-RPC spec and
// https://microsoft.github.io/language-server-protocol/specifications/lsp/3.17/specification/#responseMessage).
const (
	ErrCodeParseError     = -32700
	ErrCodeInvalidRequest = -32600
	ErrCodeMethodNotFound = -32601
	ErrCodeInvalidParams  = -32602
	ErrCodeInternalError  = -32603
)

// requestMessage is a JSON-RPC request we send to the server.
type requestMessage struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// notificationMessage is a JSON-RPC notification (no id, no reply expected).
type notificationMessage struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// wireMessage decodes any inbound JSON-RPC message: a response to one of
// our requests (ID set, Method empty), a request from the server (ID and
// Method both set), or a notification from the server (ID empty).
type wireMessage struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *ResponseError   `json:"error,omitempty"`
}

// ResponseError is a JSON-RPC error response, returned to callers as a Go
// error (via errors.As) when the server rejects a request.
type ResponseError struct {
	Code    int64           `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *ResponseError) Error() string {
	return fmt.Sprintf("lsp: server error %d: %s", e.Code, e.Message)
}

// --- Lifecycle ---

type WorkspaceFolder struct {
	URI  string `json:"uri"`
	Name string `json:"name"`
}

type InitializeParams struct {
	ProcessID             *int               `json:"processId"`
	RootURI               string             `json:"rootUri,omitempty"`
	WorkspaceFolders      []WorkspaceFolder  `json:"workspaceFolders,omitempty"`
	Capabilities          ClientCapabilities `json:"capabilities"`
	InitializationOptions any                `json:"initializationOptions,omitempty"`
}

type ClientCapabilities struct {
	TextDocument TextDocumentClientCapabilities `json:"textDocument"`
}

type TextDocumentClientCapabilities struct {
	DocumentSymbol *DocumentSymbolClientCapabilities `json:"documentSymbol,omitempty"`
	CallHierarchy  *CallHierarchyClientCapabilities  `json:"callHierarchy,omitempty"`
}

type DocumentSymbolClientCapabilities struct {
	HierarchicalDocumentSymbolSupport bool `json:"hierarchicalDocumentSymbolSupport,omitempty"`
}

type CallHierarchyClientCapabilities struct{}

// DefaultClientCapabilities returns the capabilities ragrep advertises to
// every server: hierarchical document symbols and call hierarchy support,
// which is all the client code in this package uses.
func DefaultClientCapabilities() ClientCapabilities {
	return ClientCapabilities{
		TextDocument: TextDocumentClientCapabilities{
			DocumentSymbol: &DocumentSymbolClientCapabilities{HierarchicalDocumentSymbolSupport: true},
			CallHierarchy:  &CallHierarchyClientCapabilities{},
		},
	}
}

type InitializeResult struct {
	Capabilities ServerCapabilities `json:"capabilities"`
	ServerInfo   *ServerInfo        `json:"serverInfo,omitempty"`
}

type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// ServerCapabilities records the subset of the server's advertised
// capabilities that this client acts on. Each *Provider field is LSP's
// `boolean | <FooOptions>` union: absent/false/null means unsupported, any
// other value (true, or an options object) means supported. Kept as
// json.RawMessage rather than decoded, since only that presence/absence
// distinction is used — see providerEnabled and Client.Supports.
type ServerCapabilities struct {
	DefinitionProvider     json.RawMessage `json:"definitionProvider,omitempty"`
	ReferencesProvider     json.RawMessage `json:"referencesProvider,omitempty"`
	DocumentSymbolProvider json.RawMessage `json:"documentSymbolProvider,omitempty"`
	CallHierarchyProvider  json.RawMessage `json:"callHierarchyProvider,omitempty"`
}

// --- Shared basic types ---

type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

type Location struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
}

type TextDocumentIdentifier struct {
	URI string `json:"uri"`
}

type TextDocumentPositionParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
	Position     Position               `json:"position"`
}

// --- textDocument/documentSymbol ---

type DocumentSymbolParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
}

// DocumentSymbol is the hierarchical shape returned when the client
// advertises hierarchicalDocumentSymbolSupport (see
// DefaultClientCapabilities), which gopls honors.
type DocumentSymbol struct {
	Name           string           `json:"name"`
	Detail         string           `json:"detail,omitempty"`
	Kind           int              `json:"kind"`
	Range          Range            `json:"range"`
	SelectionRange Range            `json:"selectionRange"`
	Children       []DocumentSymbol `json:"children,omitempty"`
}

// --- textDocument/definition ---

type DefinitionParams = TextDocumentPositionParams

// --- textDocument/references ---

type ReferenceContext struct {
	IncludeDeclaration bool `json:"includeDeclaration"`
}

type ReferenceParams struct {
	TextDocumentPositionParams
	Context ReferenceContext `json:"context"`
}

// --- Call hierarchy ---

type CallHierarchyPrepareParams = TextDocumentPositionParams

type CallHierarchyItem struct {
	Name           string          `json:"name"`
	Kind           int             `json:"kind"`
	Detail         string          `json:"detail,omitempty"`
	URI            string          `json:"uri"`
	Range          Range           `json:"range"`
	SelectionRange Range           `json:"selectionRange"`
	Data           json.RawMessage `json:"data,omitempty"`
}

type CallHierarchyIncomingCallsParams struct {
	Item CallHierarchyItem `json:"item"`
}

type CallHierarchyIncomingCall struct {
	From       CallHierarchyItem `json:"from"`
	FromRanges []Range           `json:"fromRanges"`
}

type CallHierarchyOutgoingCallsParams struct {
	Item CallHierarchyItem `json:"item"`
}

type CallHierarchyOutgoingCall struct {
	To         CallHierarchyItem `json:"to"`
	FromRanges []Range           `json:"fromRanges"`
}

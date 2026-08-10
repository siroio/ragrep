package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/siroio/ragrep/internal/codestore"
)

type searchCodeInput struct {
	Query string `json:"query"`
	Mode  string `json:"mode,omitempty"`
	Limit int    `json:"limit,omitempty"`
	Root  string `json:"root,omitempty"`
}

type searchCodeData struct {
	Hits       []codestore.SymbolHit `json:"hits"`
	Fresh      bool                  `json:"fresh"`
	Generation uint64                `json:"generation"`
	Degraded   string                `json:"degraded,omitempty"`
	UsedVector bool                  `json:"used_vector"`
}

type readCodeSymbolInput struct {
	Key  string `json:"key"`
	Root string `json:"root,omitempty"`
}

type readCodeSymbolData struct {
	Symbol codeSymbolOutput `json:"symbol"`
}

type inspectCodeRelationInput struct {
	Key      string `json:"key"`
	Relation string `json:"relation"`
	Root     string `json:"root,omitempty"`
}

type inspectCodeRelationData struct {
	Targets []codeExpandTarget `json:"targets"`
}

type codeQueryBackend interface {
	SearchCode(context.Context, mcpWorkspace, searchCodeInput) (searchCodeData, error)
	ReadCodeSymbol(context.Context, mcpWorkspace, readCodeSymbolInput) (readCodeSymbolData, error)
	InspectCodeRelation(context.Context, mcpWorkspace, inspectCodeRelationInput) (inspectCodeRelationData, error)
}

type codeQueryTools struct {
	defaultRoot string
	backend     codeQueryBackend
}

func registerCodeQueryTools(server *mcp.Server, tools codeQueryTools) {
	closedWorld := false
	annotations := &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &closedWorld}
	mcp.AddTool(server, &mcp.Tool{
		Name: "search_code", Description: "Search indexed code symbols before reading a selected body.", Annotations: annotations,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input searchCodeInput) (*mcp.CallToolResult, mcpToolOutput[searchCodeData], error) {
		return tools.searchCode(ctx, input)
	})
	mcp.AddTool(server, &mcp.Tool{
		Name: "read_code_symbol", Description: "Read a selected search_code candidate's body as evidence before answering or following relations.", Annotations: annotations,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input readCodeSymbolInput) (*mcp.CallToolResult, mcpToolOutput[readCodeSymbolData], error) {
		return tools.readCodeSymbol(ctx, input)
	})
	mcp.AddTool(server, &mcp.Tool{
		Name: "inspect_code_relation", Description: "Inspect one needed relation, then read the selected target body with read_code_symbol.", Annotations: annotations,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input inspectCodeRelationInput) (*mcp.CallToolResult, mcpToolOutput[inspectCodeRelationData], error) {
		return tools.inspectCodeRelation(ctx, input)
	})
}

func (tools codeQueryTools) searchCode(ctx context.Context, input searchCodeInput) (*mcp.CallToolResult, mcpToolOutput[searchCodeData], error) {
	if err := ctx.Err(); err != nil {
		return mcpToolFailure[searchCodeData](err)
	}
	if strings.TrimSpace(input.Query) == "" {
		return mcpToolFailure[searchCodeData](mcpInvalidArgument())
	}
	if input.Mode == "" {
		input.Mode = "auto"
	}
	if input.Mode != "auto" && input.Mode != "text" && input.Mode != "hybrid" {
		return mcpToolFailure[searchCodeData](mcpInvalidArgument())
	}
	if input.Limit == 0 {
		input.Limit = 5
	}
	if input.Limit < 1 || input.Limit > 10 {
		return mcpToolFailure[searchCodeData](mcpInvalidArgument())
	}
	ws, err := resolveMCPWorkspace(tools.defaultRoot, input.Root)
	if err != nil {
		return mcpToolFailure[searchCodeData](err)
	}
	data, err := tools.backend.SearchCode(ctx, ws, input)
	if err != nil {
		return mcpToolFailure[searchCodeData](err)
	}
	if data.Hits == nil {
		data.Hits = []codestore.SymbolHit{}
	}
	return mcpSuccess(fmt.Sprintf("%d code hits", len(data.Hits)), data)
}

func (tools codeQueryTools) readCodeSymbol(ctx context.Context, input readCodeSymbolInput) (*mcp.CallToolResult, mcpToolOutput[readCodeSymbolData], error) {
	if err := ctx.Err(); err != nil {
		return mcpToolFailure[readCodeSymbolData](err)
	}
	if strings.TrimSpace(input.Key) == "" {
		return mcpToolFailure[readCodeSymbolData](mcpInvalidArgument())
	}
	ws, err := resolveMCPWorkspace(tools.defaultRoot, input.Root)
	if err != nil {
		return mcpToolFailure[readCodeSymbolData](err)
	}
	data, err := tools.backend.ReadCodeSymbol(ctx, ws, input)
	if err != nil {
		return mcpToolFailure[readCodeSymbolData](err)
	}
	return mcpSuccess("code symbol read", data)
}

func (tools codeQueryTools) inspectCodeRelation(ctx context.Context, input inspectCodeRelationInput) (*mcp.CallToolResult, mcpToolOutput[inspectCodeRelationData], error) {
	if err := ctx.Err(); err != nil {
		return mcpToolFailure[inspectCodeRelationData](err)
	}
	if strings.TrimSpace(input.Key) == "" || !validCodeRelation(input.Relation) {
		return mcpToolFailure[inspectCodeRelationData](mcpInvalidArgument())
	}
	ws, err := resolveMCPWorkspace(tools.defaultRoot, input.Root)
	if err != nil {
		return mcpToolFailure[inspectCodeRelationData](err)
	}
	data, err := tools.backend.InspectCodeRelation(ctx, ws, input)
	if err != nil {
		return mcpToolFailure[inspectCodeRelationData](err)
	}
	if data.Targets == nil {
		data.Targets = []codeExpandTarget{}
	}
	return mcpSuccess(fmt.Sprintf("%d relation targets", len(data.Targets)), data)
}

func validCodeRelation(relation string) bool {
	switch relation {
	case "definition", "references", "callers", "callees", "tests":
		return true
	default:
		return false
	}
}

func (productionMCPBackend) SearchCode(ctx context.Context, ws mcpWorkspace, input searchCodeInput) (searchCodeData, error) {
	if err := requireCodeIndex(ws.CodeDB); err != nil {
		return searchCodeData{}, err
	}
	client, err := codeDaemonClientFactory()
	if err != nil {
		return searchCodeData{}, err
	}
	response, err := client.Search(ctx, searchRequest{Root: ws.Root, DB: ws.CodeDB, Query: input.Query, Mode: input.Mode, K: input.Limit})
	if err != nil {
		return searchCodeData{}, codeDaemonMCPError(err)
	}
	return searchCodeData{
		Hits: response.Hits, Fresh: response.Fresh, Generation: response.Generation,
		Degraded: response.Degraded, UsedVector: response.UsedVector,
	}, nil
}

func (productionMCPBackend) ReadCodeSymbol(ctx context.Context, ws mcpWorkspace, input readCodeSymbolInput) (readCodeSymbolData, error) {
	if err := requireCodeIndex(ws.CodeDB); err != nil {
		return readCodeSymbolData{}, err
	}
	client, err := codeDaemonClientFactory()
	if err != nil {
		return readCodeSymbolData{}, err
	}
	symbol, err := client.Get(ctx, getRequest{Root: ws.Root, DB: ws.CodeDB, Key: input.Key, Body: true})
	if err != nil {
		return readCodeSymbolData{}, codeDaemonMCPError(err)
	}
	return readCodeSymbolData{Symbol: newCodeSymbolOutput(symbol, true)}, nil
}

func (productionMCPBackend) InspectCodeRelation(ctx context.Context, ws mcpWorkspace, input inspectCodeRelationInput) (inspectCodeRelationData, error) {
	if err := requireCodeIndex(ws.CodeDB); err != nil {
		return inspectCodeRelationData{}, err
	}
	client, err := codeDaemonClientFactory()
	if err != nil {
		return inspectCodeRelationData{}, err
	}
	targets, err := client.Expand(ctx, expandRequest{Root: ws.Root, DB: ws.CodeDB, Key: input.Key, Relation: input.Relation})
	if err != nil {
		return inspectCodeRelationData{}, codeDaemonMCPError(err)
	}
	return inspectCodeRelationData{Targets: targets}, nil
}

func requireCodeIndex(path string) error {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return mcpIndexRequired()
		}
		return err
	}
	return nil
}

func codeDaemonMCPError(err error) error {
	var apiErr *apiError
	if !errors.As(err, &apiErr) {
		return err
	}
	switch apiErr.Code {
	case "workspace_syncing":
		return ErrWorkspaceSyncing
	case "stale_live_key":
		return ErrStaleLiveKey
	case "not_found":
		return codestore.ErrNotFound
	default:
		return err
	}
}

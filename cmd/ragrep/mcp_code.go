package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/siroio/ragrep/internal/coderetrieval"
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
	Targets   []codeExpandTarget `json:"targets"`
	Total     int                `json:"total"`
	Truncated bool               `json:"truncated"`
}

const maxMCPRelationTargets = 20
const maxMCPCodeKeyBytes = 1024

type codeQueryBackend interface {
	SearchCode(context.Context, mcpWorkspace, searchCodeInput) (searchCodeData, error)
	ReadCodeSymbol(context.Context, mcpWorkspace, readCodeSymbolInput) (readCodeSymbolData, error)
	InspectCodeRelation(context.Context, mcpWorkspace, inspectCodeRelationInput) (inspectCodeRelationData, error)
}

type codeQueryTools struct {
	defaultRoot string
	backend     codeQueryBackend
}

type buildCodeContextInput struct {
	Query        string   `json:"query"`
	SelectedKeys []string `json:"selected_keys,omitempty"`
	Budget       int      `json:"budget,omitempty"`
	Root         string   `json:"root,omitempty"`
}

type buildCodeContextData = codePackOutput

type verifyCodeContextInput struct {
	Manifest coderetrieval.Manifest `json:"manifest"`
	Root     string                 `json:"root,omitempty"`
}

type verifyCodeContextData = codeVerifyOutput

type codeContextBackend interface {
	BuildCodeContext(context.Context, mcpWorkspace, buildCodeContextInput) (buildCodeContextData, error)
	VerifyCodeContext(context.Context, mcpWorkspace, verifyCodeContextInput) (verifyCodeContextData, error)
}

type codeContextTools struct {
	defaultRoot string
	backend     codeContextBackend
}

func registerCodeQueryTools(server *mcp.Server, tools codeQueryTools) {
	closedWorld := false
	annotations := &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &closedWorld}
	mcp.AddTool(server, &mcp.Tool{
		Name: "search_code", Description: "Use to find indexed code candidates, then call read_code_symbol on the relevant result.", Annotations: annotations,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input searchCodeInput) (*mcp.CallToolResult, mcpToolOutput[searchCodeData], error) {
		return tools.searchCode(ctx, input)
	})
	mcp.AddTool(server, &mcp.Tool{
		Name: "read_code_symbol", Description: "Use after search_code to read a body, then call inspect_code_relation or build_code_context if more evidence is needed.", Annotations: annotations,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input readCodeSymbolInput) (*mcp.CallToolResult, mcpToolOutput[readCodeSymbolData], error) {
		return tools.readCodeSymbol(ctx, input)
	})
	mcp.AddTool(server, &mcp.Tool{
		Name: "inspect_code_relation", Description: "Use after read_code_symbol for one relation, then call read_code_symbol or build_code_context on relevant targets.", Annotations: annotations,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input inspectCodeRelationInput) (*mcp.CallToolResult, mcpToolOutput[inspectCodeRelationData], error) {
		return tools.inspectCodeRelation(ctx, input)
	})
}

func registerCodeContextTools(server *mcp.Server, tools codeContextTools) {
	closedWorld := false
	annotations := &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &closedWorld}
	mcp.AddTool(server, &mcp.Tool{
		Name: "build_code_context", Description: "Use after search_code to build bounded evidence, then call verify_code_context before relying on it.", Annotations: annotations,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input buildCodeContextInput) (*mcp.CallToolResult, mcpToolOutput[buildCodeContextData], error) {
		return tools.buildCodeContext(ctx, input)
	})
	mcp.AddTool(server, &mcp.Tool{
		Name: "verify_code_context", Description: "Use immediately before relying on built evidence; call build_code_context again when clean is false.", Annotations: annotations,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input verifyCodeContextInput) (*mcp.CallToolResult, mcpToolOutput[verifyCodeContextData], error) {
		return tools.verifyCodeContext(ctx, input)
	})
}

func (tools codeContextTools) buildCodeContext(ctx context.Context, input buildCodeContextInput) (*mcp.CallToolResult, mcpToolOutput[buildCodeContextData], error) {
	if err := ctx.Err(); err != nil {
		return mcpToolFailure[buildCodeContextData](err)
	}
	if strings.TrimSpace(input.Query) == "" || input.Budget < 0 || len(input.SelectedKeys) > 3 {
		return mcpToolFailure[buildCodeContextData](mcpInvalidArgument())
	}
	if input.Budget == 0 {
		input.Budget = codePackDefaultBudget
	}
	ws, err := resolveMCPWorkspace(tools.defaultRoot, input.Root)
	if err != nil {
		return mcpToolFailure[buildCodeContextData](err)
	}
	data, err := tools.backend.BuildCodeContext(ctx, ws, input)
	if err != nil {
		return mcpToolFailure[buildCodeContextData](err)
	}
	return mcpSuccess(fmt.Sprintf("%d code candidates", len(data.Pack.Candidates)), data)
}

func (tools codeContextTools) verifyCodeContext(ctx context.Context, input verifyCodeContextInput) (*mcp.CallToolResult, mcpToolOutput[verifyCodeContextData], error) {
	if err := ctx.Err(); err != nil {
		return mcpToolFailure[verifyCodeContextData](err)
	}
	if len(input.Manifest.Symbols) == 0 || len(input.Manifest.Symbols) > 3 {
		return mcpToolFailure[verifyCodeContextData](mcpInvalidArgument())
	}
	for _, ref := range input.Manifest.Symbols {
		if !validCodeManifestPath(ref.Path) {
			return mcpToolFailure[verifyCodeContextData](mcpInvalidArgument())
		}
	}
	ws, err := resolveMCPWorkspace(tools.defaultRoot, input.Root)
	if err != nil {
		return mcpToolFailure[verifyCodeContextData](err)
	}
	data, err := tools.backend.VerifyCodeContext(ctx, ws, input)
	if err != nil {
		return mcpToolFailure[verifyCodeContextData](err)
	}
	return mcpSuccess(fmt.Sprintf("code context clean=%v", data.Clean), data)
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
	if !validMCPCodeKey(input.Key) {
		return mcpToolFailure[readCodeSymbolData](mcpInvalidArgument())
	}
	ws, err := resolveMCPWorkspace(tools.defaultRoot, input.Root)
	if err != nil {
		return mcpToolFailure[readCodeSymbolData](err)
	}
	data, err := tools.backend.ReadCodeSymbol(ctx, ws, input)
	if err != nil {
		if errors.Is(err, codestore.ErrNotFound) {
			return mcpCodeKeyNotFound[readCodeSymbolData](input.Key)
		}
		return mcpToolFailure[readCodeSymbolData](err)
	}
	return mcpSuccess("code symbol read", data)
}

func (tools codeQueryTools) inspectCodeRelation(ctx context.Context, input inspectCodeRelationInput) (*mcp.CallToolResult, mcpToolOutput[inspectCodeRelationData], error) {
	if err := ctx.Err(); err != nil {
		return mcpToolFailure[inspectCodeRelationData](err)
	}
	if !validMCPCodeKey(input.Key) || !validCodeRelation(input.Relation) {
		return mcpToolFailure[inspectCodeRelationData](mcpInvalidArgument())
	}
	ws, err := resolveMCPWorkspace(tools.defaultRoot, input.Root)
	if err != nil {
		return mcpToolFailure[inspectCodeRelationData](err)
	}
	data, err := tools.backend.InspectCodeRelation(ctx, ws, input)
	if err != nil {
		if errors.Is(err, codestore.ErrNotFound) {
			return mcpCodeKeyNotFound[inspectCodeRelationData](input.Key)
		}
		return mcpToolFailure[inspectCodeRelationData](err)
	}
	filtered := make([]codeExpandTarget, 0, len(data.Targets))
	for _, target := range data.Targets {
		if validCodeManifestPath(target.Path) {
			filtered = append(filtered, target)
		}
	}
	data.Total = len(filtered)
	data.Truncated = data.Total > maxMCPRelationTargets
	if data.Truncated {
		filtered = filtered[:maxMCPRelationTargets]
	}
	data.Targets = filtered
	return mcpSuccess(fmt.Sprintf("%d of %d relation targets", len(data.Targets), data.Total), data)
}

func validMCPCodeKey(key string) bool {
	if strings.TrimSpace(key) == "" || len(key) > maxMCPCodeKeyBytes || !utf8.ValidString(key) {
		return false
	}
	for _, r := range key {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func mcpCodeKeyNotFound[T any](key string) (*mcp.CallToolResult, mcpToolOutput[T], error) {
	failure := mcpFailure{
		Code: "not_found", Message: fmt.Sprintf("code symbol key %q was not found", key),
		Recovery: "rerun search_code and use a returned key",
	}
	out := mcpToolOutput[T]{Error: &failure}
	data, err := json.Marshal(out)
	if err != nil {
		return nil, mcpToolOutput[T]{}, err
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(data)}}, StructuredContent: out, IsError: true,
	}, out, nil
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

func (productionMCPBackend) BuildCodeContext(ctx context.Context, ws mcpWorkspace, input buildCodeContextInput) (buildCodeContextData, error) {
	if err := requireCodeIndex(ws.CodeDB); err != nil {
		return buildCodeContextData{}, err
	}
	client, err := codeDaemonClientFactory()
	if err != nil {
		return buildCodeContextData{}, err
	}
	out, err := client.Pack(ctx, packRequest{
		Root: ws.Root, DB: ws.CodeDB, Query: input.Query, K: 10, Budget: input.Budget, SelectedKeys: input.SelectedKeys,
	})
	if err != nil {
		return buildCodeContextData{}, codeDaemonMCPError(err)
	}
	return out, nil
}

func (productionMCPBackend) VerifyCodeContext(ctx context.Context, ws mcpWorkspace, input verifyCodeContextInput) (verifyCodeContextData, error) {
	if err := requireCodeIndex(ws.CodeDB); err != nil {
		return verifyCodeContextData{}, err
	}
	client, err := codeDaemonClientFactory()
	if err != nil {
		return verifyCodeContextData{}, err
	}
	out, err := client.Verify(ctx, verifyRequest{Root: ws.Root, DB: ws.CodeDB, Manifest: input.Manifest})
	if err != nil {
		return verifyCodeContextData{}, codeDaemonMCPError(err)
	}
	return out, nil
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

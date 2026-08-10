package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/siroio/ragrep/internal/config"
	"github.com/siroio/ragrep/internal/store"
)

const mcpServerInstructions = "Search before reading bodies. Snippets and code hits are candidates. Reindex stale documents then repeat search. Add documents only on explicit user request. Cite sources as path#paragraph."

type mcpWorkspace struct {
	Root, DocumentDB, CodeDB string
}

type mcpFailure struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
	Recovery  string `json:"recovery,omitempty"`
}

type mcpDomainError struct {
	Failure mcpFailure
}

func (e *mcpDomainError) Error() string {
	if e == nil {
		return ""
	}
	if e.Failure.Message != "" {
		return e.Failure.Message
	}
	return e.Failure.Code
}

type mcpToolOutput[T any] struct {
	Data  *T          `json:"data,omitempty"`
	Error *mcpFailure `json:"error,omitempty"`
}

type productionMCPBackend struct{}

func discoverMCPWorkspace(cwd string) (mcpWorkspace, error) {
	dir, err := filepath.Abs(cwd)
	if err != nil {
		return mcpWorkspace{}, err
	}
	for {
		info, err := os.Stat(filepath.Join(dir, ".ragrep"))
		if err == nil && info.IsDir() {
			cfg, err := config.Load(dir)
			if err != nil {
				return mcpWorkspace{}, err
			}
			return mcpWorkspace{
				Root:       dir,
				DocumentDB: filepath.Join(dir, filepath.FromSlash(cfg.DB)),
				CodeDB:     filepath.Join(dir, filepath.FromSlash(cfg.CodeDB)),
			}, nil
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return mcpWorkspace{}, err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return mcpWorkspace{}, ErrWorkspaceNotFound
		}
		dir = parent
	}
}

func resolveMCPWorkspace(defaultRoot, requestedRoot string) (mcpWorkspace, error) {
	if requestedRoot != "" {
		return discoverMCPWorkspace(requestedRoot)
	}
	return discoverMCPWorkspace(defaultRoot)
}

func classifyMCPError(err error) mcpFailure {
	var domain *mcpDomainError
	if errors.As(err, &domain) {
		return normalizedMCPFailure(domain.Failure)
	}
	switch {
	case errors.Is(err, ErrWorkspaceNotFound):
		return mcpFailureForCode("workspace_not_found")
	case errors.Is(err, ErrWorkspaceSyncing):
		return mcpFailureForCode("workspace_syncing")
	case errors.Is(err, ErrStaleLiveKey):
		return mcpFailureForCode("stale_live_key")
	case errors.Is(err, store.ErrNotFound):
		return mcpFailureForCode("not_found")
	case errors.Is(err, os.ErrNotExist):
		return mcpFailureForCode("daemon_unavailable")
	}
	var networkError net.Error
	if errors.As(err, &networkError) {
		return mcpFailureForCode("daemon_unavailable")
	}
	return mcpFailureForCode("partial_failure")
}

func normalizedMCPFailure(failure mcpFailure) mcpFailure {
	defaults := mcpFailureForCode(failure.Code)
	if failure.Code == "" || defaults.Code != failure.Code {
		return mcpFailureForCode("partial_failure")
	}
	return defaults
}

func mcpFailureForCode(code string) mcpFailure {
	switch code {
	case "invalid_argument":
		return mcpFailure{Code: code, Message: "invalid argument"}
	case "workspace_not_found":
		return mcpFailure{Code: code, Message: "workspace not found"}
	case "path_outside_workspace":
		return mcpFailure{Code: code, Message: "path is outside the workspace"}
	case "already_exists":
		return mcpFailure{Code: code, Message: "already exists"}
	case "not_found":
		return mcpFailure{Code: code, Message: "not found"}
	case "workspace_syncing":
		return mcpFailure{Code: code, Message: "workspace is synchronizing", Retryable: true, Recovery: "repeat search after synchronization completes"}
	case "stale_live_key":
		return mcpFailure{Code: code, Message: "search result key is stale", Recovery: "discard the key and rerun search"}
	case "daemon_unavailable":
		return mcpFailure{Code: code, Message: "ragrep daemon is unavailable", Retryable: true, Recovery: "start the ragrep daemon and retry"}
	case "index_required":
		return mcpFailure{Code: code, Message: "index is required"}
	case "partial_failure":
		return mcpFailure{Code: code, Message: "request partially completed"}
	default:
		return mcpFailure{}
	}
}

func mcpSuccess[T any](message string, data T) (*mcp.CallToolResult, mcpToolOutput[T], error) {
	out := mcpToolOutput[T]{Data: &data}
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: message}},
		StructuredContent: out,
	}, out, nil
}

func mcpToolFailure[T any](err error) (*mcp.CallToolResult, mcpToolOutput[T], error) {
	failure := classifyMCPError(err)
	out := mcpToolOutput[T]{Error: &failure}
	data, marshalErr := json.Marshal(out)
	if marshalErr != nil {
		return nil, mcpToolOutput[T]{}, marshalErr
	}
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: string(data)}},
		StructuredContent: out,
		IsError:           true,
	}, out, nil
}

func newMCPBaseServer() *mcp.Server {
	return mcp.NewServer(
		&mcp.Implementation{Name: "ragrep", Version: "0.1.0"},
		&mcp.ServerOptions{Instructions: mcpServerInstructions},
	)
}

func runMCPServer(ctx context.Context, transport mcp.Transport, server *mcp.Server) error {
	return server.Run(ctx, transport)
}

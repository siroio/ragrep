package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/siroio/ragrep/internal/store"
)

func TestResolveMCPWorkspaceUsesNearestRagrepAndConfig(t *testing.T) {
	outer := t.TempDir()
	inner := filepath.Join(outer, "inner")
	for _, root := range []string{outer, inner} {
		if err := os.MkdirAll(filepath.Join(root, ".ragrep"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(inner, ".ragrep", "config.json"), []byte(`{"db":"state/docs.db","code_db":"state/code.db"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := resolveMCPWorkspace(outer, filepath.Join(inner, "nested", "dir"))
	if err != nil {
		t.Fatalf("resolveMCPWorkspace: %v", err)
	}
	if want := (mcpWorkspace{
		Root:       inner,
		DocumentDB: filepath.Join(inner, "state", "docs.db"),
		CodeDB:     filepath.Join(inner, "state", "code.db"),
	}); got != want {
		t.Fatalf("workspace = %#v, want %#v", got, want)
	}
}

func TestResolveMCPWorkspaceRejectsMissingWorkspace(t *testing.T) {
	_, err := resolveMCPWorkspace(filepath.Join(t.TempDir(), "missing"), "")
	if !errors.Is(err, ErrWorkspaceNotFound) {
		t.Fatalf("resolveMCPWorkspace error = %v, want ErrWorkspaceNotFound", err)
	}
}

func TestMCPToolFailureMapsWorkspaceSyncingAndStaleKey(t *testing.T) {
	cases := []struct {
		name         string
		err          error
		code         string
		retryable    bool
		recoveryPart string
	}{
		{"workspace syncing", ErrWorkspaceSyncing, "workspace_syncing", true, "repeat search"},
		{"stale key", ErrStaleLiveKey, "stale_live_key", false, "discard the key and rerun search"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, out, err := mcpToolFailure[struct{}](tc.err)
			if err != nil {
				t.Fatalf("mcpToolFailure: %v", err)
			}
			if out.Error == nil {
				t.Fatal("mcpToolFailure returned no structured error")
			}
			if got := out.Error.Code; got != tc.code {
				t.Fatalf("error code = %q, want %q", got, tc.code)
			}
			if got := out.Error.Retryable; got != tc.retryable {
				t.Fatalf("retryable = %v, want %v", got, tc.retryable)
			}
			if !strings.Contains(out.Error.Recovery, tc.recoveryPart) {
				t.Fatalf("recovery = %q, want it to contain %q", out.Error.Recovery, tc.recoveryPart)
			}
		})
	}
}

func TestMCPToolFailureDoesNotExposeDBOrToken(t *testing.T) {
	secret := &mcpDomainError{Failure: mcpFailure{
		Code:     "invalid_argument",
		Message:  `dial tcp C:\\private\\index.db: Bearer top-secret-token`,
		Recovery: "use token top-secret-token",
	}}
	result, out, err := mcpToolFailure[struct{}](secret)
	if err != nil {
		t.Fatalf("mcpToolFailure: %v", err)
	}
	encoded, err := json.Marshal(struct {
		Result *mcp.CallToolResult     `json:"result"`
		Output mcpToolOutput[struct{}] `json:"output"`
	}{result, out})
	if err != nil {
		t.Fatalf("marshal structured failure: %v", err)
	}
	for _, forbidden := range []string{"C:\\private\\index.db", "top-secret-token"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("structured failure exposed %q: %s", forbidden, encoded)
		}
	}
}

func TestMCPToolOutputHasDataXorError(t *testing.T) {
	success, successOut, err := mcpSuccess("found result", "value")
	if err != nil {
		t.Fatalf("mcpSuccess: %v", err)
	}
	if successOut.Data == nil || successOut.Error != nil || success.IsError {
		t.Fatalf("success output = %#v, result IsError = %v", successOut, success.IsError)
	}

	failure, failureOut, err := mcpToolFailure[string](store.ErrNotFound)
	if err != nil {
		t.Fatalf("mcpToolFailure: %v", err)
	}
	if failureOut.Data != nil || failureOut.Error == nil || !failure.IsError {
		t.Fatalf("failure output = %#v, result IsError = %v", failureOut, failure.IsError)
	}
}

func TestRunMCPServerPropagatesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	serverTransport, _ := mcp.NewInMemoryTransports()
	err := runMCPServer(ctx, serverTransport, newMCPBaseServer())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runMCPServer error = %v, want context.Canceled", err)
	}
}

func TestMCPToolFailureUsesStableCodes(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode string
	}{
		{"invalid argument", &mcpDomainError{Failure: mcpFailure{Code: "invalid_argument"}}, "invalid_argument"},
		{"workspace not found", ErrWorkspaceNotFound, "workspace_not_found"},
		{"path outside workspace", &mcpDomainError{Failure: mcpFailure{Code: "path_outside_workspace"}}, "path_outside_workspace"},
		{"already exists", &mcpDomainError{Failure: mcpFailure{Code: "already_exists"}}, "already_exists"},
		{"not found", store.ErrNotFound, "not_found"},
		{"workspace syncing", ErrWorkspaceSyncing, "workspace_syncing"},
		{"stale live key", ErrStaleLiveKey, "stale_live_key"},
		{"daemon unavailable", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, "daemon_unavailable"},
		{"index required", &mcpDomainError{Failure: mcpFailure{Code: "index_required"}}, "index_required"},
		{"partial failure", &mcpDomainError{Failure: mcpFailure{Code: "partial_failure"}}, "partial_failure"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, out, err := mcpToolFailure[struct{}](tc.err)
			if err != nil {
				t.Fatalf("mcpToolFailure: %v", err)
			}
			if result.IsError != true || out.Data != nil || out.Error == nil {
				t.Fatalf("tool failure result = %#v, output = %#v", result, out)
			}
			if got := out.Error.Code; got != tc.wantCode {
				t.Fatalf("mcpToolFailure(%v).Code = %q, want %q", tc.err, got, tc.wantCode)
			}
		})
	}

	failure := classifyMCPError(&net.OpError{Op: "dial", Err: errors.New("token=super-secret")})
	if failure.Message != "ragrep daemon is unavailable" {
		t.Fatalf("daemon failure message = %q", failure.Message)
	}
}

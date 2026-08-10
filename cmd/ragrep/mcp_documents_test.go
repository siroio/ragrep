package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/siroio/ragrep/internal/store"
)

type fakeDocumentQueryBackend struct {
	search func(context.Context, mcpWorkspace, searchDocumentsInput) (searchDocumentsData, error)
	read   func(context.Context, mcpWorkspace, readDocumentInput) (readDocumentData, error)
}

func (b fakeDocumentQueryBackend) SearchDocuments(ctx context.Context, ws mcpWorkspace, in searchDocumentsInput) (searchDocumentsData, error) {
	return b.search(ctx, ws, in)
}

func (b fakeDocumentQueryBackend) ReadDocument(ctx context.Context, ws mcpWorkspace, in readDocumentInput) (readDocumentData, error) {
	return b.read(ctx, ws, in)
}

func newMCPDocumentWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".ragrep"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func connectDocumentTools(t *testing.T, tools documentQueryTools) *mcp.ClientSession {
	t.Helper()
	server := newMCPBaseServer()
	registerDocumentQueryTools(server, tools)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { serverSession.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0.1.0"}, nil)
	session, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

func documentToolOutput[T any](t *testing.T, result *mcp.CallToolResult) mcpToolOutput[T] {
	t.Helper()
	data, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var out mcpToolOutput[T]
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSearchDocumentsDefaultsAndPublicDTO(t *testing.T) {
	root := newMCPDocumentWorkspace(t)
	var gotWS mcpWorkspace
	var gotInput searchDocumentsInput
	backend := fakeDocumentQueryBackend{
		search: func(_ context.Context, ws mcpWorkspace, in searchDocumentsInput) (searchDocumentsData, error) {
			gotWS, gotInput = ws, in
			return searchDocumentsData{Hits: []documentHitOutput{{Path: "notes/guide.md", Paragraph: 3, Heading: "Guide", Lines: "8-10", Score: 0.75, Snippet: "matching text", Stale: true}}}, nil
		},
	}
	session := connectDocumentTools(t, documentQueryTools{defaultRoot: root, backend: backend})
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "search_documents", Arguments: map[string]any{"query": "matching", "tags": []string{"guide"}}})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("result=%+v", result)
	}
	if gotWS.Root != root {
		t.Fatalf("workspace root=%q, want %q", gotWS.Root, root)
	}
	if gotInput.Query != "matching" || gotInput.Mode != "hybrid" || gotInput.Limit != 5 || len(gotInput.Tags) != 1 || gotInput.Tags[0] != "guide" {
		t.Fatalf("input=%+v, want hybrid mode, limit 5, and guide tag", gotInput)
	}
	out := documentToolOutput[searchDocumentsData](t, result)
	if out.Data == nil || len(out.Data.Hits) != 1 || out.Data.Hits[0].Path != "notes/guide.md" || out.Data.Hits[0].Paragraph != 3 {
		t.Fatalf("output=%+v", out)
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "mtime") || strings.Contains(string(encoded), filepath.Join(root, ".ragrep", "index.db")) {
		t.Fatalf("public output leaked internal data: %s", encoded)
	}
}

func TestSearchDocumentsValidationEmptyResultsRootAndCancellation(t *testing.T) {
	root := newMCPDocumentWorkspace(t)
	otherRoot := newMCPDocumentWorkspace(t)
	var gotRoot string
	backend := fakeDocumentQueryBackend{
		search: func(ctx context.Context, ws mcpWorkspace, _ searchDocumentsInput) (searchDocumentsData, error) {
			gotRoot = ws.Root
			if err := ctx.Err(); err != nil {
				return searchDocumentsData{}, err
			}
			return searchDocumentsData{Hits: []documentHitOutput{}}, nil
		},
	}
	tools := documentQueryTools{defaultRoot: root, backend: backend}
	session := connectDocumentTools(t, tools)
	for _, arguments := range []map[string]any{
		{"query": "   "},
		{"query": "q", "mode": "unknown"},
		{"query": "q", "limit": -1},
		{"query": "q", "limit": 11},
	} {
		result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "search_documents", Arguments: arguments})
		if err != nil {
			t.Fatal(err)
		}
		out := documentToolOutput[searchDocumentsData](t, result)
		if !result.IsError || out.Error == nil || out.Error.Code != "invalid_argument" {
			t.Fatalf("arguments=%v result=%+v output=%+v", arguments, result, out)
		}
	}
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "search_documents", Arguments: map[string]any{"query": "q", "root": otherRoot}})
	if err != nil {
		t.Fatal(err)
	}
	out := documentToolOutput[searchDocumentsData](t, result)
	if result.IsError || out.Data == nil || len(out.Data.Hits) != 0 || gotRoot != otherRoot {
		t.Fatalf("result=%+v output=%+v root=%q", result, out, gotRoot)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, out, err = tools.searchDocuments(ctx, searchDocumentsInput{Query: "q"})
	if err != nil || !result.IsError || out.Error == nil || out.Error.Code != "partial_failure" {
		t.Fatalf("canceled search result=%+v output=%+v error=%v", result, out, err)
	}
}

func TestReadDocumentParagraphContextValidationAndWholeDocumentLimit(t *testing.T) {
	root := newMCPDocumentWorkspace(t)
	dbPath := filepath.Join(root, ".ragrep", "index.db")
	s, err := openStoreAt(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertDoc("notes/guide.md", "first\n\nsecond\n\nthird", 1, func(string) ([]float32, error) { return make([]float32, 768), nil }); err != nil {
		s.Close()
		t.Fatal(err)
	}
	if _, err := s.UpsertDoc("notes/large.md", strings.Repeat("x", 32*1024+1), 1, func(string) ([]float32, error) { return make([]float32, 768), nil }); err != nil {
		s.Close()
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	session := connectDocumentTools(t, documentQueryTools{defaultRoot: root, backend: productionMCPBackend{}})
	paragraph := 1
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "read_document", Arguments: map[string]any{"path": "notes/guide.md", "paragraph": paragraph, "context": 1}})
	if err != nil {
		t.Fatal(err)
	}
	out := documentToolOutput[readDocumentData](t, result)
	if result.IsError || out.Data == nil || out.Data.Path != "notes/guide.md" || out.Data.Paragraph == nil || *out.Data.Paragraph != 1 || out.Data.Context != 1 || out.Data.Content != "first\n\nsecond\n\nthird" {
		t.Fatalf("result=%+v output=%+v", result, out)
	}
	for _, arguments := range []map[string]any{
		{"path": "notes/guide.md", "paragraph": -1},
		{"path": "notes/guide.md", "context": -1},
		{"path": filepath.Join(filepath.Dir(root), "outside.md")},
		{"path": "notes/large.md"},
	} {
		result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "read_document", Arguments: arguments})
		if err != nil {
			t.Fatal(err)
		}
		out := documentToolOutput[readDocumentData](t, result)
		if !result.IsError || out.Error == nil {
			t.Fatalf("arguments=%v result=%+v output=%+v", arguments, result, out)
		}
		if arguments["path"] == "notes/large.md" {
			if out.Error.Code != "invalid_argument" {
				t.Fatalf("large document error=%+v", out.Error)
			}
		} else if arguments["path"] != filepath.Join(filepath.Dir(root), "outside.md") && out.Error.Code != "invalid_argument" {
			t.Fatalf("arguments=%v error=%+v", arguments, out.Error)
		} else if arguments["path"] == filepath.Join(filepath.Dir(root), "outside.md") && out.Error.Code != "path_outside_workspace" {
			t.Fatalf("outside error=%+v", out.Error)
		}
	}
}

func TestDocumentHandlersUseDocumentDBAndReadonlyAnnotations(t *testing.T) {
	root := newMCPDocumentWorkspace(t)
	backend := fakeDocumentQueryBackend{
		search: func(context.Context, mcpWorkspace, searchDocumentsInput) (searchDocumentsData, error) {
			return searchDocumentsData{}, nil
		},
		read: func(context.Context, mcpWorkspace, readDocumentInput) (readDocumentData, error) {
			return readDocumentData{}, nil
		},
	}
	session := connectDocumentTools(t, documentQueryTools{defaultRoot: root, backend: backend})
	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Tools) != 2 {
		t.Fatalf("tools=%+v", listed.Tools)
	}
	for _, tool := range listed.Tools {
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint || tool.Annotations.OpenWorldHint == nil || *tool.Annotations.OpenWorldHint {
			t.Fatalf("tool %q annotations=%+v", tool.Name, tool.Annotations)
		}
	}
}

func TestSearchDocumentsProductionBackendRequiresIndexAndForwardsDaemonRequest(t *testing.T) {
	root := newMCPDocumentWorkspace(t)
	ws := mcpWorkspace{Root: root, DocumentDB: filepath.Join(root, ".ragrep", "index.db")}
	backend := productionMCPBackend{}
	_, err := backend.SearchDocuments(context.Background(), ws, searchDocumentsInput{Query: "q", Mode: "text", Limit: 1})
	var domain *mcpDomainError
	if !errors.As(err, &domain) || domain.Failure.Code != "index_required" {
		t.Fatalf("missing index error=%v", err)
	}
	if err := os.WriteFile(ws.DocumentDB, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	oldFactory := documentDaemonClientFactory
	defer func() { documentDaemonClientFactory = oldFactory }()
	var got documentSearchRequest
	documentDaemonClientFactory = func() (documentDaemonClient, error) {
		return documentDaemonClientFunc(func(_ context.Context, request documentSearchRequest) ([]store.Hit, error) {
			got = request
			return []store.Hit{{Doc: "notes/guide.md", Para: 1, Lines: "2-3", Score: 1, Snippet: "q", Heading: "Guide", Mtime: 1}}, nil
		}), nil
	}
	data, err := backend.SearchDocuments(context.Background(), ws, searchDocumentsInput{Query: "q", Mode: "text", Limit: 1, Tags: []string{"guide"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.DB != ws.DocumentDB || got.Query != "q" || got.Mode != "text" || got.K != 1 || len(got.Tags) != 1 || got.Tags[0] != "guide" || len(data.Hits) != 1 || data.Hits[0].Path != "notes/guide.md" {
		t.Fatalf("request=%+v data=%+v", got, data)
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "mtime") || strings.Contains(string(encoded), ws.DocumentDB) {
		t.Fatalf("public output leaked internal data: %s", encoded)
	}
}

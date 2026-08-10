package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/siroio/ragrep/internal/store"
)

const maxMCPWholeDocumentBytes = 32 * 1024

type searchDocumentsInput struct {
	Query string   `json:"query"`
	Mode  string   `json:"mode,omitempty"`
	Limit int      `json:"limit,omitempty"`
	Tags  []string `json:"tags,omitempty"`
	Root  string   `json:"root,omitempty"`
}

type documentHitOutput struct {
	Path      string  `json:"path"`
	Paragraph int     `json:"paragraph"`
	Heading   string  `json:"heading,omitempty"`
	Lines     string  `json:"lines"`
	Score     float64 `json:"score"`
	Snippet   string  `json:"snippet"`
	Stale     bool    `json:"stale"`
}

type searchDocumentsData struct {
	Hits []documentHitOutput `json:"hits"`
}

type readDocumentInput struct {
	Path      string `json:"path"`
	Paragraph *int   `json:"paragraph,omitempty"`
	Context   int    `json:"context,omitempty"`
	Root      string `json:"root,omitempty"`
}

type readDocumentData struct {
	Path      string `json:"path"`
	Paragraph *int   `json:"paragraph,omitempty"`
	Context   int    `json:"context"`
	Content   string `json:"content"`
}

type documentQueryBackend interface {
	SearchDocuments(context.Context, mcpWorkspace, searchDocumentsInput) (searchDocumentsData, error)
	ReadDocument(context.Context, mcpWorkspace, readDocumentInput) (readDocumentData, error)
}

type documentQueryTools struct {
	defaultRoot string
	backend     documentQueryBackend
}

func registerDocumentQueryTools(server *mcp.Server, tools documentQueryTools) {
	closedWorld := false
	annotations := &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &closedWorld}
	mcp.AddTool(server, &mcp.Tool{
		Name:        "search_documents",
		Description: "Search indexed workspace documents and return paragraph candidates.",
		Annotations: annotations,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input searchDocumentsInput) (*mcp.CallToolResult, mcpToolOutput[searchDocumentsData], error) {
		return tools.searchDocuments(ctx, input)
	})
	mcp.AddTool(server, &mcp.Tool{
		Name:        "read_document",
		Description: "Read an indexed document or a bounded paragraph window.",
		Annotations: annotations,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input readDocumentInput) (*mcp.CallToolResult, mcpToolOutput[readDocumentData], error) {
		return tools.readDocument(ctx, input)
	})
}

func (tools documentQueryTools) searchDocuments(ctx context.Context, input searchDocumentsInput) (*mcp.CallToolResult, mcpToolOutput[searchDocumentsData], error) {
	if err := ctx.Err(); err != nil {
		return mcpToolFailure[searchDocumentsData](err)
	}
	if strings.TrimSpace(input.Query) == "" {
		return mcpToolFailure[searchDocumentsData](mcpInvalidArgument())
	}
	if input.Mode == "" {
		input.Mode = "hybrid"
	}
	if input.Mode != "text" && input.Mode != "vector" && input.Mode != "hybrid" {
		return mcpToolFailure[searchDocumentsData](mcpInvalidArgument())
	}
	if input.Limit == 0 {
		input.Limit = 5
	}
	if input.Limit < 0 {
		return mcpToolFailure[searchDocumentsData](mcpInvalidArgument())
	}
	ws, err := resolveMCPWorkspace(tools.defaultRoot, input.Root)
	if err != nil {
		return mcpToolFailure[searchDocumentsData](err)
	}
	data, err := tools.backend.SearchDocuments(ctx, ws, input)
	if err != nil {
		return mcpToolFailure[searchDocumentsData](err)
	}
	if data.Hits == nil {
		data.Hits = []documentHitOutput{}
	}
	return mcpSuccess(fmt.Sprintf("%d document hits", len(data.Hits)), data)
}

func (tools documentQueryTools) readDocument(ctx context.Context, input readDocumentInput) (*mcp.CallToolResult, mcpToolOutput[readDocumentData], error) {
	if err := ctx.Err(); err != nil {
		return mcpToolFailure[readDocumentData](err)
	}
	if strings.TrimSpace(input.Path) == "" || (input.Paragraph != nil && *input.Paragraph < 0) || input.Context < 0 {
		return mcpToolFailure[readDocumentData](mcpInvalidArgument())
	}
	ws, err := resolveMCPWorkspace(tools.defaultRoot, input.Root)
	if err != nil {
		return mcpToolFailure[readDocumentData](err)
	}
	data, err := tools.backend.ReadDocument(ctx, ws, input)
	if err != nil {
		return mcpToolFailure[readDocumentData](err)
	}
	return mcpSuccess("document read", data)
}

func (productionMCPBackend) SearchDocuments(ctx context.Context, ws mcpWorkspace, input searchDocumentsInput) (searchDocumentsData, error) {
	if _, err := os.Stat(ws.DocumentDB); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return searchDocumentsData{}, mcpIndexRequired()
		}
		return searchDocumentsData{}, err
	}
	client, err := documentDaemonClientFactory()
	if err != nil {
		return searchDocumentsData{}, err
	}
	hits, err := client.SearchDocuments(ctx, documentSearchRequest{
		DB: ws.DocumentDB, Query: input.Query, Mode: input.Mode, K: input.Limit, Tags: input.Tags,
	})
	if err != nil {
		return searchDocumentsData{}, err
	}
	return documentSearchDataFromHits(hits), nil
}

func (productionMCPBackend) ReadDocument(ctx context.Context, ws mcpWorkspace, input readDocumentInput) (readDocumentData, error) {
	if err := ctx.Err(); err != nil {
		return readDocumentData{}, err
	}
	if _, err := os.Stat(ws.DocumentDB); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return readDocumentData{}, mcpIndexRequired()
		}
		return readDocumentData{}, err
	}
	key, err := mcpDocumentKey(input.Path, ws.Root)
	if err != nil {
		return readDocumentData{}, err
	}
	s, err := openStoreAt(ws.DocumentDB)
	if err != nil {
		return readDocumentData{}, err
	}
	defer s.Close()
	paragraph := -1
	if input.Paragraph != nil {
		paragraph = *input.Paragraph
	}
	content, err := getContent(s, key, "", paragraph, input.Context)
	if err != nil {
		return readDocumentData{}, err
	}
	if input.Paragraph == nil && len(content) > maxMCPWholeDocumentBytes {
		return readDocumentData{}, mcpInvalidArgument()
	}
	return readDocumentData{Path: key, Paragraph: input.Paragraph, Context: input.Context, Content: content}, nil
}

func documentSearchDataFromHits(hits []store.Hit) searchDocumentsData {
	data := searchDocumentsData{Hits: make([]documentHitOutput, len(hits))}
	for i, hit := range hits {
		data.Hits[i] = documentHitOutput{
			Path: hit.Doc, Paragraph: hit.Para, Heading: hit.Heading, Lines: hit.Lines,
			Score: hit.Score, Snippet: hit.Snippet, Stale: hit.Stale,
		}
	}
	return data
}

func mcpDocumentKey(path, root string) (string, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	key, err := normPath(path, root)
	if err != nil {
		return "", &mcpDomainError{Failure: mcpFailureForCode("path_outside_workspace")}
	}
	return key, nil
}

func mcpInvalidArgument() error {
	return &mcpDomainError{Failure: mcpFailureForCode("invalid_argument")}
}

func mcpIndexRequired() error {
	return &mcpDomainError{Failure: mcpFailureForCode("index_required")}
}

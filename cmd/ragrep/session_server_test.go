package main

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/siroio/ragrep/internal/store"
)

func readSessionResponse(t *testing.T, r *bufio.Reader) sessionResponse {
	t.Helper()
	line, err := r.ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var response sessionResponse
	if err := json.Unmarshal(line, &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func TestSessionProtocolMalformedRequest(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()
	go serveSessionConnection(server, func(sessionRequest) sessionResponse {
		return sessionResponse{}
	})
	if _, err := client.Write([]byte("{bad json}\n")); err != nil {
		t.Fatal(err)
	}
	response := readSessionResponse(t, bufio.NewReader(client))
	if !strings.Contains(response.Error, "malformed") {
		t.Fatalf("error=%q, want malformed request", response.Error)
	}
}

func TestSessionProtocolUnknownOperation(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()
	go serveSessionConnection(server, func(sessionRequest) sessionResponse {
		return sessionResponse{}
	})
	if _, err := client.Write([]byte(`{"op":"remove"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	response := readSessionResponse(t, bufio.NewReader(client))
	if !strings.Contains(response.Error, "unknown operation") {
		t.Fatalf("error=%q, want unknown operation", response.Error)
	}
}

func TestSessionProtocolWritesOneResponsePerRequestAndPreservesResults(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()
	go serveSessionConnection(server, func(request sessionRequest) sessionResponse {
		switch request.Operation {
		case "search":
			return sessionResponse{Hits: []store.Hit{{Doc: "first.md", Snippet: "first text"}, {Doc: "second.md", Snippet: "second text"}}}
		case "get":
			return sessionResponse{Content: "line one\nline two"}
		default:
			return sessionResponse{Error: "unexpected"}
		}
	})
	if _, err := client.Write([]byte(`{"op":"search"}` + "\n" + `{"op":"get"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(client)
	search := readSessionResponse(t, reader)
	get := readSessionResponse(t, reader)
	if len(search.Hits) != 2 || search.Hits[0].Doc != "first.md" || search.Hits[1].Snippet != "second text" {
		t.Fatalf("search hits=%+v, want ordered preserved hits", search.Hits)
	}
	if get.Content != "line one\nline two" {
		t.Fatalf("get content=%q, want preserved text", get.Content)
	}
}

func TestSessionEndpointRecordIncludesConnectionIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	want := endpointRecord{
		DBPath:           filepath.Join("work", ".ragrep", "index.db"),
		Address:          "127.0.0.1:43123",
		ProtocolVersion:  sessionProtocolVersion,
		ModelFingerprint: "model-sha256",
		Token:            "random-token",
	}
	if err := writeEndpointRecord(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := readEndpointRecord(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("endpoint=%+v, want %+v", got, want)
	}
}

func TestSessionClientRejectsMissingEndpoint(t *testing.T) {
	_, err := requestSession(filepath.Join(t.TempDir(), "missing.json"), "index.db", sessionRequest{Operation: "search"})
	if err == nil || !strings.Contains(err.Error(), "invalid session endpoint") {
		t.Fatalf("missing endpoint error=%v, want clear invalid-session error", err)
	}
}

func TestSessionFlagDoesNotFallBackToOneShot(t *testing.T) {
	db := filepath.Join(t.TempDir(), "unopened", "index.db")
	endpoint := filepath.Join(t.TempDir(), "missing.json")
	for _, args := range [][]string{
		{"search", "--db", db, "--session", endpoint, "alpha"},
		{"get", "--db", db, "--session", endpoint, "a.md"},
	} {
		if code := run(args); code != 1 {
			t.Fatalf("%v: exit=%d, want 1", args, code)
		}
	}
	if _, err := os.Stat(db); !os.IsNotExist(err) {
		t.Fatalf("missing session must not open one-shot db: stat err=%v", err)
	}
}

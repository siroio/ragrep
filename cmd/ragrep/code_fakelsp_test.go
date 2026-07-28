package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeLSPServerSrc is a standalone (no internal/lsp, no internal/lsp/testdata
// -- both are off-limits to modify for this task, and testdata's fake server
// always advertises documentSymbolProvider:true) minimal language server: it
// answers "initialize" with capabilities that deliberately omit
// documentSymbolProvider, and otherwise just drains stdin until killed. It's
// compiled into a real executable per test run (see buildFakeLSPServer) so it
// can be registered as a plain `servers` command string like any other
// language server -- config.Servers has no slot for extra args/env, so a
// re-exec-self-as-subtest trick (as internal/lsp's own tests use) doesn't fit
// here.
const fakeLSPServerSrc = `package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/textproto"
	"os"
	"strconv"
)

func main() {
	tp := textproto.NewReader(bufio.NewReader(os.Stdin))
	for {
		hdr, err := tp.ReadMIMEHeader()
		if err != nil {
			return
		}
		n, err := strconv.Atoi(hdr.Get("Content-Length"))
		if err != nil {
			return
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(tp.R, body); err != nil {
			return
		}
		var msg map[string]json.RawMessage
		if err := json.Unmarshal(body, &msg); err != nil {
			return
		}
		idRaw, hasID := msg["id"]
		var method string
		if m, ok := msg["method"]; ok {
			json.Unmarshal(m, &method)
		}
		if method == "initialize" && hasID {
			resp := fmt.Sprintf("{\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"capabilities\":{\"definitionProvider\":true}}}", string(idRaw))
			fmt.Fprintf(os.Stdout, "Content-Length: %d\r\n\r\n%s", len(resp), resp)
		}
		// "initialized" (a notification) and anything else: no reply is
		// expected or needed for this test's scenario.
	}
}
`

// buildFakeLSPServer compiles fakeLSPServerSrc into a standalone executable
// in dir and returns its path. Skips the test if the "go" toolchain isn't on
// PATH (it always is wherever `go test` itself runs, but this keeps the
// dependency explicit and the test skippable rather than failing somewhere
// unusual).
func buildFakeLSPServer(t *testing.T, dir string) string {
	t.Helper()
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not on PATH; skipping fake-LSP-server test")
	}

	src := filepath.Join(dir, "fakelsp.go")
	if err := os.WriteFile(src, []byte(fakeLSPServerSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	exePath := filepath.Join(dir, "fakelsp.exe")
	cmd := exec.Command(goBin, "build", "-o", exePath, src)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("building fake LSP server: %v: %s", err, stderr.String())
	}
	return exePath
}

// code index must fail clearly, and index nothing, when the configured
// language server doesn't advertise textDocument/documentSymbol support --
// verified against a real (if minimal) LSP-speaking subprocess, not a mock
// of internal/lsp's Client.
func TestCmdCodeIndexCapabilityGate(t *testing.T) {
	root, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	exePath := buildFakeLSPServer(t, root)

	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ragrepDir := filepath.Join(root, ".ragrep")
	if err := os.MkdirAll(ragrepDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgJSON, err := json.Marshal(map[string]any{"servers": map[string]string{"go": exePath}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ragrepDir, "config.json"), cfgJSON, 0o644); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(ragrepDir, "code.db")

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	code := run([]string{"code", "index", "--db", db, "--language", "go", root})
	w.Close()
	os.Stderr = old
	var buf bytes.Buffer
	buf.ReadFrom(r)

	if code != 1 {
		t.Fatalf("capability gate: exit=%d, want 1, stderr=%q", code, buf.String())
	}
	if !strings.Contains(buf.String(), "documentSymbol") {
		t.Fatalf("stderr=%q, want a clear message naming the missing documentSymbol capability", buf.String())
	}

	// The capability check happens before the codestore is ever opened, so
	// nothing should have been indexed -- not even an empty code.db file.
	if _, err := os.Stat(db); !os.IsNotExist(err) {
		t.Fatalf("code.db must not be created when the capability gate fails, stat err=%v", err)
	}
}

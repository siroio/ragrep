package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/siroio/ragrep/internal/codeindex"
	"github.com/siroio/ragrep/internal/codestore"
	"github.com/siroio/ragrep/internal/embed"
	"github.com/siroio/ragrep/internal/store"
)

func TestSmokeMCP(t *testing.T) {
	cache, err := embed.CacheDir()
	if err != nil {
		t.Fatal(err)
	}
	if !embed.ModelCached(cache) {
		t.Skip("model not cached; run 'ragrep init' to enable the MCP mutation smoke test")
	}
	packageDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(t.TempDir(), "ragrep-mcp-smoke")
	if runtime.GOOS == "windows" {
		executable += ".exe"
	}
	build := exec.Command("go", "build", "-ldflags", "-X=main.daemonBindAddress=127.0.0.1:0", "-o", executable, ".")
	build.Dir = packageDir
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build MCP smoke binary: %v\n%s", err, output)
	}

	environmentRoot := t.TempDir()
	environment := isolatedDaemonEnvironment(environmentRoot)
	if !copyDaemonSmokeEmbedCache(t, filepath.Join(environmentRoot, "cache", "ragrep")) {
		t.Fatal("host model disappeared before MCP smoke")
	}
	root := daemonSmokeWorkspace(t, "func MCPFixture() {}\n")
	fakeLSP := buildFakeLSPServerFromSrc(t, t.TempDir(), "fakelsp-mcp-smoke", fakeLSPServerAllCapsEmptySrc)
	configData, err := json.Marshal(map[string]any{"servers": map[string]string{"go": fakeLSP}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".ragrep", "config.json"), configData, 0o600); err != nil {
		t.Fatal(err)
	}
	codeDB := filepath.Join(root, ".ragrep", "code.db")
	symbol := testSymbol()
	symbol.Path = "service.go"
	symbol.Body = "func MCPFixture() {}\n"
	symbol.BodyHash = codeindex.FileHash([]byte(symbol.Body))
	symbol.EmbeddingText = codeindex.RenderEmbeddingText(symbol)
	codeStore, err := codestore.Open(codeDB, codeModelID, codeEmbedDim)
	if err != nil {
		t.Fatal(err)
	}
	fileContent, err := os.ReadFile(filepath.Join(root, "service.go"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := codeStore.UpsertSymbols(symbol.Path, codeindex.FileHash(fileContent), []codeindex.Symbol{symbol}, 0, fakeCodeEmbed); err != nil {
		codeStore.Close()
		t.Fatal(err)
	}
	if err := codeStore.Close(); err != nil {
		t.Fatal(err)
	}

	discoveryPath := daemonSmokeDiscoveryPath(environmentRoot, runtime.GOOS)
	stdout, stderr, err := runBuiltRagrep(executable, packageDir, environment, "daemon", "start")
	if err != nil {
		t.Fatalf("daemon start: %v: %s", err, stderr)
	}
	daemonPID, err := strconv.Atoi(strings.TrimSpace(stdout))
	if err != nil {
		t.Fatal(err)
	}
	discovery, err := readDaemonDiscovery(discoveryPath)
	if err != nil {
		t.Fatal(err)
	}
	address := strings.TrimPrefix(discovery.Endpoint, "http://")
	stopped := false
	t.Cleanup(func() {
		if stopped {
			return
		}
		_, _, _ = runBuiltRagrep(executable, packageDir, environment, "daemon", "stop")
		_ = waitForDaemonSmokeCleanup(daemonPID, address, 3*time.Second)
	})
	if _, stderr, err := runBuiltRagrep(executable, packageDir, environment, "workspace", "add", root); err != nil {
		t.Fatalf("workspace add: %v: %s", err, stderr)
	}

	command := exec.Command(executable, "mcp", "serve")
	command.Dir, command.Env = root, environment
	var mcpStderr bytes.Buffer
	command.Stderr = &mcpStderr
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "ragrep-smoke", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: command, TerminateDuration: time.Second}, nil)
	if err != nil {
		t.Fatalf("MCP connect: %v: %s", err, mcpStderr.String())
	}
	t.Cleanup(func() {
		_ = session.Close()
		deadline := time.Now().Add(2 * time.Second)
		for command.ProcessState == nil && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if command.ProcessState == nil {
			_ = command.Process.Kill()
		}
	})
	callMCPTool(t, ctx, session, "add_document", map[string]any{"path": "notes/mcp.md", "content": "old MCP marker"})
	search := callMCPTool(t, ctx, session, "search_documents", map[string]any{"query": "old MCP marker", "mode": "text"})
	var searched mcpToolOutput[searchDocumentsData]
	decodeMCPStructured(t, search, &searched)
	if searched.Data == nil || len(searched.Data.Hits) != 1 {
		t.Fatalf("document search=%+v", searched)
	}
	callMCPTool(t, ctx, session, "read_document", map[string]any{"path": "notes/mcp.md"})
	if err := os.WriteFile(filepath.Join(root, "notes", "mcp.md"), []byte("new MCP marker"), 0o644); err != nil {
		t.Fatal(err)
	}
	callMCPTool(t, ctx, session, "reindex_documents", map[string]any{"paths": []string{"notes/mcp.md"}})
	callMCPTool(t, ctx, session, "search_documents", map[string]any{"query": "new MCP marker", "mode": "text"})

	codeSearch := callMCPTool(t, ctx, session, "search_code", map[string]any{"query": "MCPFixture", "mode": "text"})
	var codeHits mcpToolOutput[searchCodeData]
	decodeMCPStructured(t, codeSearch, &codeHits)
	if codeHits.Data == nil || len(codeHits.Data.Hits) == 0 {
		t.Fatalf("code search=%+v", codeHits)
	}
	key := codeHits.Data.Hits[0].Key
	callMCPTool(t, ctx, session, "read_code_symbol", map[string]any{"key": key})
	callMCPTool(t, ctx, session, "inspect_code_relation", map[string]any{"key": key, "relation": "tests"})
	contextResult := callMCPTool(t, ctx, session, "build_code_context", map[string]any{"query": "MCPFixture", "selected_keys": []string{key}})
	var built mcpToolOutput[buildCodeContextData]
	decodeMCPStructured(t, contextResult, &built)
	if built.Data == nil {
		t.Fatal("build_code_context returned no data")
	}
	callMCPTool(t, ctx, session, "verify_code_context", map[string]any{"manifest": built.Data.Manifest})

	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if !daemonSmokeProcessRunning(daemonPID) {
		t.Fatal("closing MCP stopped the shared daemon")
	}
	stopDaemonDocumentSmoke(t, executable, packageDir, environment, discoveryPath, daemonPID, address)
	stopped = true
	if mcpStderr.Len() != 0 {
		t.Fatalf("MCP stderr=%q", mcpStderr.String())
	}
}

func callMCPTool(t *testing.T, ctx context.Context, session *mcp.ClientSession, name string, arguments any) *mcp.CallToolResult {
	t.Helper()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if result.IsError {
		encoded, _ := json.Marshal(result)
		t.Fatalf("%s returned tool error: %s", name, encoded)
	}
	return result
}

func decodeMCPStructured(t *testing.T, result *mcp.CallToolResult, destination any) {
	t.Helper()
	data, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, destination); err != nil {
		t.Fatalf("decode structured result: %v: %s", err, data)
	}
}

// End-to-end: index a small corpus and search it with the real model.
// Skips when model assets are not cached (run 'ragrep init' first).
func TestSmoke(t *testing.T) {
	dir, err := embed.CacheDir()
	if err != nil {
		t.Fatal(err)
	}
	if !embed.ModelCached(dir) {
		t.Skip("model not cached; run 'ragrep init' to enable this test")
	}
	injectDocumentDaemonClient(t, directDocumentSearchClient(), nil)

	tmp := t.TempDir()
	corpus := filepath.Join(tmp, "docs")
	os.MkdirAll(corpus, 0o755)
	os.WriteFile(filepath.Join(corpus, "auth.md"), []byte(
		"認証エラー一覧。\nERR_AUTH_104 はトークン期限切れ。\n\n再認証の手順はログイン画面から行う。\n"), 0o644)
	os.WriteFile(filepath.Join(corpus, "net.md"), []byte(
		"ネットワーク設定。\nプロキシはenvで指定する。\n"), 0o644)

	// A doc outside the "docs" root, indexed alongside it, to prove --prune
	// only touches paths under the pruned root(s).
	other := filepath.Join(tmp, "other")
	os.MkdirAll(other, 0o755)
	os.WriteFile(filepath.Join(other, "keep.md"), []byte("キープ対象のドキュメント。\n"), 0o644)

	db := filepath.Join(tmp, "index.db")
	wd, _ := os.Getwd()
	os.Chdir(tmp)
	defer os.Chdir(wd)

	if code := run([]string{"index", "--db", db, "docs", "other"}); code != 0 {
		t.Fatalf("index exit=%d", code)
	}
	if code := run([]string{"search", "--db", db, "--json", "ERR_AUTH_104"}); code != 0 {
		t.Fatalf("search exit=%d", code)
	}
	if code := run([]string{"search", "--db", db, "--mode", "text", "存在しない謎の文字列XYZQ"}); code != 2 {
		t.Fatalf("no-hit search exit=%d, want 2", code)
	}
	if code := run([]string{"get", "--db", db, "--para", "0", "--context", "1", "docs/auth.md"}); code != 0 {
		t.Fatalf("get exit=%d", code)
	}
	if code := run([]string{"get", "--db", db, "docs/missing.md"}); code != 2 {
		t.Fatalf("get missing exit=%d, want 2", code)
	}
	// Windows-style separators in the get path arg must still resolve to the
	// "docs/auth.md" key stored (with ToSlash) at index time.
	if code := run([]string{"get", "--db", db, `docs\auth.md`}); code != 0 {
		t.Fatalf("get backslash-path exit=%d, want 0", code)
	}

	// --prune: remove indexed docs under the given root(s) that no longer
	// exist on disk, leaving docs outside the pruned root untouched.
	if err := os.Remove(filepath.Join(corpus, "auth.md")); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"index", "--prune", "--db", db, "docs"}); code != 0 {
		t.Fatalf("index --prune exit=%d", code)
	}
	if code := run([]string{"get", "--db", db, "docs/auth.md"}); code != 2 {
		t.Fatalf("get pruned doc exit=%d, want 2", code)
	}
	if code := run([]string{"get", "--db", db, "docs/net.md"}); code != 0 {
		t.Fatalf("get untouched doc in pruned root exit=%d, want 0", code)
	}
	if code := run([]string{"get", "--db", db, "other/keep.md"}); code != 0 {
		t.Fatalf("get doc outside pruned root exit=%d, want 0", code)
	}
}

// A malformed flag must fail with exit 1 (this CLI's generic error code), not
// flag package's own exit 2 -- which would collide with the "no hits / not
// found" contract. Doesn't need the model: flag parsing fails before the
// embedder or DB are ever touched.
func TestUnknownFlagExitsOne(t *testing.T) {
	if code := run([]string{"search", "--bogusflag", "x"}); code != 1 {
		t.Fatalf("unknown flag exit=%d, want 1", code)
	}
}

func TestDaemonSmokeProcessRunningDetectsExitedProcess(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows process lookup regression")
	}
	command := exec.Command("cmd", "/c", "exit", "0")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	pid := command.Process.Pid
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}
	if daemonSmokeProcessRunning(pid) {
		t.Fatalf("exited PID %d is still reported as running", pid)
	}
}

func TestSmokeDaemonLiveSearchIsFreshAndIsolated(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatal("go toolchain is required for daemon smoke test")
	}
	packageDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	exeName := "ragrep-smoke"
	if runtime.GOOS == "windows" {
		exeName += ".exe"
	}
	executable := filepath.Join(t.TempDir(), exeName)
	build := exec.Command(goBin, "build", "-ldflags", "-X=main.daemonBindAddress=127.0.0.1:0", "-o", executable, ".")
	build.Dir = packageDir
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build daemon: %v\n%s", err, output)
	}

	environmentRoot := t.TempDir()
	environment := isolatedDaemonEnvironment(environmentRoot)
	firstRoot := daemonSmokeWorkspace(t, "func OldWorkspaceSymbol() {}\n")
	secondRoot := daemonSmokeWorkspace(t, "func SecondWorkspaceOnly() {}\n")
	var daemonPID int
	var daemonEndpointAddress string
	stopped := false
	discoveryPath := daemonSmokeDiscoveryPath(environmentRoot, runtime.GOOS)
	t.Cleanup(func() {
		if stopped {
			return
		}
		if discovery, discoveryErr := readDaemonDiscovery(discoveryPath); discoveryErr == nil {
			if daemonPID == 0 {
				daemonPID = discovery.PID
			}
			if daemonEndpointAddress == "" {
				daemonEndpointAddress = strings.TrimPrefix(discovery.Endpoint, "http://")
			}
		}
		_, stderr, err := runBuiltRagrep(executable, packageDir, environment, "daemon", "stop")
		if err != nil && daemonPID > 0 {
			if process, findErr := os.FindProcess(daemonPID); findErr == nil {
				_ = process.Kill()
			}
		}
		if daemonPID > 0 && daemonEndpointAddress != "" {
			if waitErr := waitForDaemonSmokeCleanup(daemonPID, daemonEndpointAddress, 3*time.Second); waitErr != nil {
				t.Errorf("daemon cleanup wait: %v", waitErr)
			}
		}
		if err != nil {
			t.Errorf("daemon cleanup: %v: %s", err, stderr)
		}
	})

	stdout, stderr, err := runBuiltRagrep(executable, packageDir, environment, "daemon", "start")
	if err != nil {
		t.Fatalf("daemon start: %v: %s", err, stderr)
	}
	daemonPID, err = strconv.Atoi(strings.TrimSpace(stdout))
	if err != nil || daemonPID <= 0 {
		t.Fatalf("daemon PID=%q err=%v", stdout, err)
	}
	discovery, err := readDaemonDiscovery(discoveryPath)
	if err != nil {
		t.Fatal(err)
	}
	daemonEndpointAddress = strings.TrimPrefix(discovery.Endpoint, "http://")
	host, portText, err := net.SplitHostPort(daemonEndpointAddress)
	if err != nil {
		t.Fatalf("daemon endpoint=%q: %v", discovery.Endpoint, err)
	}
	port, err := strconv.Atoi(portText)
	if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() || port == 0 || daemonEndpointAddress == daemonAddress {
		t.Fatalf("daemon endpoint=%q host=%q port=%d err=%v, want allocated loopback", discovery.Endpoint, host, port, err)
	}
	secondStart, stderr, err := runBuiltRagrep(executable, packageDir, environment, "daemon", "start")
	if err != nil || strings.TrimSpace(secondStart) != strconv.Itoa(daemonPID) {
		t.Fatalf("second start PID=%q want=%d err=%v stderr=%s", secondStart, daemonPID, err, stderr)
	}
	for _, root := range []string{firstRoot, secondRoot} {
		if _, stderr, err := runBuiltRagrep(executable, packageDir, environment, "workspace", "add", root); err != nil {
			t.Fatalf("workspace add %s: %v: %s", root, err, stderr)
		}
	}

	initial := daemonSmokeSearch(t, executable, firstRoot, environment, "OldWorkspaceSymbol")
	if !initial.Fresh || !daemonSmokeHasPath(initial.Hits, "service.go") {
		t.Fatalf("initial search=%+v", initial)
	}
	second := daemonSmokeSearch(t, executable, secondRoot, environment, "SecondWorkspaceOnly")
	if !second.Fresh || !daemonSmokeHasPath(second.Hits, "service.go") {
		t.Fatalf("second workspace search=%+v", second)
	}

	visibilitySamples := make([]time.Duration, 5)
	for i := range visibilitySamples {
		newSymbol := fmt.Sprintf("NewWorkspaceSymbol%d", i)
		untrackedSymbol := fmt.Sprintf("AddedUntrackedSymbol%d", i)
		started := time.Now()
		if err := os.WriteFile(filepath.Join(firstRoot, "service.go"), []byte("package sample\nfunc "+newSymbol+"() {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(firstRoot, "untracked.go"), []byte("package sample\nfunc "+untrackedSymbol+"() {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		updated := daemonSmokeSearch(t, executable, firstRoot, environment, newSymbol+" "+untrackedSymbol)
		visibilitySamples[i] = time.Since(started)
		if !updated.Fresh || !daemonSmokeHasPath(updated.Hits, "service.go") || !daemonSmokeHasPath(updated.Hits, "untracked.go") {
			t.Fatalf("sample %d updated search=%+v", i, updated)
		}
	}
	if p95 := durationP95(visibilitySamples); p95 > 500*time.Millisecond {
		t.Fatalf("save visibility p95=%v, want <=500ms (samples=%v)", p95, visibilitySamples)
	}
	if _, stderr, err := runBuiltRagrep(executable, firstRoot, environment, "code", "search", "--mode", "auto", "--json", "-k", "5", "OldWorkspaceSymbol"); daemonSmokeExitCode(err) != 2 {
		t.Fatalf("old symbol search err=%v stderr=%s, want exit 2", err, stderr)
	}
	if _, stderr, err := runBuiltRagrep(executable, firstRoot, environment, "code", "search", "--mode", "auto", "--json", "-k", "5", "SecondWorkspaceOnly"); daemonSmokeExitCode(err) != 2 {
		t.Fatalf("cross-workspace search err=%v stderr=%s, want exit 2", err, stderr)
	}

	if _, stderr, err := runBuiltRagrep(executable, packageDir, environment, "daemon", "stop"); err != nil {
		t.Fatalf("daemon stop: %v: %s", err, stderr)
	}
	if err := waitForDaemonSmokeCleanup(daemonPID, daemonEndpointAddress, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	stopped = true
	t.Logf("save visibility p95: %v (samples=%v)", durationP95(visibilitySamples), visibilitySamples)
}

func TestSmokeDaemonDocument(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatal("go toolchain is required for daemon smoke test")
	}
	packageDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	exeName := "ragrep-document-daemon-smoke"
	if runtime.GOOS == "windows" {
		exeName += ".exe"
	}
	executable := filepath.Join(t.TempDir(), exeName)
	build := exec.Command(goBin, "build", "-ldflags", "-X=main.daemonBindAddress=127.0.0.1:0", "-o", executable, ".")
	build.Dir = packageDir
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build daemon: %v\n%s", err, output)
	}

	environmentRoot := t.TempDir()
	environment := isolatedDaemonEnvironment(environmentRoot)
	isolatedEmbedCache := filepath.Join(environmentRoot, "cache", "ragrep")
	hostModelCached := copyDaemonSmokeEmbedCache(t, isolatedEmbedCache)
	firstContent := "alpha unique document phrase"
	secondContent := "beta isolated document phrase"
	firstRoot, firstDB := daemonDocumentSmokeWorkspace(t, firstContent)
	secondRoot, secondDB := daemonDocumentSmokeWorkspace(t, secondContent)
	if hostModelCached {
		for _, workspace := range []struct {
			root string
			db   string
		}{
			{firstRoot, firstDB},
			{secondRoot, secondDB},
		} {
			if _, stderr, err := runBuiltRagrep(executable, workspace.root, environment, "index", "--db", workspace.db, "docs"); err != nil {
				t.Fatalf("direct index %s: %v: %s", workspace.root, err, stderr)
			}
		}
	} else {
		seedDaemonDocumentSmokeDB(t, firstDB, firstContent)
		seedDaemonDocumentSmokeDB(t, secondDB, secondContent)
		t.Log("host model is unavailable; seeded text fixtures and will skip hybrid checks")
	}
	if err := os.RemoveAll(isolatedEmbedCache); err != nil {
		t.Fatal(err)
	}
	if embed.ModelCached(isolatedEmbedCache) {
		t.Fatal("isolated cache still contains a model before the first text search")
	}

	discoveryPath := daemonSmokeDiscoveryPath(environmentRoot, runtime.GOOS)
	var daemonPID int
	var daemonEndpointAddress string
	stopped := false
	t.Cleanup(func() {
		if stopped {
			return
		}
		if discovery, err := readDaemonDiscovery(discoveryPath); err == nil {
			daemonPID = discovery.PID
			daemonEndpointAddress = strings.TrimPrefix(discovery.Endpoint, "http://")
		}
		_, _, _ = runBuiltRagrep(executable, packageDir, environment, "daemon", "stop")
		if daemonPID > 0 && daemonEndpointAddress != "" {
			if err := waitForDaemonSmokeCleanup(daemonPID, daemonEndpointAddress, 3*time.Second); err != nil {
				t.Errorf("daemon cleanup: %v", err)
			}
		}
	})

	first := daemonDocumentSmokeSearch(t, executable, firstRoot, environment, firstDB, "text", "alpha unique")
	if !daemonSmokeHasDocument(first, "docs/document.md") {
		t.Fatalf("first text search=%+v", first)
	}
	t.Log("text search auto-started the daemon with an empty isolated model cache")
	discovery, err := readDaemonDiscovery(discoveryPath)
	if err != nil {
		t.Fatal(err)
	}
	daemonPID = discovery.PID
	daemonEndpointAddress = strings.TrimPrefix(discovery.Endpoint, "http://")
	if !hostModelCached {
		stopDaemonDocumentSmoke(t, executable, packageDir, environment, discoveryPath, daemonPID, daemonEndpointAddress)
		stopped = true
		return
	}
	if !copyDaemonSmokeEmbedCache(t, isolatedEmbedCache) {
		t.Fatal("host model disappeared before hybrid checks")
	}

	firstHybrid := daemonDocumentSmokeSearch(t, executable, firstRoot, environment, firstDB, "hybrid", "alpha unique document phrase")
	if !daemonSmokeHasDocument(firstHybrid, "docs/document.md") {
		t.Fatalf("first hybrid search=%+v", firstHybrid)
	}
	secondHybrid := daemonDocumentSmokeSearch(t, executable, secondRoot, environment, secondDB, "hybrid", "beta isolated document phrase")
	if !daemonSmokeHasDocument(secondHybrid, "docs/document.md") || daemonSmokeHasSnippet(secondHybrid, "alpha unique") {
		t.Fatalf("second hybrid search=%+v", secondHybrid)
	}

	if _, stderr, err := runBuiltRagrepInput(executable, firstRoot, environment, "direct add marker", "add", "--db", firstDB, "notes/direct.md"); err != nil {
		t.Fatalf("direct add: %v: %s", err, stderr)
	}
	added := daemonDocumentSmokeSearch(t, executable, firstRoot, environment, firstDB, "text", "direct add marker")
	if !daemonSmokeHasDocument(added, "notes/direct.md") {
		t.Fatalf("direct add visibility=%+v", added)
	}
	if err := os.WriteFile(filepath.Join(firstRoot, "docs", "later.md"), []byte("direct index marker"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, stderr, err := runBuiltRagrep(executable, firstRoot, environment, "index", "--db", firstDB, "docs"); err != nil {
		t.Fatalf("direct reindex: %v: %s", err, stderr)
	}
	indexed := daemonDocumentSmokeSearch(t, executable, firstRoot, environment, firstDB, "text", "direct index marker")
	if !daemonSmokeHasDocument(indexed, "docs/later.md") {
		t.Fatalf("direct index visibility=%+v", indexed)
	}

	stopDaemonDocumentSmoke(t, executable, packageDir, environment, discoveryPath, daemonPID, daemonEndpointAddress)
	stopped = true
}

func stopDaemonDocumentSmoke(t *testing.T, executable, packageDir string, environment []string, discoveryPath string, daemonPID int, daemonEndpointAddress string) {
	t.Helper()
	if _, stderr, err := runBuiltRagrep(executable, packageDir, environment, "daemon", "stop"); err != nil {
		t.Fatalf("daemon stop: %v: %s", err, stderr)
	}
	if err := waitForDaemonSmokeCleanup(daemonPID, daemonEndpointAddress, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(discoveryPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("discovery remains after stop: %v", err)
	}
}

func daemonDocumentSmokeWorkspace(t *testing.T, content string) (string, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".ragrep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "docs", "document.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, filepath.Join(root, ".ragrep", "index.db")
}

func copyDaemonSmokeEmbedCache(t *testing.T, destination string) bool {
	t.Helper()
	source, err := embed.CacheDir()
	if err != nil {
		t.Fatal(err)
	}
	if !embed.ModelCached(source) {
		return false
	}
	if err := filepath.WalkDir(source, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !d.IsDir() && filepath.Base(path) == "daemon.json" {
			return nil
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o755)
	}); err != nil {
		t.Fatal(err)
	}
	return true
}

func seedDaemonDocumentSmokeDB(t *testing.T, db, content string) {
	t.Helper()
	s, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.UpsertDoc("docs/document.md", content, 1, func(string) ([]float32, error) {
		return make([]float32, 768), nil
	}); err != nil {
		t.Fatal(err)
	}
}

func daemonDocumentSmokeSearch(t *testing.T, executable, root string, environment []string, db, mode, query string) []store.Hit {
	t.Helper()
	stdout, stderr, err := runBuiltRagrep(executable, root, environment, "search", "--db", db, "--mode", mode, "--json", query)
	if err != nil {
		t.Fatalf("document search %q: %v: %s", query, err, stderr)
	}
	var hits []store.Hit
	if err := json.Unmarshal([]byte(stdout), &hits); err != nil {
		t.Fatalf("decode document search %q: %v: %s", query, err, stdout)
	}
	return hits
}

func daemonSmokeHasDocument(hits []store.Hit, document string) bool {
	for _, hit := range hits {
		if filepath.ToSlash(hit.Doc) == document {
			return true
		}
	}
	return false
}

func daemonSmokeHasSnippet(hits []store.Hit, fragment string) bool {
	for _, hit := range hits {
		if strings.Contains(hit.Snippet, fragment) {
			return true
		}
	}
	return false
}

type daemonSmokeSearchResult struct {
	Hits       []codestore.SymbolHit `json:"hits"`
	Fresh      bool                  `json:"fresh"`
	Generation uint64                `json:"generation"`
}

func daemonSmokeWorkspace(t *testing.T, body string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".ragrep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "service.go"), []byte("package sample\n"+body), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestSmokeDaemonIsolationCoversDarwinUserDirs(t *testing.T) {
	root := t.TempDir()
	values := make(map[string]string)
	for _, value := range isolatedDaemonEnvironment(root) {
		name, value, ok := strings.Cut(value, "=")
		if ok {
			values[strings.ToUpper(name)] = value
		}
	}
	wantHome := filepath.Join(root, "home")
	if values["HOME"] != wantHome {
		t.Fatalf("HOME=%q, want %q", values["HOME"], wantHome)
	}
	wantDiscovery := filepath.Join(wantHome, "Library", "Caches", "ragrep", "daemon.json")
	if got := daemonSmokeDiscoveryPath(root, "darwin"); got != wantDiscovery {
		t.Fatalf("darwin discovery=%q, want %q", got, wantDiscovery)
	}
}

func daemonSmokeDiscoveryPath(root, goos string) string {
	cache := filepath.Join(root, "cache")
	if goos == "darwin" {
		cache = filepath.Join(root, "home", "Library", "Caches")
	}
	return filepath.Join(cache, "ragrep", "daemon.json")
}

func isolatedDaemonEnvironment(root string) []string {
	names := map[string]bool{"APPDATA": true, "LOCALAPPDATA": true, "XDG_CACHE_HOME": true, "XDG_CONFIG_HOME": true, "HOME": true}
	environment := make([]string, 0, len(os.Environ())+5)
	for _, value := range os.Environ() {
		name, _, _ := strings.Cut(value, "=")
		if !names[strings.ToUpper(name)] {
			environment = append(environment, value)
		}
	}
	return append(environment,
		"APPDATA="+filepath.Join(root, "config"),
		"LOCALAPPDATA="+filepath.Join(root, "cache"),
		"XDG_CACHE_HOME="+filepath.Join(root, "cache"),
		"XDG_CONFIG_HOME="+filepath.Join(root, "config"),
		"HOME="+filepath.Join(root, "home"),
	)
}

func runBuiltRagrep(executable, directory string, environment []string, args ...string) (string, string, error) {
	command := exec.Command(executable, args...)
	command.Dir = directory
	command.Env = environment
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	return stdout.String(), stderr.String(), err
}

func runBuiltRagrepInput(executable, directory string, environment []string, input string, args ...string) (string, string, error) {
	command := exec.Command(executable, args...)
	command.Dir = directory
	command.Env = environment
	command.Stdin = strings.NewReader(input)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	return stdout.String(), stderr.String(), err
}

func waitForDaemonSmokeCleanup(pid int, address string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		running := daemonSmokeProcessRunning(pid)
		listener, listenErr := net.Listen("tcp", address)
		if listenErr == nil {
			_ = listener.Close()
		}
		if !running && listenErr == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("daemon cleanup timed out: pid=%d running=%v address=%s listen=%v", pid, running, address, listenErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func daemonSmokeProcessRunning(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	defer process.Release()
	if runtime.GOOS == "windows" {
		output, err := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/FO", "CSV", "/NH").Output()
		return err == nil && strings.Contains(string(output), fmt.Sprintf("\"%d\"", pid))
	}
	err = process.Signal(syscall.Signal(0))
	return err == nil || !errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.ESRCH)
}

func daemonSmokeSearch(t *testing.T, executable, root string, environment []string, query string) daemonSmokeSearchResult {
	t.Helper()
	stdout, stderr, err := runBuiltRagrep(executable, root, environment, "code", "search", "--mode", "auto", "--json", "-k", "5", query)
	if err != nil {
		t.Fatalf("code search %q: %v: %s", query, err, stderr)
	}
	var result daemonSmokeSearchResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("decode search %q: %v: %s", query, err, stdout)
	}
	return result
}

func daemonSmokeHasPath(hits []codestore.SymbolHit, path string) bool {
	for _, hit := range hits {
		if filepath.ToSlash(hit.Path) == path {
			return true
		}
	}
	return false
}

func daemonSmokeExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

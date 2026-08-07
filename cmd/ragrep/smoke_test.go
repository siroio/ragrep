package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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

	"github.com/siroio/ragrep/internal/codestore"
	"github.com/siroio/ragrep/internal/embed"
)

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
		return true
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

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/siroio/ragrep/internal/store"
)

func TestDocumentConverterSnapshotHelper(t *testing.T) {
	if os.Getenv("GO_WANT_DOCUMENT_CONVERTER") != "1" {
		return
	}
	input := os.Args[len(os.Args)-1]
	if record := os.Getenv("DOCUMENT_CONVERTER_RECORD"); record != "" {
		if err := os.WriteFile(record, []byte(input), 0o600); err != nil {
			os.Exit(4)
		}
	}
	if os.Getenv("DOCUMENT_CONVERTER_SLEEP") == "1" {
		time.Sleep(5 * time.Second)
		os.Exit(0)
	}
	data, err := os.ReadFile(input)
	if err != nil {
		os.Exit(5)
	}
	fmt.Printf("input=%s;ext=%s;data=%s", input, filepath.Ext(input), data)
	fmt.Fprintln(os.Stderr, "stderr-input="+input)
	os.Exit(0)
}

func documentConverterTestArgv() []string {
	return []string{os.Args[0], "-test.run=TestDocumentConverterSnapshotHelper", "--", "{input}"}
}

func mutationErrorCode(err error) string {
	var public *mcpSafePublicError
	if errors.As(err, &public) {
		return public.failure().Code
	}
	var domain *mcpDomainError
	if errors.As(err, &domain) {
		return domain.Failure.Code
	}
	return ""
}

func newDocumentMutationWorkspace(t *testing.T) (root, db string) {
	t.Helper()
	root = t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".ragrep"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root, filepath.Join(root, ".ragrep", "index.db")
}

func makeDocumentDirectoryLink(t *testing.T, link, target string) {
	t.Helper()
	if err := os.Symlink(target, link); err == nil {
		return
	} else if runtime.GOOS != "windows" {
		t.Skipf("host denied link creation: %v", err)
	}
	if output, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Skipf("host denied symlink and junction creation: %v (%s)", err, strings.TrimSpace(string(output)))
	}
}

func TestResolveDocumentWritePathRejectsUnsafePaths(t *testing.T) {
	root, _ := newDocumentMutationWorkspace(t)
	wantAbsolute := filepath.Join(root, "notes", "guide.md")
	absolute, key, err := resolveDocumentWritePath(root, filepath.Join("notes", "guide.md"))
	if err != nil || absolute != wantAbsolute || key != "notes/guide.md" {
		t.Fatalf("safe path = %q, %q, %v; want %q, notes/guide.md, nil", absolute, key, err, wantAbsolute)
	}

	cases := []struct {
		name, path, code string
	}{
		{"absolute", wantAbsolute, "invalid_argument"},
		{"traversal", filepath.Join("..", "outside.md"), "path_outside_workspace"},
		{"code extension", filepath.Join("notes", "unsafe.go"), "invalid_argument"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := resolveDocumentWritePath(root, tc.path); mutationErrorCode(err) != tc.code {
				t.Fatalf("resolveDocumentWritePath(%q) error = %v (code %q), want %q", tc.path, err, mutationErrorCode(err), tc.code)
			}
		})
	}
}

func TestResolveDocumentWritePathRejectsOutsideRootLink(t *testing.T) {
	root, _ := newDocumentMutationWorkspace(t)
	outside := t.TempDir()
	link := filepath.Join(root, "linked")
	makeDocumentDirectoryLink(t, link, outside)
	_, _, err := resolveDocumentWritePath(root, filepath.Join("linked", "outside.md"))
	if got := mutationErrorCode(err); got != "path_outside_workspace" {
		t.Fatalf("linked outside path error = %v (code %q), want path_outside_workspace", err, got)
	}
}

func TestRunAddDocumentRejectsInvalidTagsAndFinalSize(t *testing.T) {
	root, db := newDocumentMutationWorkspace(t)
	for _, tag := range []string{"two,tags", "closing]", "line\rbreak", "line\nbreak"} {
		path := filepath.Join("notes", strings.NewReplacer(",", "-", "]", "-", "\r", "-", "\n", "-").Replace(tag)+".md")
		_, err := runAddDocument(context.Background(), addDocumentRequest{DB: db, Root: root, Path: path, Content: "body", Tags: []string{tag}}, addDocumentDeps{})
		if got := mutationErrorCode(err); got != "invalid_argument" {
			t.Fatalf("tag %q error = %v (code %q), want invalid_argument", tag, err, got)
		}
		if _, err := os.Stat(filepath.Join(root, path)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("invalid tag %q created a file: %v", tag, err)
		}
	}

	path := filepath.Join("notes", "too-large.md")
	_, err := runAddDocument(context.Background(), addDocumentRequest{
		DB: db, Root: root, Path: path, Content: strings.Repeat("x", maxFileSize), Tags: []string{"tag"},
	}, addDocumentDeps{})
	if got := mutationErrorCode(err); got != "invalid_argument" {
		t.Fatalf("final-size error = %v (code %q), want invalid_argument", err, got)
	}
	if _, err := os.Stat(filepath.Join(root, path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("oversized final document created a file: %v", err)
	}
}

func TestRunAddDocumentIndexesAndPreservesExistingFrontmatter(t *testing.T) {
	root, db := newDocumentMutationWorkspace(t)
	content := "---\ntags: [Existing, guide]\n---\n\nfirst\n\nsecond"
	result, err := runAddDocument(context.Background(), addDocumentRequest{
		DB: db, Root: root, Path: "notes/guide.md", Content: content, Tags: []string{"ignored"},
	}, addDocumentDeps{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Path != "notes/guide.md" || result.Paragraphs != 2 || strings.Join(result.Tags, ",") != "existing,guide" {
		t.Fatalf("result = %+v, want path notes/guide.md, 2 paragraphs, existing/guide tags", result)
	}
	written, err := os.ReadFile(filepath.Join(root, "notes", "guide.md"))
	if err != nil || string(written) != content {
		t.Fatalf("written content = %q, %v; want existing frontmatter unchanged", written, err)
	}
	s, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got, err := s.GetDoc("notes/guide.md"); err != nil || got != content {
		t.Fatalf("indexed content = %q, %v", got, err)
	}
}

func TestRunAddDocumentUsesExclusiveCreate(t *testing.T) {
	root, db := newDocumentMutationWorkspace(t)
	request := addDocumentRequest{DB: db, Root: root, Path: "notes/once.md", Content: "one paragraph"}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := runAddDocument(context.Background(), request, addDocumentDeps{})
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	successes, duplicates := 0, 0
	for err := range results {
		switch mutationErrorCode(err) {
		case "":
			if err != nil {
				t.Fatal(err)
			}
			successes++
		case "already_exists":
			duplicates++
		default:
			t.Fatalf("unexpected concurrent add error: %v", err)
		}
	}
	if successes != 1 || duplicates != 1 {
		t.Fatalf("concurrent adds: successes=%d duplicates=%d, want 1 and 1", successes, duplicates)
	}
}

func TestRunAddDocumentRollsBackOnlyNewFile(t *testing.T) {
	root, db := newDocumentMutationWorkspace(t)
	indexErr := errors.New("index failed")
	path := filepath.Join("notes", "rollback.md")
	_, err := runAddDocument(context.Background(), addDocumentRequest{DB: db, Root: root, Path: path, Content: "body"}, addDocumentDeps{
		Index: func(context.Context, string, string, string, int64) error { return indexErr },
	})
	if !errors.Is(err, indexErr) {
		t.Fatalf("runAddDocument error = %v, want index failure", err)
	}
	if _, err := os.Stat(filepath.Join(root, path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed add left new file behind: %v", err)
	}
}

func TestRunAddDocumentPostCommitFailuresKeepSourceAndIndexConsistent(t *testing.T) {
	for _, tc := range []struct {
		name string
		deps addDocumentDeps
	}{
		{name: "paragraph count", deps: addDocumentDeps{
			Count: func(*store.Store, string) (int, error) { return 0, errors.New("count failed") },
		}},
		{name: "store close", deps: addDocumentDeps{
			Close: func(s *store.Store) error {
				if err := s.Close(); err != nil {
					return err
				}
				return errors.New("close failed")
			},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, db := newDocumentMutationWorkspace(t)
			oldFactory := documentMutationEmbedderFactory
			documentMutationEmbedderFactory = func() (textEmbedder, error) { return textEmbedderFunc(fakeEmbed), nil }
			t.Cleanup(func() { documentMutationEmbedderFactory = oldFactory })
			path := filepath.Join("notes", "committed.md")
			_, err := runAddDocument(context.Background(), addDocumentRequest{
				DB: db, Root: root, Path: path, Content: "committed body",
			}, tc.deps)
			if err == nil {
				t.Fatal("injected post-commit failure unexpectedly succeeded")
			}
			if content, readErr := os.ReadFile(filepath.Join(root, path)); readErr != nil || string(content) != "committed body" {
				t.Fatalf("post-commit failure removed source: content=%q error=%v", content, readErr)
			}
			s, openErr := store.Open(db)
			if openErr != nil {
				t.Fatal(openErr)
			}
			defer s.Close()
			if content, getErr := s.GetDoc("notes/committed.md"); getErr != nil || content != "committed body" {
				t.Fatalf("post-commit failure lost index: content=%q error=%v", content, getErr)
			}
		})
	}
}

func TestDocumentMutationsSerializeAddAndReindex(t *testing.T) {
	root, db := newDocumentMutationWorkspace(t)
	oldFactory := documentMutationEmbedderFactory
	documentMutationEmbedderFactory = func() (textEmbedder, error) { return textEmbedderFunc(fakeEmbed), nil }
	t.Cleanup(func() { documentMutationEmbedderFactory = oldFactory })
	indexStarted := make(chan struct{})
	releaseAdd := make(chan struct{})
	reindexRead := make(chan struct{})
	addDone := make(chan error, 1)
	go func() {
		_, err := runAddDocument(context.Background(), addDocumentRequest{
			DB: db, Root: root, Path: "notes/partial.md", Content: "partial",
		}, addDocumentDeps{Index: func(context.Context, string, string, string, int64) error {
			close(indexStarted)
			<-releaseAdd
			return errors.New("index failed")
		}})
		addDone <- err
	}()
	<-indexStarted

	reindexDone := make(chan error, 1)
	go func() {
		_, err := runDocumentIndex(context.Background(), documentIndexRequest{
			DB: db, Paths: []string{filepath.Join(root, "notes", "partial.md")},
			beforeRead: func(string) { close(reindexRead) },
		})
		reindexDone <- err
	}()
	crossed := false
	select {
	case <-reindexRead:
		crossed = true
	case <-time.After(2 * time.Second):
	}
	close(releaseAdd)
	if err := <-addDone; err == nil {
		t.Fatal("injected add failure unexpectedly succeeded")
	}
	select {
	case <-reindexDone:
	case <-time.After(2 * time.Second):
		t.Fatal("reindex remained blocked after add finished")
	}
	if crossed {
		t.Fatal("reindex read a file from an in-progress add")
	}
}

func TestCanceledDocumentMutationNeverTouchesFilesystemAndReturnsGate(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  func(addDocumentDeps) (context.Context, addDocumentDeps)
	}{
		{
			name: "already canceled with free token",
			ctx: func(deps addDocumentDeps) (context.Context, addDocumentDeps) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx, deps
			},
		},
		{
			name: "canceled immediately after token acquisition",
			ctx: func(deps addDocumentDeps) (context.Context, addDocumentDeps) {
				ctx, cancel := context.WithCancel(context.Background())
				deps.acquireHooks.afterAcquire = cancel
				return ctx, deps
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, db := newDocumentMutationWorkspace(t)
			beforeCreate := false
			deps := addDocumentDeps{beforeCreate: func() { beforeCreate = true }}
			ctx, deps := tc.ctx(deps)
			_, err := runAddDocument(ctx, addDocumentRequest{
				DB: db, Root: root, Path: "notes/canceled.md", Content: "body",
			}, deps)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled add error=%v, want context.Canceled", err)
			}
			if beforeCreate {
				t.Fatal("canceled add reached beforeCreate")
			}
			if _, err := os.Stat(filepath.Join(root, "notes")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("canceled add left directory state: %v", err)
			}
			if _, err := os.Stat(db); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("canceled add touched DB: %v", err)
			}

			oldFactory := documentMutationEmbedderFactory
			documentMutationEmbedderFactory = func() (textEmbedder, error) { return textEmbedderFunc(fakeEmbed), nil }
			t.Cleanup(func() { documentMutationEmbedderFactory = oldFactory })
			if _, err := runAddDocument(context.Background(), addDocumentRequest{
				DB: db, Root: root, Path: "notes/next.md", Content: "next",
			}, addDocumentDeps{}); err != nil {
				t.Fatalf("next mutation failed after canceled acquire: %v", err)
			}
		})
	}
}

func TestAcquireDocumentMutationReturnsTokenWhenCancelAndReleaseRace(t *testing.T) {
	<-documentMutationGate
	ctx, cancel := context.WithCancel(context.Background())
	waiting := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- acquireDocumentMutation(ctx, documentMutationAcquireHooks{
			beforeWait: func() { close(waiting) },
		})
	}()
	<-waiting
	cancel()
	unlockDocumentMutation()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("racing acquire error=%v, want context.Canceled", err)
	}
	if err := acquireDocumentMutation(context.Background(), documentMutationAcquireHooks{}); err != nil {
		t.Fatalf("gate token leaked after racing cancellation: %v", err)
	}
	unlockDocumentMutation()
}

func TestRunAddDocumentDoesNotCreateThroughSwappedAncestor(t *testing.T) {
	root, db := newDocumentMutationWorkspace(t)
	if err := os.Mkdir(filepath.Join(root, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	_, err := runAddDocument(context.Background(), addDocumentRequest{DB: db, Root: root, Path: "notes/safe.md", Content: "workspace"}, addDocumentDeps{
		beforeCreate: func() {
			if err := os.Rename(filepath.Join(root, "notes"), filepath.Join(root, "moved")); err != nil {
				t.Fatal(err)
			}
			makeDocumentDirectoryLink(t, filepath.Join(root, "notes"), out)
		},
	})
	if err == nil {
		t.Fatal("add through swapped ancestor succeeded; want confinement error")
	}
	if _, statErr := os.Stat(filepath.Join(out, "safe.md")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("add wrote outside retained root: %v", statErr)
	}
}

func TestRunAddDocumentRollbackDoesNotRemoveThroughSwappedAncestor(t *testing.T) {
	root, db := newDocumentMutationWorkspace(t)
	out := t.TempDir()
	outsidePath := filepath.Join(out, "safe.md")
	if err := os.WriteFile(outsidePath, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := runAddDocument(context.Background(), addDocumentRequest{DB: db, Root: root, Path: "notes/safe.md", Content: "workspace"}, addDocumentDeps{
		Index: func(context.Context, string, string, string, int64) error {
			if err := os.Rename(filepath.Join(root, "notes"), filepath.Join(root, "moved")); err != nil {
				t.Fatal(err)
			}
			makeDocumentDirectoryLink(t, filepath.Join(root, "notes"), out)
			return errors.New("index failed")
		},
	})
	if got := mutationErrorCode(err); got != "partial_failure" {
		t.Fatalf("rollback after ancestor swap error = %v (code %q), want partial_failure", err, got)
	}
	if content, readErr := os.ReadFile(outsidePath); readErr != nil || string(content) != "outside" {
		t.Fatalf("rollback touched outside file: content=%q error=%v", content, readErr)
	}
	if content, readErr := os.ReadFile(filepath.Join(root, "moved", "safe.md")); readErr != nil || string(content) != "workspace" {
		t.Fatalf("original created file should remain after failed confined rollback: content=%q error=%v", content, readErr)
	}
}

func TestRunAddDocumentRollbackFailureIsPartialFailure(t *testing.T) {
	root, db := newDocumentMutationWorkspace(t)
	path := filepath.Join("notes", "partial.md")
	_, err := runAddDocument(context.Background(), addDocumentRequest{DB: db, Root: root, Path: path, Content: "body"}, addDocumentDeps{
		Index:  func(context.Context, string, string, string, int64) error { return errors.New("index failed") },
		Remove: func(string) error { return errors.New("remove failed") },
	})
	if got := mutationErrorCode(err); got != "partial_failure" || !strings.Contains(err.Error(), "notes/partial.md") {
		t.Fatalf("rollback failure = %v (code %q), want typed partial_failure containing relative path", err, got)
	}
	if _, statErr := os.Stat(filepath.Join(root, path)); statErr != nil {
		t.Fatalf("failed rollback should leave the new file visible: %v", statErr)
	}
}

func TestRunDocumentIndexValidatesEveryRootBeforeMutation(t *testing.T) {
	root, db := newDocumentMutationWorkspace(t)
	inside := filepath.Join(root, "inside.md")
	if err := os.WriteFile(inside, []byte("inside"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	_, err := runDocumentIndex(context.Background(), documentIndexRequest{DB: db, Paths: []string{inside, out}})
	if got := mutationErrorCode(err); got != "path_outside_workspace" {
		t.Fatalf("mixed roots error = %v (code %q), want path_outside_workspace", err, got)
	}
	if _, err := os.Stat(db); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("index mutated DB before validating every root: %v", err)
	}
}

func TestRunDocumentIndexRejectsOutsideRootLinkBeforeMutation(t *testing.T) {
	root, db := newDocumentMutationWorkspace(t)
	outside := t.TempDir()
	link := filepath.Join(root, "linked")
	makeDocumentDirectoryLink(t, link, outside)
	_, err := runDocumentIndex(context.Background(), documentIndexRequest{DB: db, Paths: []string{link}})
	if got := mutationErrorCode(err); got != "path_outside_workspace" {
		t.Fatalf("linked root error = %v (code %q), want path_outside_workspace", err, got)
	}
	if _, err := os.Stat(db); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("linked root mutated DB before rejection: %v", err)
	}
}

func TestRunDocumentIndexDoesNotReadThroughSwappedAncestor(t *testing.T) {
	root, db := newDocumentMutationWorkspace(t)
	if err := os.Mkdir(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "docs", "safe.md"), []byte("workspace"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if err := os.WriteFile(filepath.Join(out, "safe.md"), []byte("outside secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := runDocumentIndex(context.Background(), documentIndexRequest{
		DB: db, Paths: []string{filepath.Join(root, "docs", "safe.md")},
		beforeRead: func(string) {
			if err := os.Rename(filepath.Join(root, "docs"), filepath.Join(root, "moved")); err != nil {
				t.Fatal(err)
			}
			makeDocumentDirectoryLink(t, filepath.Join(root, "docs"), out)
		},
	})
	if err == nil {
		t.Fatal("reindex through swapped ancestor succeeded; want confinement error")
	}
	s, openErr := store.Open(db)
	if openErr != nil {
		t.Fatal(openErr)
	}
	defer s.Close()
	if _, getErr := s.GetDoc("docs/safe.md"); getErr != store.ErrNotFound {
		t.Fatalf("reindex stored content after confined read failure: %v", getErr)
	}
}

func TestRunDocumentIndexCancellationAfterEmbeddingStartsRollsBack(t *testing.T) {
	root, db := newDocumentMutationWorkspace(t)
	path := filepath.Join(root, "cancel.md")
	if err := os.WriteFile(path, []byte("cancel this paragraph"), 0o644); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	oldFactory := documentMutationEmbedderFactory
	documentMutationEmbedderFactory = func() (textEmbedder, error) {
		return textEmbedderFunc(func(string) ([]float32, error) {
			close(started)
			<-release
			return make([]float32, 768), nil
		}), nil
	}
	t.Cleanup(func() { documentMutationEmbedderFactory = oldFactory })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := runDocumentIndex(ctx, documentIndexRequest{DB: db, Paths: []string{path}})
		done <- err
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		t.Fatalf("index returned while its synchronous embedder was still running: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled index error=%v, want context.Canceled", err)
	}
	s, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.GetDoc("cancel.md"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("canceled index committed a partial document: %v", err)
	}
}

func TestRunDocumentConverterSnapshotPreservesDisplayPathExtensionAndCleansUp(t *testing.T) {
	t.Setenv("GO_WANT_DOCUMENT_CONVERTER", "1")
	record := filepath.Join(t.TempDir(), "snapshot-path.txt")
	t.Setenv("DOCUMENT_CONVERTER_RECORD", record)
	text, stderr, err := runDocumentConverterSnapshot(context.Background(), documentConverterTestArgv(), "docs/report.conv", []byte("source"))
	if err != nil {
		t.Fatal(err)
	}
	if text != "input=docs/report.conv;ext=.conv;data=source" {
		t.Fatalf("converted text=%q, want caller-relative path and matching extension", text)
	}
	if stderr != "stderr-input=docs/report.conv\n" {
		t.Fatalf("converter stderr=%q, want sanitized caller-relative path", stderr)
	}
	snapshot, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Ext(string(snapshot)) != ".conv" {
		t.Fatalf("snapshot path=%q, want .conv extension", snapshot)
	}
	if _, err := os.Stat(string(snapshot)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("converter snapshot was not cleaned up: %v", err)
	}
}

func TestRunDocumentConverterSnapshotHonorsCancellation(t *testing.T) {
	t.Setenv("GO_WANT_DOCUMENT_CONVERTER", "1")
	t.Setenv("DOCUMENT_CONVERTER_SLEEP", "1")
	record := filepath.Join(t.TempDir(), "started.txt")
	t.Setenv("DOCUMENT_CONVERTER_RECORD", record)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := runDocumentConverterSnapshot(ctx, documentConverterTestArgv(), "docs/report.conv", []byte("source"))
		done <- err
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(record); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("converter did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled converter error=%v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("converter ignored context cancellation")
	}
}

func TestRunDocumentIndexIndexesDocumentsAndCountsCodeExclusions(t *testing.T) {
	root, db := newDocumentMutationWorkspace(t)
	if err := os.WriteFile(filepath.Join(root, "guide.md"), []byte("first\n\nsecond"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ignored.go"), []byte("package ignored"), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := runDocumentIndex(context.Background(), documentIndexRequest{DB: db, Paths: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Indexed != 1 || result.Skipped != 0 || result.Excluded != 1 {
		t.Fatalf("index result = %+v, want 1 indexed, 0 skipped, 1 excluded", result)
	}
	s, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if count, err := s.ParagraphCount("guide.md"); err != nil || count != 2 {
		t.Fatalf("indexed paragraph count = %d, %v; want 2", count, err)
	}
}

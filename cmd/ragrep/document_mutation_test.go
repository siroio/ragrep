package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/siroio/ragrep/internal/store"
)

func mutationErrorCode(err error) string {
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

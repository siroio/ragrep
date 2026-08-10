package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/siroio/ragrep/internal/config"
	"github.com/siroio/ragrep/internal/embed"
	"github.com/siroio/ragrep/internal/store"
)

type documentIndexRequest struct {
	DB          string
	Paths       []string
	Prune       bool
	IncludeCode bool

	displayPaths []string
	beforeRead   func(string)
}

type documentIndexResult struct {
	Indexed  int      `json:"indexed"`
	Skipped  int      `json:"skipped"`
	Excluded int      `json:"excluded"`
	Warnings []string `json:"warnings,omitempty"`

	prunedPaths []string
	events      []documentIndexEvent
}

type documentIndexEvent struct {
	text   string
	stderr bool
	raw    bool
}

func (r *documentIndexResult) addWarning(warning string) {
	r.Warnings = append(r.Warnings, warning)
	r.events = append(r.events, documentIndexEvent{text: warning, stderr: true})
}

func (r *documentIndexResult) addIndexed(path string) {
	r.Indexed++
	r.events = append(r.events, documentIndexEvent{text: "indexed " + path})
}

func (r *documentIndexResult) addPruned(path string) {
	r.prunedPaths = append(r.prunedPaths, path)
	r.events = append(r.events, documentIndexEvent{text: "pruned " + path})
}

func (r *documentIndexResult) addStderr(output string) {
	if output != "" {
		r.events = append(r.events, documentIndexEvent{text: output, stderr: true, raw: true})
	}
}

type confinedDocumentRoot struct {
	logicalPath string
	path        string
	root        *os.Root
}

func openConfinedDocumentRoot(path string) (*confinedDocumentRoot, error) {
	logical, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	evaluated, err := filepath.EvalSymlinks(logical)
	if err != nil {
		return nil, err
	}
	evaluated, err = filepath.Abs(evaluated)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(evaluated)
	if err != nil {
		return nil, err
	}
	return &confinedDocumentRoot{logicalPath: logical, path: evaluated, root: root}, nil
}

func (r *confinedDocumentRoot) validateExisting(path string) error {
	evaluated, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	evaluated, err = filepath.Abs(evaluated)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(r.path, evaluated)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return documentPathOutsideError(path, r.path)
	}
	if _, err := r.root.Stat(rel); err != nil {
		if _, statErr := os.Stat(path); statErr != nil {
			return statErr
		}
		return documentPathOutsideError(path, r.path)
	}
	return nil
}

type addDocumentRequest struct {
	DB, Root, Path, Content string
	Tags                    []string
}

type addDocumentResult struct {
	Path       string   `json:"path"`
	Paragraphs int      `json:"paragraphs"`
	Tags       []string `json:"tags"`
}

type addDocumentDeps struct {
	Index        func(context.Context, string, string, string, int64) error
	Remove       func(string) error
	beforeCreate func()
}

type documentWalkRoot struct {
	absolute string
	display  string
}

var documentMutationEmbedderFactory = func() (textEmbedder, error) {
	dir, err := embed.CacheDir()
	if err != nil {
		return nil, err
	}
	return embed.New(dir)
}

func runDocumentIndex(ctx context.Context, request documentIndexRequest) (documentIndexResult, error) {
	var result documentIndexResult
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if len(request.Paths) == 0 {
		return result, mcpInvalidArgument()
	}
	root, err := workspaceRoot(request.DB)
	if err != nil {
		return result, err
	}
	confinedRoot, err := openConfinedDocumentRoot(root)
	if err != nil {
		return result, err
	}
	defer confinedRoot.root.Close()

	paths := make([]documentWalkRoot, len(request.Paths))
	normRoots := make([]string, len(request.Paths))
	for i, requested := range request.Paths {
		absolute, err := filepath.Abs(requested)
		if err != nil {
			return result, err
		}
		if err := confinedRoot.validateExisting(absolute); err != nil {
			return result, err
		}
		normRoots[i], err = normPath(absolute, root)
		if err != nil {
			return result, documentPathOutsideError(requested, root)
		}
		display := requested
		if len(request.displayPaths) == len(request.Paths) {
			display = request.displayPaths[i]
		}
		paths[i] = documentWalkRoot{absolute: absolute, display: display}
	}

	s, err := openStoreAt(request.DB)
	if err != nil {
		return result, err
	}
	defer s.Close()
	e, err := documentMutationEmbedderFactory()
	if err != nil {
		return result, err
	}
	defer e.Close()

	cfg, err := config.Load(root)
	if err != nil {
		result.addWarning(fmt.Sprintf("warning: loading config: %v", err))
		cfg = config.Config{}
	}

	for _, walkRoot := range paths {
		err := filepath.WalkDir(walkRoot.absolute, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := confinedRoot.validateExisting(path); err != nil {
				return err
			}
			name := d.Name()
			if d.IsDir() {
				if strings.HasPrefix(name, ".") && path != walkRoot.absolute {
					return filepath.SkipDir
				}
				return nil
			}
			rel, err := normPath(path, root)
			if err != nil {
				return err
			}
			displayPath, err := documentDisplayPath(walkRoot, path)
			if err != nil {
				return err
			}
			if !request.IncludeCode && codeExtensions[strings.ToLower(filepath.Ext(name))] {
				result.Excluded++
				return nil
			}
			if request.beforeRead != nil {
				request.beforeRead(rel)
			}
			safePath := filepath.FromSlash(rel)
			if argv := cfg.ConverterFor(strings.ToLower(filepath.Ext(name))); argv != nil {
				raw, err := confinedRoot.root.ReadFile(safePath)
				if err != nil {
					result.addWarning(fmt.Sprintf("warning: reading %s: %v", displayPath, err))
					result.Skipped++
					return nil
				}
				srcHash := store.HashContent(string(raw))
				if old, _ := s.DocHash(rel); old == srcHash {
					if info, err := confinedRoot.root.Stat(safePath); err == nil {
						if err := s.TouchDoc(rel, info.ModTime().Unix()); err != nil {
							return err
						}
					}
					return nil
				}
				text, converterStderr, err := runDocumentConverterSnapshot(ctx, argv, displayPath, raw)
				result.addStderr(converterStderr)
				if err != nil {
					if ctxErr := ctx.Err(); ctxErr != nil {
						return ctxErr
					}
					result.addWarning(fmt.Sprintf("warning: convert %s: %v", displayPath, err))
					result.Skipped++
					return nil
				}
				info, err := confinedRoot.root.Stat(safePath)
				if err != nil {
					return err
				}
				changed, err := s.UpsertDocWithHash(rel, text, info.ModTime().Unix(), srcHash, e.Embed)
				if err != nil {
					return fmt.Errorf("%s: %w", rel, err)
				}
				if changed {
					result.addIndexed(rel)
				}
				return nil
			}

			info, err := confinedRoot.root.Stat(safePath)
			if err != nil {
				return err
			}
			if info.Size() > maxFileSize {
				result.Skipped++
				return nil
			}
			data, err := confinedRoot.root.ReadFile(safePath)
			if err != nil {
				return err
			}
			if !isTextDocument(data) {
				result.Skipped++
				return nil
			}
			changed, err := s.UpsertDoc(rel, string(data), info.ModTime().Unix(), e.Embed)
			if err != nil {
				return fmt.Errorf("%s: %w", rel, err)
			}
			if changed {
				result.addIndexed(rel)
			}
			return nil
		})
		if err != nil {
			return result, err
		}
	}

	if request.Prune {
		paths, err := s.ListPaths()
		if err != nil {
			return result, err
		}
		for _, p := range paths {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			underRoot := false
			for _, root := range normRoots {
				if root == "." || p == root || strings.HasPrefix(p, root+"/") {
					underRoot = true
					break
				}
			}
			if !underRoot {
				continue
			}
			_, statErr := confinedRoot.root.Stat(filepath.FromSlash(p))
			doPrune, err := pruneDecision(statErr)
			if err != nil {
				return result, err
			}
			if !doPrune {
				continue
			}
			if err := s.DeleteDoc(p); err != nil {
				return result, err
			}
			result.addPruned(p)
		}
	}
	return result, nil
}

func documentDisplayPath(root documentWalkRoot, absolute string) (string, error) {
	rel, err := filepath.Rel(root.absolute, absolute)
	if err != nil {
		return "", err
	}
	if rel == "." {
		return root.display, nil
	}
	return filepath.Join(root.display, rel), nil
}

func isTextDocument(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	limit := min(len(data), 8192)
	return !bytes.ContainsRune(data[:limit], 0)
}

func runDocumentConverterSnapshot(ctx context.Context, argv []string, displayPath string, raw []byte) (text, stderr string, err error) {
	snapshot, err := os.CreateTemp("", "ragrep-converter-*"+filepath.Ext(displayPath))
	if err != nil {
		return "", "", err
	}
	snapshotPath := snapshot.Name()
	defer os.Remove(snapshotPath)
	if _, err := snapshot.Write(raw); err != nil {
		snapshot.Close()
		return "", "", sanitizedDocumentConverterError(err, snapshotPath, displayPath)
	}
	if err := snapshot.Close(); err != nil {
		return "", "", sanitizedDocumentConverterError(err, snapshotPath, displayPath)
	}
	args := make([]string, len(argv)-1)
	for i, arg := range argv[1:] {
		args[i] = strings.ReplaceAll(arg, "{input}", snapshotPath)
	}
	var stdout, stderrBuffer bytes.Buffer
	cmd := exec.CommandContext(ctx, argv[0], args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderrBuffer
	err = cmd.Run()
	text = strings.ReplaceAll(stdout.String(), snapshotPath, displayPath)
	stderr = strings.ReplaceAll(stderrBuffer.String(), snapshotPath, displayPath)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return text, stderr, ctxErr
	}
	return text, stderr, sanitizedDocumentConverterError(err, snapshotPath, displayPath)
}

func sanitizedDocumentConverterError(err error, snapshotPath, displayPath string) error {
	if err == nil {
		return nil
	}
	return errors.New(strings.ReplaceAll(err.Error(), snapshotPath, displayPath))
}

func resolveDocumentWritePath(root, requested string) (absolute, key string, err error) {
	confinedRoot, err := openConfinedDocumentRoot(root)
	if err != nil {
		return "", "", err
	}
	defer confinedRoot.root.Close()
	return resolveDocumentWritePathAt(confinedRoot, requested)
}

func resolveDocumentWritePathAt(confinedRoot *confinedDocumentRoot, requested string) (absolute, key string, err error) {
	root := confinedRoot.logicalPath
	if strings.TrimSpace(requested) == "" || filepath.IsAbs(requested) {
		return "", "", mcpInvalidArgument()
	}
	clean := filepath.Clean(requested)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", "", documentPathOutsideError(requested, root)
	}
	if codeExtensions[strings.ToLower(filepath.Ext(clean))] {
		return "", "", mcpInvalidArgument()
	}
	absolute = filepath.Join(root, clean)
	key, err = normPath(absolute, root)
	if err != nil {
		return "", "", documentPathOutsideError(requested, root)
	}
	parent := filepath.Dir(absolute)
	for {
		evaluatedParent, evalErr := filepath.EvalSymlinks(parent)
		if evalErr == nil {
			if err := confinedRoot.validateExisting(evaluatedParent); err != nil {
				return "", "", documentPathOutsideError(requested, root)
			}
			break
		}
		if !errors.Is(evalErr, os.ErrNotExist) {
			return "", "", evalErr
		}
		next := filepath.Dir(parent)
		if next == parent {
			return "", "", evalErr
		}
		parent = next
	}
	return absolute, key, nil
}

func runAddDocument(ctx context.Context, request addDocumentRequest, deps addDocumentDeps) (addDocumentResult, error) {
	var result addDocumentResult
	if err := ctx.Err(); err != nil {
		return result, err
	}
	confinedRoot, err := openConfinedDocumentRoot(request.Root)
	if err != nil {
		return result, err
	}
	defer confinedRoot.root.Close()
	_, key, err := resolveDocumentWritePathAt(confinedRoot, request.Path)
	if err != nil {
		return result, err
	}
	for _, tag := range request.Tags {
		if strings.ContainsAny(tag, ",]\r\n") {
			return result, mcpInvalidArgument()
		}
	}
	if len(request.Content) == 0 {
		return result, mcpInvalidArgument()
	}
	content := withFrontmatter(request.Content, request.Tags)
	if len(content) > maxFileSize {
		return result, mcpInvalidArgument()
	}
	safePath := filepath.FromSlash(key)
	if err := confinedRoot.root.MkdirAll(filepath.Dir(safePath), 0o755); err != nil {
		return result, err
	}
	_, checkedKey, err := resolveDocumentWritePathAt(confinedRoot, request.Path)
	if err != nil {
		return result, err
	}
	if checkedKey != key {
		return result, documentPathOutsideError(request.Path, request.Root)
	}
	if deps.beforeCreate != nil {
		deps.beforeCreate()
	}

	f, err := confinedRoot.root.OpenFile(safePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, os.ErrExist) {
		return result, documentMutationError("already_exists", fmt.Sprintf("%s already exists", key))
	}
	if err != nil {
		return result, err
	}
	remove := deps.Remove
	if remove == nil {
		remove = func(path string) error { return confinedRoot.root.Remove(filepath.FromSlash(path)) }
	}
	rollback := func(cause error) error {
		if removeErr := remove(key); removeErr != nil {
			return documentMutationError("partial_failure", fmt.Sprintf("failed to roll back %s after %v: %v", key, cause, removeErr))
		}
		return cause
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return result, rollback(err)
	}
	if err := f.Close(); err != nil {
		return result, rollback(err)
	}
	info, err := confinedRoot.root.Stat(safePath)
	if err != nil {
		return result, rollback(err)
	}
	index := deps.Index
	if index == nil {
		index = indexNewDocument
	}
	if err := index(ctx, request.DB, key, content, info.ModTime().Unix()); err != nil {
		return result, rollback(err)
	}
	s, err := openStoreAt(request.DB)
	if err != nil {
		return result, rollback(err)
	}
	paragraphs, countErr := s.ParagraphCount(key)
	closeErr := s.Close()
	if countErr != nil {
		return result, rollback(countErr)
	}
	if closeErr != nil {
		return result, rollback(closeErr)
	}
	tags := store.ParseTags(content)
	if tags == nil {
		tags = []string{}
	}
	return addDocumentResult{Path: key, Paragraphs: paragraphs, Tags: tags}, nil
}

func indexNewDocument(ctx context.Context, db, key, content string, mtime int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s, err := openStoreAt(db)
	if err != nil {
		return err
	}
	defer s.Close()
	e, err := documentMutationEmbedderFactory()
	if err != nil {
		return err
	}
	defer e.Close()
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err = s.UpsertDoc(key, content, mtime, e.Embed)
	return err
}

func documentMutationError(code, message string) error {
	return &mcpDomainError{Failure: mcpFailure{Code: code, Message: message}}
}

func documentPathOutsideError(path, root string) error {
	return documentMutationError("path_outside_workspace", fmt.Sprintf("%s is outside the workspace root %s", path, root))
}

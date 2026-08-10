package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
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
	line   string
	stderr bool
}

func (r *documentIndexResult) addWarning(warning string) {
	r.Warnings = append(r.Warnings, warning)
	r.events = append(r.events, documentIndexEvent{line: warning, stderr: true})
}

func (r *documentIndexResult) addIndexed(path string) {
	r.Indexed++
	r.events = append(r.events, documentIndexEvent{line: "indexed " + path})
}

func (r *documentIndexResult) addPruned(path string) {
	r.prunedPaths = append(r.prunedPaths, path)
	r.events = append(r.events, documentIndexEvent{line: "pruned " + path})
}

type confinedDocumentRoot struct {
	path string
	root *os.Root
}

func openConfinedDocumentRoot(path string) (*confinedDocumentRoot, error) {
	evaluated, err := filepath.EvalSymlinks(path)
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
	return &confinedDocumentRoot{path: evaluated, root: root}, nil
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
	Index  func(context.Context, string, string, string, int64) error
	Remove func(string) error
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

	paths := make([]string, len(request.Paths))
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
		paths[i] = absolute
	}

	s, err := openStoreAt(request.DB)
	if err != nil {
		return result, err
	}
	defer s.Close()
	dir, err := embed.CacheDir()
	if err != nil {
		return result, err
	}
	e, err := embed.New(dir)
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
		err := filepath.WalkDir(walkRoot, func(path string, d fs.DirEntry, walkErr error) error {
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
				if strings.HasPrefix(name, ".") && path != walkRoot {
					return filepath.SkipDir
				}
				return nil
			}
			if !request.IncludeCode && codeExtensions[strings.ToLower(filepath.Ext(name))] {
				result.Excluded++
				return nil
			}
			if argv := cfg.ConverterFor(strings.ToLower(filepath.Ext(name))); argv != nil {
				raw, err := os.ReadFile(path)
				if err != nil {
					result.addWarning(fmt.Sprintf("warning: reading %s: %v", path, err))
					result.Skipped++
					return nil
				}
				rel, err := normPath(path, root)
				if err != nil {
					return err
				}
				srcHash := store.HashContent(string(raw))
				if old, _ := s.DocHash(rel); old == srcHash {
					if info, err := d.Info(); err == nil {
						if err := s.TouchDoc(rel, info.ModTime().Unix()); err != nil {
							return err
						}
					}
					return nil
				}
				text, err := runConverter(argv, path)
				if err != nil {
					result.addWarning(fmt.Sprintf("warning: convert %s: %v", path, err))
					result.Skipped++
					return nil
				}
				info, err := d.Info()
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

			info, err := d.Info()
			if err != nil || info.Size() > maxFileSize || !isTextFile(path) {
				result.Skipped++
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, err := normPath(path, root)
			if err != nil {
				return err
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
			_, statErr := os.Stat(filepath.Join(root, filepath.FromSlash(p)))
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

func resolveDocumentWritePath(root, requested string) (absolute, key string, err error) {
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
	root, err = filepath.Abs(root)
	if err != nil {
		return "", "", err
	}
	absolute = filepath.Join(root, clean)
	key, err = normPath(absolute, root)
	if err != nil {
		return "", "", documentPathOutsideError(requested, root)
	}
	confinedRoot, err := openConfinedDocumentRoot(root)
	if err != nil {
		return "", "", err
	}
	defer confinedRoot.root.Close()
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
	absolute, key, err := resolveDocumentWritePath(request.Root, request.Path)
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
	if err := os.MkdirAll(filepath.Dir(absolute), 0o755); err != nil {
		return result, err
	}
	checkedAbsolute, checkedKey, err := resolveDocumentWritePath(request.Root, request.Path)
	if err != nil {
		return result, err
	}
	if checkedAbsolute != absolute || checkedKey != key {
		return result, documentPathOutsideError(request.Path, request.Root)
	}

	f, err := os.OpenFile(absolute, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, os.ErrExist) {
		return result, documentMutationError("already_exists", fmt.Sprintf("%s already exists", key))
	}
	if err != nil {
		return result, err
	}
	remove := deps.Remove
	if remove == nil {
		remove = os.Remove
	}
	rollback := func(cause error) error {
		if removeErr := remove(absolute); removeErr != nil {
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
	info, err := os.Stat(absolute)
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
	dir, err := embed.CacheDir()
	if err != nil {
		return err
	}
	e, err := embed.New(dir)
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

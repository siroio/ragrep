package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/siroio/ragrep/internal/codeindex"
	"github.com/siroio/ragrep/internal/codestore"
)

var ErrWorkspaceSyncing = errors.New("workspace syncing")

type workspaceState struct {
	root, language string
	roots          []string
	store          *codestore.Store
	mu             sync.Mutex
	generation     uint64
	refreshDone    chan struct{}
	refreshErr     error
	watcher        *fsnotify.Watcher

	ext                 string
	enumerate           func(string, string) ([]string, error)
	refreshRunning      bool
	refreshWaiters      int
	fullRefreshRequired bool
	closed              bool
	refreshMu           sync.Mutex
	watcherWG           sync.WaitGroup
}

type workspaceFile struct {
	hash string
	body string
}

func newWorkspaceState(root string, store *codestore.Store, roots []string, language string) (*workspaceState, error) {
	if store == nil {
		return nil, errors.New("workspace store is nil")
	}
	ext, ok := codeLangExt[language]
	if !ok {
		return nil, fmt.Errorf("unsupported language %q", language)
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	relRoots := make([]string, 0, len(roots))
	for _, root := range roots {
		path := root
		if !filepath.IsAbs(path) {
			path = filepath.Join(absRoot, filepath.FromSlash(path))
		}
		rel, err := normPath(path, absRoot)
		if err != nil {
			return nil, err
		}
		relRoots = append(relRoots, rel)
	}
	return &workspaceState{
		root:      absRoot,
		language:  language,
		roots:     relRoots,
		store:     store,
		ext:       ext,
		enumerate: discoverCodeFiles,
	}, nil
}

func (w *workspaceState) Barrier(ctx context.Context) (uint64, error) {
	w.mu.Lock()
	if w.refreshDone == nil {
		w.refreshDone = make(chan struct{})
		w.refreshRunning = true
		done := w.refreshDone
		go w.runFullRefresh(done)
	}
	done := w.refreshDone
	w.refreshWaiters++
	w.mu.Unlock()

	select {
	case <-done:
		w.mu.Lock()
		generation, err := w.generation, w.refreshErr
		w.releaseRefreshWaiterLocked()
		w.mu.Unlock()
		return generation, err
	case <-ctx.Done():
		w.mu.Lock()
		w.releaseRefreshWaiterLocked()
		w.mu.Unlock()
		return 0, ErrWorkspaceSyncing
	}
}

func (w *workspaceState) releaseRefreshWaiterLocked() {
	w.refreshWaiters--
	if w.refreshWaiters == 0 && !w.refreshRunning {
		w.refreshDone = nil
	}
}

func (w *workspaceState) runFullRefresh(done chan struct{}) {
	w.refreshMu.Lock()
	err := w.refreshAll()
	w.refreshMu.Unlock()

	w.mu.Lock()
	w.refreshErr = err
	if err == nil {
		w.fullRefreshRequired = false
	}
	w.refreshRunning = false
	close(done)
	if w.refreshWaiters == 0 {
		w.refreshDone = nil
	}
	w.mu.Unlock()
}

func (w *workspaceState) refreshAll() error {
	files := make(map[string]workspaceFile)
	for _, root := range w.roots {
		absRoot := filepath.Join(w.root, filepath.FromSlash(root))
		found, err := w.enumerate(absRoot, w.ext)
		if err != nil {
			return err
		}
		for _, path := range found {
			body, err := os.ReadFile(path)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return err
			}
			rel, err := normPath(path, w.root)
			if err != nil {
				return err
			}
			files[rel] = workspaceFile{hash: codeindex.FileHash(body), body: string(body)}
		}
	}

	states, err := w.store.ListFileStates()
	if err != nil {
		return err
	}
	previous := make(map[string]codestore.FileState, len(states))
	for _, state := range states {
		previous[state.Path] = state
	}

	var changed []string
	for path, file := range files {
		state, ok := previous[path]
		if !ok || state.Deleted || state.Hash != file.hash {
			changed = append(changed, path)
		}
	}
	var deleted []string
	for path, state := range previous {
		if _, ok := files[path]; !ok && !state.Deleted && pathUnderAnyRoot(path, w.roots) {
			deleted = append(deleted, path)
		}
	}
	if len(changed) == 0 && len(deleted) == 0 {
		return nil
	}
	sort.Strings(changed)
	sort.Strings(deleted)
	generation := w.nextGeneration()
	for _, path := range changed {
		file := files[path]
		if err := w.store.PutLiveFile(path, file.hash, file.body, generation); err != nil {
			return err
		}
	}
	for _, path := range deleted {
		if err := w.store.PutLiveDeletion(path, previous[path].Hash, generation); err != nil {
			return err
		}
	}
	w.setGeneration(generation)
	return nil
}

func (w *workspaceState) nextGeneration() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.generation + 1
}

func (w *workspaceState) setGeneration(generation uint64) {
	w.mu.Lock()
	w.generation = generation
	w.mu.Unlock()
}

func (w *workspaceState) StartWatcher() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return errors.New("workspace is closed")
	}
	if w.watcher != nil {
		return nil
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	if err := w.addWatchRoots(watcher); err != nil {
		watcher.Close()
		return err
	}
	w.watcher = watcher
	w.watcherWG.Add(1)
	go w.watch(watcher)
	return nil
}

func (w *workspaceState) addWatchRoots(watcher *fsnotify.Watcher) error {
	added := make(map[string]bool)
	for _, root := range w.roots {
		absRoot := filepath.Join(w.root, filepath.FromSlash(root))
		err := filepath.WalkDir(absRoot, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !entry.IsDir() {
				return nil
			}
			name := entry.Name()
			if path != absRoot && (strings.HasPrefix(name, ".") || codeIndexSkipDirs[name]) {
				return filepath.SkipDir
			}
			if added[path] {
				return nil
			}
			added[path] = true
			return watcher.Add(path)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (w *workspaceState) watch(watcher *fsnotify.Watcher) {
	defer w.watcherWG.Done()
	pending := make(map[string]time.Time)
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()

	reset := func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		var next time.Time
		for _, deadline := range pending {
			if next.IsZero() || deadline.Before(next) {
				next = deadline
			}
		}
		if !next.IsZero() {
			delay := time.Until(next)
			if delay < 0 {
				delay = 0
			}
			timer.Reset(delay)
		}
	}

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if event.Has(fsnotify.Create) {
				if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
					if err := w.addWatchRoots(watcher); err != nil {
						w.requireFullRefresh()
					}
					w.requireFullRefresh()
					continue
				}
			}
			if filepath.Ext(event.Name) == w.ext {
				pending[event.Name] = time.Now().Add(50 * time.Millisecond)
				reset()
			}
		case _, ok := <-watcher.Errors:
			if !ok {
				return
			}
			w.requireFullRefresh()
		case now := <-timer.C:
			var paths []string
			for path, deadline := range pending {
				if !deadline.After(now) {
					paths = append(paths, path)
					delete(pending, path)
				}
			}
			if len(paths) != 0 {
				w.refreshMu.Lock()
				_ = w.refreshPaths(paths)
				w.refreshMu.Unlock()
			}
			reset()
		}
	}
}

func (w *workspaceState) requireFullRefresh() {
	w.mu.Lock()
	w.fullRefreshRequired = true
	w.mu.Unlock()
}

func (w *workspaceState) refreshPaths(paths []string) error {
	w.mu.Lock()
	fullRefreshRequired := w.fullRefreshRequired
	w.mu.Unlock()
	if fullRefreshRequired {
		return ErrWorkspaceSyncing
	}
	states, err := w.store.ListFileStates()
	if err != nil {
		return err
	}
	previous := make(map[string]codestore.FileState, len(states))
	for _, state := range states {
		previous[state.Path] = state
	}
	files := make(map[string]workspaceFile)
	var deleted []string
	for _, path := range paths {
		rel, err := normPath(path, w.root)
		if err != nil || !pathUnderAnyRoot(rel, w.roots) {
			continue
		}
		body, err := os.ReadFile(path)
		if err == nil && !(w.ext == ".go" && isGeneratedGoFile(path)) {
			file := workspaceFile{hash: codeindex.FileHash(body), body: string(body)}
			state, ok := previous[rel]
			if !ok || state.Deleted || state.Hash != file.hash {
				files[rel] = file
			}
			continue
		}
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if state, ok := previous[rel]; ok && !state.Deleted {
			deleted = append(deleted, rel)
		}
	}
	if len(files) == 0 && len(deleted) == 0 {
		return nil
	}
	generation := w.nextGeneration()
	for path, file := range files {
		if err := w.store.PutLiveFile(path, file.hash, file.body, generation); err != nil {
			return err
		}
	}
	for _, path := range deleted {
		if err := w.store.PutLiveDeletion(path, previous[path].Hash, generation); err != nil {
			return err
		}
	}
	w.setGeneration(generation)
	return nil
}

func (w *workspaceState) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	watcher := w.watcher
	w.watcher = nil
	w.mu.Unlock()
	if watcher == nil {
		return nil
	}
	err := watcher.Close()
	w.watcherWG.Wait()
	return err
}

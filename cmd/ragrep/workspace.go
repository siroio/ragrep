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

	ext                  string
	enumerate            func(string, string) ([]string, error)
	refresh              *workspaceRefresh
	fullRefreshRequired  bool
	fullRefreshEpoch     uint64
	confirmPath          func(string, string) error
	closed               bool
	shutdownDone         chan struct{}
	shutdownErr          error
	updateMu             contextMutex
	refreshWG            sync.WaitGroup
	watcherWG            sync.WaitGroup
	confirmationMu       sync.Mutex
	confirmationLocks    map[string]*sync.Mutex
	pendingConfirmations map[workspaceConfirmation]struct{}
}

type workspaceConfirmation struct{ path, hash string }

type contextMutex struct {
	once  sync.Once
	token chan struct{}
}

func (m *contextMutex) init() {
	m.token = make(chan struct{}, 1)
	m.token <- struct{}{}
}

func (m *contextMutex) Lock() {
	_ = m.LockContext(context.Background())
}

func (m *contextMutex) LockContext(ctx context.Context) error {
	m.once.Do(m.init)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-m.token:
	}
	if err := ctx.Err(); err != nil {
		m.Unlock()
		return err
	}
	return nil
}

func (m *contextMutex) Unlock() {
	m.once.Do(m.init)
	m.token <- struct{}{}
}

type workspaceRefresh struct {
	done             chan struct{}
	generation       uint64
	fullRefreshEpoch uint64
	err              error
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
	states, err := store.ListFileStates()
	if err != nil {
		return nil, err
	}
	var generation uint64
	pendingConfirmations := make(map[workspaceConfirmation]struct{}, len(states))
	for _, state := range states {
		if state.Generation > generation {
			generation = state.Generation
		}
		pendingConfirmations[workspaceConfirmation{path: state.Path, hash: state.Hash}] = struct{}{}
	}
	return &workspaceState{
		root:                 absRoot,
		language:             language,
		roots:                relRoots,
		store:                store,
		ext:                  ext,
		enumerate:            discoverCodeFiles,
		generation:           generation,
		pendingConfirmations: pendingConfirmations,
		shutdownDone:         make(chan struct{}),
	}, nil
}

func (w *workspaceState) Barrier(ctx context.Context) (uint64, error) {
	for {
		w.mu.Lock()
		if w.closed {
			w.mu.Unlock()
			return 0, ErrWorkspaceSyncing
		}
		if w.refresh == nil {
			w.refresh = &workspaceRefresh{done: make(chan struct{}), fullRefreshEpoch: w.fullRefreshEpoch}
			w.refreshDone = w.refresh.done
			w.refreshWG.Add(1)
			go w.runFullRefresh(w.refresh)
		}
		refresh := w.refresh
		w.mu.Unlock()

		select {
		case <-refresh.done:
			w.mu.Lock()
			stable := refresh.fullRefreshEpoch == w.fullRefreshEpoch
			w.mu.Unlock()
			if refresh.err != nil || stable {
				return refresh.generation, refresh.err
			}
		case <-ctx.Done():
			return 0, ErrWorkspaceSyncing
		}
	}
}

func (w *workspaceState) Snapshot(ctx context.Context) (uint64, func(), error) {
	if _, err := w.Barrier(ctx); err != nil {
		return 0, nil, err
	}
	if err := w.updateMu.LockContext(ctx); err != nil {
		return 0, nil, ErrWorkspaceSyncing
	}
	if err := ctx.Err(); err != nil {
		w.updateMu.Unlock()
		return 0, nil, ErrWorkspaceSyncing
	}
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		w.updateMu.Unlock()
		return 0, nil, ErrWorkspaceSyncing
	}
	generation := w.generation
	w.mu.Unlock()
	return generation, w.updateMu.Unlock, nil
}

func (w *workspaceState) runFullRefresh(refresh *workspaceRefresh) {
	defer w.refreshWG.Done()
	w.updateMu.Lock()
	err := w.refreshAll()
	w.updateMu.Unlock()

	w.mu.Lock()
	refresh.generation = w.generation
	refresh.err = err
	w.refreshErr = err
	if err == nil && w.fullRefreshEpoch == refresh.fullRefreshEpoch {
		w.fullRefreshRequired = false
	}
	if w.refresh == refresh {
		w.refresh = nil
		w.refreshDone = nil
	}
	close(refresh.done)
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

	states, err := w.store.ListWorkspaceFileStates()
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
	for _, path := range changed {
		w.scheduleConfirmation(path, files[path].hash)
	}
	for _, path := range deleted {
		w.scheduleConfirmation(path, previous[path].Hash)
	}
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
	watched, err := w.addWatchRoots(watcher)
	if err != nil {
		watcher.Close()
		return err
	}
	w.watcher = watcher
	w.watcherWG.Add(1)
	go w.watch(watcher, watched)
	return nil
}

func (w *workspaceState) addWatchRoots(watcher *fsnotify.Watcher) (map[string]bool, error) {
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
			return added, err
		}
	}
	return added, nil
}

func (w *workspaceState) watch(watcher *fsnotify.Watcher, watched map[string]bool) {
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
					added, err := w.addWatchRoots(watcher)
					if err != nil {
						w.requireFullRefresh()
					}
					for path := range added {
						watched[path] = true
					}
					w.requireFullRefresh()
					continue
				}
			}
			if (event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename)) && watched[event.Name] {
				delete(watched, event.Name)
				w.requireFullRefresh()
				continue
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
				err := w.refreshPaths(paths)
				if err != nil {
					w.requireFullRefresh()
				}
			}
			reset()
		}
	}
}

func (w *workspaceState) requireFullRefresh() {
	w.mu.Lock()
	w.fullRefreshRequired = true
	w.fullRefreshEpoch++
	w.mu.Unlock()
}

func (w *workspaceState) setConfirmation(confirm func(string, string) error) {
	w.mu.Lock()
	w.confirmPath = confirm
	pending := w.pendingConfirmations
	w.pendingConfirmations = nil
	w.mu.Unlock()
	for confirmation := range pending {
		go w.runConfirmation(confirm, confirmation)
	}
}

func (w *workspaceState) scheduleConfirmation(path, hash string) {
	w.mu.Lock()
	confirm := w.confirmPath
	if confirm == nil {
		if w.pendingConfirmations == nil {
			w.pendingConfirmations = make(map[workspaceConfirmation]struct{})
		}
		w.pendingConfirmations[workspaceConfirmation{path: path, hash: hash}] = struct{}{}
	}
	w.mu.Unlock()
	if confirm != nil {
		go w.runConfirmation(confirm, workspaceConfirmation{path: path, hash: hash})
	}
}

func (w *workspaceState) runConfirmation(confirm func(string, string) error, confirmation workspaceConfirmation) {
	if err := confirm(confirmation.path, confirmation.hash); err != nil {
		w.mu.Lock()
		if w.pendingConfirmations == nil {
			w.pendingConfirmations = make(map[workspaceConfirmation]struct{})
		}
		w.pendingConfirmations[confirmation] = struct{}{}
		w.mu.Unlock()
	}
}

func (w *workspaceState) lockConfirmation(path string) func() {
	path = filepath.ToSlash(path)
	w.confirmationMu.Lock()
	if w.confirmationLocks == nil {
		w.confirmationLocks = make(map[string]*sync.Mutex)
	}
	pathMu := w.confirmationLocks[path]
	if pathMu == nil {
		pathMu = new(sync.Mutex)
		w.confirmationLocks[path] = pathMu
	}
	w.confirmationMu.Unlock()
	pathMu.Lock()
	return pathMu.Unlock
}

func (w *workspaceState) refreshPaths(paths []string) error {
	w.updateMu.Lock()
	defer w.updateMu.Unlock()

	w.mu.Lock()
	fullRefreshRequired := w.fullRefreshRequired
	w.mu.Unlock()
	if fullRefreshRequired {
		return ErrWorkspaceSyncing
	}
	states, err := w.store.ListWorkspaceFileStates()
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
	for path, file := range files {
		w.scheduleConfirmation(path, file.hash)
	}
	for _, path := range deleted {
		w.scheduleConfirmation(path, previous[path].Hash)
	}
	return nil
}

func (w *workspaceState) Close() error {
	w.mu.Lock()
	if w.closed {
		done := w.shutdownDone
		w.mu.Unlock()
		<-done
		w.mu.Lock()
		err := w.shutdownErr
		w.mu.Unlock()
		return err
	}
	w.closed = true
	watcher := w.watcher
	w.watcher = nil
	w.mu.Unlock()
	var err error
	if watcher != nil {
		err = watcher.Close()
	}
	w.refreshWG.Wait()
	w.watcherWG.Wait()
	w.mu.Lock()
	w.shutdownErr = err
	close(w.shutdownDone)
	w.mu.Unlock()
	return err
}

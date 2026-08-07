package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/siroio/ragrep/internal/codeindex"
	"github.com/siroio/ragrep/internal/codestore"
)

type testWorkspace struct {
	*workspaceState
	root  string
	store *codestore.Store
	gen   uint64
}

func newTestWorkspace(t *testing.T, body string) *testWorkspace {
	t.Helper()
	root, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeWorkspaceFile(t, root, "service.go", body)
	store, err := codestore.Open(filepath.Join(t.TempDir(), "code.db"), "test-model", codeEmbedDim)
	if err != nil {
		t.Fatal(err)
	}
	state, err := newWorkspaceState(root, store, []string{"."}, "go")
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	w := &testWorkspace{workspaceState: state, root: root, store: store}
	t.Cleanup(func() {
		w.Close()
		store.Close()
	})
	w.gen = barrierGeneration(t, w.workspaceState)
	return w
}

func writeWorkspaceFile(t *testing.T, root, rel, body string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func barrierGeneration(t *testing.T, w *workspaceState) uint64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	gen, err := w.Barrier(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return gen
}

func TestWorkspaceBarrierSeesSaveBeforeWatcherDelivery(t *testing.T) {
	w := newTestWorkspace(t, "func OldName() {}")
	writeWorkspaceFile(t, w.root, "service.go", "func NewName() {}")
	gen := barrierGeneration(t, w.workspaceState)
	if gen != w.gen+1 {
		t.Fatalf("generation=%d, want %d", gen, w.gen+1)
	}
	hits, err := w.store.SearchLiveText("NewName", 5)
	if err != nil || len(hits) != 1 {
		t.Fatalf("hits=%v err=%v", hits, err)
	}
}

func TestWorkspaceQueuesConfirmationBeforeCallbackRegistration(t *testing.T) {
	w := newTestWorkspace(t, "func QueuedConfirmation() {}")
	confirmed := make(chan string, 1)
	w.setConfirmation(func(path, hash string) error {
		confirmed <- path + ":" + hash
		return nil
	})
	select {
	case got := <-confirmed:
		if want := "service.go:" + codeindex.FileHash([]byte("func QueuedConfirmation() {}")); got != want {
			t.Fatalf("confirmation=%q, want %q", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("confirmation scheduled before callback registration was dropped")
	}
}

func TestWorkspaceQueuesPersistedLiveConfirmationAtStartup(t *testing.T) {
	root, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, err := codestore.Open(filepath.Join(t.TempDir(), "code.db"), "test-model", codeEmbedDim)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.PutLiveFile("service.go", "persisted-hash", "func Persisted() {}", 7); err != nil {
		t.Fatal(err)
	}
	w, err := newWorkspaceState(root, store, []string{"."}, "go")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	confirmed := make(chan string, 1)
	w.setConfirmation(func(path, hash string) error {
		confirmed <- path + ":" + hash
		return nil
	})
	select {
	case got := <-confirmed:
		if got != "service.go:persisted-hash" {
			t.Fatalf("confirmation=%q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("persisted live confirmation was not resumed")
	}
}

func TestWorkspaceGenerationChangesOnlyWithVisibleContent(t *testing.T) {
	w := newTestWorkspace(t, "func StableName() {}")
	if w.gen == 0 {
		t.Fatal("initial generation=0")
	}
	if got := barrierGeneration(t, w.workspaceState); got != w.gen {
		t.Fatalf("unchanged generation=%d, want %d", got, w.gen)
	}
}

func TestWorkspaceBarrierSeesAddedFile(t *testing.T) {
	w := newTestWorkspace(t, "func ExistingName() {}")
	writeWorkspaceFile(t, w.root, "added.go", "func AddedName() {}")
	if got := barrierGeneration(t, w.workspaceState); got != w.gen+1 {
		t.Fatalf("generation=%d, want %d", got, w.gen+1)
	}
	hits, err := w.store.SearchLiveText("AddedName", 5)
	if err != nil || len(hits) != 1 || hits[0].Path != "added.go" {
		t.Fatalf("hits=%v err=%v", hits, err)
	}
}

func TestWorkspaceBarrierSeesDeletedFile(t *testing.T) {
	w := newTestWorkspace(t, "func DeletedName() {}")
	if err := os.Remove(filepath.Join(w.root, "service.go")); err != nil {
		t.Fatal(err)
	}
	if got := barrierGeneration(t, w.workspaceState); got != w.gen+1 {
		t.Fatalf("generation=%d, want %d", got, w.gen+1)
	}
	hits, err := w.store.SearchLiveText("DeletedName", 5)
	if err != nil || len(hits) != 0 {
		t.Fatalf("hits=%v err=%v", hits, err)
	}
	states, err := w.store.ListFileStates()
	if err != nil || len(states) != 1 || states[0].Path != "service.go" || !states[0].Deleted {
		t.Fatalf("states=%v err=%v", states, err)
	}
}

func TestWorkspaceBarrierSeesRenamedFileInOneGeneration(t *testing.T) {
	w := newTestWorkspace(t, "func RenamedName() {}")
	if err := os.Rename(filepath.Join(w.root, "service.go"), filepath.Join(w.root, "renamed.go")); err != nil {
		t.Fatal(err)
	}
	gen := barrierGeneration(t, w.workspaceState)
	if gen != w.gen+1 {
		t.Fatalf("generation=%d, want %d", gen, w.gen+1)
	}
	hits, err := w.store.SearchLiveText("RenamedName", 5)
	if err != nil || len(hits) != 1 || hits[0].Path != "renamed.go" || hits[0].Generation != gen {
		t.Fatalf("hits=%v err=%v", hits, err)
	}
	states, err := w.store.ListFileStates()
	if err != nil || len(states) != 2 || states[0].Path != "renamed.go" || states[0].Deleted || states[1].Path != "service.go" || !states[1].Deleted {
		t.Fatalf("states=%v err=%v", states, err)
	}
}

func TestWorkspaceStatesAreIsolated(t *testing.T) {
	first := newTestWorkspace(t, "func SharedName() {}")
	second := newTestWorkspace(t, "func SharedName() {}")
	writeWorkspaceFile(t, first.root, "service.go", "func ChangedName() {}")
	if got := barrierGeneration(t, first.workspaceState); got != first.gen+1 {
		t.Fatalf("first generation=%d, want %d", got, first.gen+1)
	}
	if got := barrierGeneration(t, second.workspaceState); got != second.gen {
		t.Fatalf("second generation=%d, want %d", got, second.gen)
	}
	hits, err := second.store.SearchLiveText("SharedName", 5)
	if err != nil || len(hits) != 1 {
		t.Fatalf("second hits=%v err=%v", hits, err)
	}
}

func TestWorkspaceBarrierTimeoutDoesNotCancelRefresh(t *testing.T) {
	w := newTestWorkspace(t, "func OldName() {}")
	writeWorkspaceFile(t, w.root, "service.go", "func FreshName() {}")
	unblock := make(chan struct{})
	original := w.enumerate
	w.enumerate = func(root, ext string) ([]string, error) {
		<-unblock
		return original(root, ext)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := w.Barrier(ctx); !errors.Is(err, ErrWorkspaceSyncing) {
		t.Fatalf("Barrier timeout err=%v, want ErrWorkspaceSyncing", err)
	}
	close(unblock)
	if got := barrierGeneration(t, w.workspaceState); got != w.gen+1 {
		t.Fatalf("generation=%d, want %d", got, w.gen+1)
	}
	hits, err := w.store.SearchLiveText("FreshName", 5)
	if err != nil || len(hits) != 1 {
		t.Fatalf("hits=%v err=%v", hits, err)
	}
}

func TestWorkspaceSnapshotStopsWaitingForUpdateLockWhenContextExpires(t *testing.T) {
	w := newTestWorkspace(t, "func CurrentName() {}")
	done := make(chan struct{})
	close(done)
	w.mu.Lock()
	w.refresh = &workspaceRefresh{done: done, generation: w.generation}
	w.mu.Unlock()

	w.updateMu.Lock()
	locked := true
	defer func() {
		if locked {
			w.updateMu.Unlock()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, unlock, err := w.Snapshot(ctx)
		if unlock != nil {
			unlock()
		}
		result <- err
	}()

	select {
	case err := <-result:
		if !errors.Is(err, ErrWorkspaceSyncing) {
			t.Fatalf("Snapshot error=%v, want ErrWorkspaceSyncing", err)
		}
	case <-time.After(200 * time.Millisecond):
		w.updateMu.Unlock()
		locked = false
		err := <-result
		t.Fatalf("Snapshot ignored context while waiting for update lock: %v", err)
	}
}

func TestWorkspaceFullRefreshRequestDuringRefreshSurvivesUntilNextBarrier(t *testing.T) {
	w := newTestWorkspace(t, "func BeforeRefreshRequest() {}")
	started := make(chan struct{})
	unblock := make(chan struct{})
	original := w.enumerate
	w.enumerate = func(root, ext string) ([]string, error) {
		close(started)
		<-unblock
		return original(root, ext)
	}

	refreshed := make(chan error, 1)
	go func() {
		_, err := w.Barrier(context.Background())
		refreshed <- err
	}()
	<-started
	w.requireFullRefresh()
	close(unblock)
	if err := <-refreshed; err != nil {
		t.Fatal(err)
	}
	if err := w.refreshPaths(nil); !errors.Is(err, ErrWorkspaceSyncing) {
		t.Fatalf("refreshPaths before next Barrier err=%v, want ErrWorkspaceSyncing", err)
	}

	w.enumerate = original
	if _, err := w.Barrier(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := w.refreshPaths(nil); err != nil {
		t.Fatalf("refreshPaths after next Barrier: %v", err)
	}
}

func TestWorkspaceFullRefreshRequestAfterFlightCreationSurvivesUntilNextBarrier(t *testing.T) {
	w := newTestWorkspace(t, "func BeforeFlightRequest() {}")
	previousProcs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previousProcs)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := w.Barrier(ctx); !errors.Is(err, ErrWorkspaceSyncing) {
		t.Fatalf("canceled Barrier err=%v, want ErrWorkspaceSyncing", err)
	}
	w.mu.Lock()
	refresh := w.refresh
	w.mu.Unlock()
	if refresh == nil {
		t.Fatal("refresh flight completed before request could be recorded")
	}
	w.requireFullRefresh()
	<-refresh.done

	if err := w.refreshPaths(nil); !errors.Is(err, ErrWorkspaceSyncing) {
		t.Fatalf("refreshPaths before next Barrier err=%v, want ErrWorkspaceSyncing", err)
	}
}

func TestWorkspaceWatcherRefreshesSavedPath(t *testing.T) {
	w := newTestWorkspace(t, "func BeforeWatch() {}")
	if err := w.StartWatcher(); err != nil {
		t.Fatal(err)
	}
	writeWorkspaceFile(t, w.root, "service.go", "func AfterWatch() {}")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		hits, err := w.store.SearchLiveText("AfterWatch", 5)
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) == 1 && hits[0].Generation == w.gen+1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("watcher did not refresh saved path")
}

func TestWorkspaceBarrierDoesNotJoinCompletedRefresh(t *testing.T) {
	w := newTestWorkspace(t, "func OldFlightName() {}")
	done := make(chan struct{})
	close(done)
	w.mu.Lock()
	w.refreshDone = done
	w.mu.Unlock()

	writeWorkspaceFile(t, w.root, "service.go", "func NewFlightName() {}")
	if got := barrierGeneration(t, w.workspaceState); got != w.gen+1 {
		t.Fatalf("generation=%d, want %d", got, w.gen+1)
	}
	hits, err := w.store.SearchLiveText("NewFlightName", 5)
	if err != nil || len(hits) != 1 {
		t.Fatalf("hits=%v err=%v", hits, err)
	}
}

func TestWorkspaceWatcherRequiresFullRefreshForRemovedDirectory(t *testing.T) {
	w := newTestWorkspace(t, "func RootName() {}")
	writeWorkspaceFile(t, w.root, "pkg/nested.go", "func NestedName() {}")
	barrierGeneration(t, w.workspaceState)
	if err := w.StartWatcher(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(w.root, "pkg")); err != nil {
		t.Fatal(err)
	}
	waitForFullRefreshRequired(t, w.workspaceState)
}

func TestWorkspaceWatcherRequiresFullRefreshForRenamedDirectory(t *testing.T) {
	w := newTestWorkspace(t, "func RootName() {}")
	writeWorkspaceFile(t, w.root, "pkg/nested.go", "func NestedName() {}")
	barrierGeneration(t, w.workspaceState)
	if err := w.StartWatcher(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(w.root, "pkg"), filepath.Join(t.TempDir(), "moved")); err != nil {
		t.Fatal(err)
	}
	waitForFullRefreshRequired(t, w.workspaceState)
}

func TestWorkspaceWatcherRefreshErrorRequiresFullRefresh(t *testing.T) {
	w := newTestWorkspace(t, "func BeforeError() {}")
	if err := w.StartWatcher(); err != nil {
		t.Fatal(err)
	}
	if err := w.store.Close(); err != nil {
		t.Fatal(err)
	}
	writeWorkspaceFile(t, w.root, "service.go", "func AfterError() {}")
	waitForFullRefreshRequired(t, w.workspaceState)
}

func TestWorkspaceCloseWaitsForInFlightRefresh(t *testing.T) {
	w := newTestWorkspace(t, "func BeforeClose() {}")
	unblock := make(chan struct{})
	original := w.enumerate
	w.enumerate = func(root, ext string) ([]string, error) {
		<-unblock
		return original(root, ext)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := w.Barrier(ctx); !errors.Is(err, ErrWorkspaceSyncing) {
		t.Fatalf("Barrier timeout err=%v, want ErrWorkspaceSyncing", err)
	}
	closed := make(chan error, 1)
	go func() { closed <- w.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned before refresh completed: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(unblock)
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not return after refresh completed")
	}
}

func TestWorkspaceGenerationContinuesFromPopulatedStore(t *testing.T) {
	root, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeWorkspaceFile(t, root, "service.go", "func OldGeneration() {}")
	db := filepath.Join(t.TempDir(), "code.db")
	store, err := codestore.Open(db, "test-model", codeEmbedDim)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutLiveFile("service.go", "old-hash", "func OldGeneration() {}", 7); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = codestore.Open(db, "test-model", codeEmbedDim)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	w, err := newWorkspaceState(root, store, []string{"."}, "go")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	writeWorkspaceFile(t, root, "service.go", "func NewGeneration() {}")
	if got := barrierGeneration(t, w); got != 8 {
		t.Fatalf("generation=%d, want 8", got)
	}
}

func waitForFullRefreshRequired(t *testing.T, w *workspaceState) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		w.mu.Lock()
		required := w.fullRefreshRequired
		w.mu.Unlock()
		if required {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("full refresh was not required")
}

func TestWorkspaceConcurrentCloseWaitsForInFlightRefresh(t *testing.T) {
	w := newTestWorkspace(t, "func ConcurrentClose() {}")
	unblock := make(chan struct{})
	original := w.enumerate
	w.enumerate = func(root, ext string) ([]string, error) {
		<-unblock
		return original(root, ext)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := w.Barrier(ctx); !errors.Is(err, ErrWorkspaceSyncing) {
		t.Fatalf("Barrier timeout err=%v, want ErrWorkspaceSyncing", err)
	}
	closed := make(chan error, 2)
	go func() { closed <- w.Close() }()
	waitForWorkspaceClosed(t, w.workspaceState)
	go func() { closed <- w.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("concurrent Close returned before refresh completed: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(unblock)
	for range 2 {
		select {
		case err := <-closed:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("Close did not return after refresh completed")
		}
	}
}

func TestWorkspaceAddWatchRootsReturnsPartialSuccess(t *testing.T) {
	root, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "valid"), 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := codestore.Open(filepath.Join(t.TempDir(), "code.db"), "test-model", codeEmbedDim)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	w, err := newWorkspaceState(root, store, []string{"valid", "missing"}, "go")
	if err != nil {
		t.Fatal(err)
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { watcher.Close() })
	added, err := w.addWatchRoots(watcher)
	if err == nil {
		t.Fatal("addWatchRoots err=nil, want missing-root error")
	}
	valid := filepath.Join(root, "valid")
	if len(watcher.WatchList()) != 1 || !added[valid] {
		t.Fatalf("watch list=%v added=%v err=%v, want valid root recorded", watcher.WatchList(), added, err)
	}
}

func waitForWorkspaceClosed(t *testing.T, w *workspaceState) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		w.mu.Lock()
		closed := w.closed
		w.mu.Unlock()
		if closed {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("workspace did not begin closing")
}

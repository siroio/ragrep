package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/siroio/ragrep/internal/config"
)

const (
	daemonAddress        = "127.0.0.1:7377"
	daemonEndpoint       = "http://" + daemonAddress
	daemonStartTimeout   = 3 * time.Second
	workspaceIdleTimeout = 30 * time.Minute
)

var ErrWorkspaceNotFound = errors.New("workspace not found")

type daemonDiscovery struct {
	Endpoint string `json:"endpoint"`
	Token    string `json:"token"`
	PID      int    `json:"pid"`
}

func daemonDiscoveryPath() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "ragrep", "daemon.json"), nil
}

func workspaceRegistryPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "ragrep", "workspaces.json"), nil
}

func newDaemonToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

func writeDaemonDiscovery(path string, discovery daemonDiscovery) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(discovery)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

func readDaemonDiscovery(path string) (daemonDiscovery, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return daemonDiscovery{}, err
	}
	var discovery daemonDiscovery
	if err := json.Unmarshal(data, &discovery); err != nil {
		return daemonDiscovery{}, err
	}
	if discovery.Endpoint == "" || discovery.Token == "" || discovery.PID <= 0 {
		return daemonDiscovery{}, errors.New("invalid daemon discovery file")
	}
	return discovery, nil
}

func clientFromDiscovery() (daemonClient, daemonDiscovery, error) {
	path, err := daemonDiscoveryPath()
	if err != nil {
		return daemonClient{}, daemonDiscovery{}, err
	}
	discovery, err := readDaemonDiscovery(path)
	if err != nil {
		return daemonClient{}, daemonDiscovery{}, err
	}
	return daemonClient{endpoint: discovery.Endpoint, token: discovery.Token}, discovery, nil
}

func listenDaemon(address string) (net.Listener, error) {
	return net.Listen("tcp", address)
}

type workspaceOpener func(string) (*workspaceState, error)

type workspaceRegistryEntry struct {
	state    *workspaceState
	explicit bool
	timer    *time.Timer
	epoch    uint64
	leases   int
}

type workspaceRegistry struct {
	mu      sync.Mutex
	file    string
	idle    time.Duration
	open    workspaceOpener
	entries map[string]*workspaceRegistryEntry
	closed  bool
}

type workspaceRegistryFile struct {
	Roots []string `json:"roots"`
}

func newWorkspaceRegistry(file string, idle time.Duration, opener workspaceOpener) (*workspaceRegistry, error) {
	if idle <= 0 {
		idle = workspaceIdleTimeout
	}
	if opener == nil {
		opener = openDaemonWorkspace
	}
	r := &workspaceRegistry{file: file, idle: idle, open: opener, entries: make(map[string]*workspaceRegistryEntry)}
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	var saved workspaceRegistryFile
	if err := json.Unmarshal(data, &saved); err != nil {
		return nil, err
	}
	for _, path := range saved.Roots {
		if path == "" {
			continue
		}
		root, err := filepath.Abs(path)
		if err == nil {
			r.entries[filepath.Clean(root)] = &workspaceRegistryEntry{explicit: true}
		}
	}
	return r, nil
}

func canonicalWorkspaceRoot(path string) (string, error) {
	if path == "" {
		path = "."
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		abs = filepath.Dir(abs)
	}
	for {
		if info, err := os.Stat(filepath.Join(abs, ".ragrep")); err == nil && info.IsDir() {
			return filepath.Clean(abs), nil
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return "", ErrWorkspaceNotFound
		}
		abs = parent
	}
}

func (r *workspaceRegistry) Add(path string) (string, error) {
	root, err := canonicalWorkspaceRoot(path)
	if err != nil {
		return "", err
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return "", errors.New("workspace registry is closed")
	}
	entry := r.entries[root]
	existed := entry != nil
	if entry == nil {
		entry = &workspaceRegistryEntry{}
		r.entries[root] = entry
	}
	oldState := entry.state
	if entry.state == nil {
		state, err := r.open(root)
		if err != nil {
			if !existed {
				delete(r.entries, root)
			}
			r.mu.Unlock()
			return "", err
		}
		entry.state = state
	}
	if entry.explicit {
		r.mu.Unlock()
		return root, nil
	}
	oldTimer := entry.timer != nil
	entry.epoch++
	if entry.timer != nil {
		entry.timer.Stop()
		entry.timer = nil
	}
	entry.explicit = true
	if err := r.persistLocked(); err != nil {
		entry.explicit = false
		var closeEntry *workspaceRegistryEntry
		if !existed {
			delete(r.entries, root)
			closeEntry = entry
		} else {
			if oldState == nil {
				closeEntry = &workspaceRegistryEntry{state: entry.state}
				entry.state = nil
			}
			if oldTimer {
				r.scheduleEvictionLocked(root, entry)
			}
		}
		r.mu.Unlock()
		_ = closeWorkspaceEntry(closeEntry)
		return "", err
	}
	r.mu.Unlock()
	return root, nil
}

func (r *workspaceRegistry) Remove(path string) (bool, error) {
	root, err := canonicalWorkspaceRoot(path)
	if err != nil {
		root, err = filepath.Abs(path)
		if err != nil {
			return false, err
		}
		root = filepath.Clean(root)
	}
	r.mu.Lock()
	entry := r.entries[root]
	if entry == nil || !entry.explicit {
		r.mu.Unlock()
		return false, nil
	}
	delete(r.entries, root)
	if err := r.persistLocked(); err != nil {
		r.entries[root] = entry
		r.mu.Unlock()
		return false, err
	}
	r.mu.Unlock()
	closeWorkspaceEntry(entry)
	return true, nil
}

func (r *workspaceRegistry) List() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.explicitRootsLocked()
}

func (r *workspaceRegistry) Resolve(path string) (*workspaceState, error) {
	root, err := canonicalWorkspaceRoot(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrWorkspaceNotFound
		}
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, errors.New("workspace registry is closed")
	}
	entry := r.entries[root]
	if entry == nil {
		entry = &workspaceRegistryEntry{}
		r.entries[root] = entry
	}
	if entry.state == nil {
		state, err := r.open(root)
		if err != nil {
			if !entry.explicit {
				delete(r.entries, root)
			}
			return nil, err
		}
		entry.state = state
	}
	r.scheduleEvictionLocked(root, entry)
	return entry.state, nil
}

func (r *workspaceRegistry) Acquire(path string) (func(), error) {
	root, err := canonicalWorkspaceRoot(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrWorkspaceNotFound
		}
		return nil, err
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, errors.New("workspace registry is closed")
	}
	entry := r.entries[root]
	if entry == nil {
		entry = &workspaceRegistryEntry{}
		r.entries[root] = entry
	}
	if entry.state == nil {
		state, err := r.open(root)
		if err != nil {
			if !entry.explicit {
				delete(r.entries, root)
			}
			r.mu.Unlock()
			return nil, err
		}
		entry.state = state
	}
	entry.leases++
	entry.epoch++
	if entry.timer != nil {
		entry.timer.Stop()
		entry.timer = nil
	}
	r.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			if r.entries[root] == entry && entry.leases > 0 {
				entry.leases--
				r.scheduleEvictionLocked(root, entry)
			}
			r.mu.Unlock()
		})
	}, nil
}

func (r *workspaceRegistry) scheduleEvictionLocked(root string, entry *workspaceRegistryEntry) {
	if entry.explicit || entry.leases > 0 {
		return
	}
	if entry.timer != nil {
		entry.timer.Stop()
	}
	entry.epoch++
	epoch := entry.epoch
	entry.timer = time.AfterFunc(r.idle, func() { r.evict(root, entry, epoch) })
}

func (r *workspaceRegistry) evict(root string, entry *workspaceRegistryEntry, epoch uint64) {
	r.mu.Lock()
	if r.entries[root] != entry || entry.explicit || entry.leases > 0 || entry.epoch != epoch || r.closed {
		r.mu.Unlock()
		return
	}
	delete(r.entries, root)
	r.mu.Unlock()
	closeWorkspaceEntry(entry)
}

func (r *workspaceRegistry) persistLocked() error {
	data, err := json.Marshal(workspaceRegistryFile{Roots: r.explicitRootsLocked()})
	if err != nil {
		return err
	}
	dir := filepath.Dir(r.file)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, "workspaces-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, r.file)
}

func (r *workspaceRegistry) explicitRootsLocked() []string {
	roots := make([]string, 0, len(r.entries))
	for root, entry := range r.entries {
		if entry.explicit {
			roots = append(roots, root)
		}
	}
	sort.Strings(roots)
	return roots
}

func (r *workspaceRegistry) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	entries := r.entries
	r.entries = make(map[string]*workspaceRegistryEntry)
	r.mu.Unlock()
	var errs []error
	for _, entry := range entries {
		if entry.timer != nil {
			entry.timer.Stop()
		}
		errs = append(errs, closeWorkspaceEntry(entry))
	}
	return errors.Join(errs...)
}

func closeWorkspaceEntry(entry *workspaceRegistryEntry) error {
	if entry == nil || entry.state == nil {
		return nil
	}
	err := entry.state.Close()
	if entry.state.store != nil {
		err = errors.Join(err, entry.state.store.Close())
	}
	return err
}

func openDaemonWorkspace(root string) (*workspaceState, error) {
	cfg, err := config.Load(root)
	if err != nil {
		return nil, err
	}
	store, err := openCodeStoreAt(filepath.Join(root, filepath.FromSlash(cfg.CodeDB)))
	if err != nil {
		return nil, err
	}
	state, err := newWorkspaceState(root, store, []string{"."}, "go")
	if err != nil {
		store.Close()
		return nil, err
	}
	if err := state.StartWatcher(); err != nil {
		state.Close()
		store.Close()
		return nil, err
	}
	return state, nil
}

func cmdDaemon(args []string) int {
	if len(args) != 1 {
		return fail(errors.New("usage: ragrep daemon start|stop|status|serve"))
	}
	switch args[0] {
	case "start":
		return daemonStart()
	case "stop":
		return daemonStop()
	case "status":
		return daemonStatusCommand()
	case "serve":
		if err := serveDaemon(); err != nil {
			return fail(err)
		}
		return 0
	default:
		return fail(errors.New("usage: ragrep daemon start|stop|status|serve"))
	}
}

func daemonStart() int {
	if client, _, err := clientFromDiscovery(); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		status, statusErr := client.Status(ctx)
		cancel()
		if statusErr == nil {
			fmt.Println(status.PID)
			return 0
		}
	}
	executable, err := os.Executable()
	if err != nil {
		return fail(err)
	}
	process, err := launchDaemonProcess(executable)
	if err != nil {
		return fail(err)
	}
	discovery, err := waitForDaemon(daemonStartTimeout)
	if err != nil {
		_ = process.Kill()
		return fail(err)
	}
	_ = process.Release()
	fmt.Println(discovery.PID)
	return 0
}

func launchDaemonProcess(executable string) (*os.Process, error) {
	cmd := newDaemonProcessCommand(executable)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd.Process, nil
}

func newDaemonProcessCommand(executable string) *exec.Cmd {
	cmd := exec.Command(executable, "daemon", "serve")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	configureDaemonProcess(cmd)
	return cmd
}

func waitForDaemon(timeout time.Duration) (daemonDiscovery, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		client, discovery, err := clientFromDiscovery()
		if err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			_, statusErr := client.Status(ctx)
			cancel()
			if statusErr == nil {
				return discovery, nil
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	return daemonDiscovery{}, errors.New("daemon did not become ready within 3 seconds")
}

func daemonStop() int {
	client, _, err := clientFromDiscovery()
	if err != nil {
		return fail(err)
	}
	discoveryPath, err := daemonDiscoveryPath()
	if err != nil {
		return fail(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), daemonStartTimeout)
	defer cancel()
	if err := client.Stop(ctx); err != nil {
		return fail(err)
	}
	if err := waitForDaemonStop(ctx, client, discoveryPath); err != nil {
		return fail(err)
	}
	return 0
}

func waitForDaemonStop(ctx context.Context, client daemonClient, discoveryPath string) error {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(discoveryPath); errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if _, err := client.Status(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func daemonStatusCommand() int {
	client, _, err := clientFromDiscovery()
	if err != nil {
		return fail(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	status, err := client.Status(ctx)
	if err != nil {
		return fail(err)
	}
	fmt.Printf("%s %d\n", status.Status, status.PID)
	return 0
}

func serveDaemon() error {
	discoveryPath, err := daemonDiscoveryPath()
	if err != nil {
		return err
	}
	if client, _, err := clientFromDiscovery(); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		_, statusErr := client.Status(ctx)
		cancel()
		if statusErr == nil {
			return errors.New("daemon is already running")
		}
	}
	listener, err := listenDaemon(daemonAddress)
	if err != nil {
		return fmt.Errorf("daemon already running or address unavailable: %w", err)
	}
	defer listener.Close()
	registryPath, err := workspaceRegistryPath()
	if err != nil {
		return err
	}
	registry, err := newWorkspaceRegistry(registryPath, workspaceIdleTimeout, nil)
	if err != nil {
		return err
	}
	defer registry.Close()
	service := newCodeService(registry.Resolve, nil, nil)
	defer service.Close()
	token, err := newDaemonToken()
	if err != nil {
		return err
	}
	discovery := daemonDiscovery{Endpoint: daemonEndpoint, Token: token, PID: os.Getpid()}
	if err := writeDaemonDiscovery(discoveryPath, discovery); err != nil {
		return err
	}
	defer os.Remove(discoveryPath)
	stop := make(chan struct{}, 1)
	server := newDaemonHTTPServer(newDaemonServerHandler(service, token, registry, func() {
		select {
		case stop <- struct{}{}:
		default:
		}
	}))
	go func() {
		<-stop
		shutdownDaemonServer(server, daemonStartTimeout)
	}()
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func newDaemonHTTPServer(handler http.Handler) *http.Server {
	return &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
}

func shutdownDaemonServer(server *http.Server, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		_ = server.Close()
	}
}

func cmdWorkspace(args []string) int {
	if len(args) == 0 {
		return fail(errors.New("usage: ragrep workspace add|remove|list [path]"))
	}
	client, _, err := clientFromDiscovery()
	if err != nil {
		return fail(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), daemonStartTimeout)
	defer cancel()
	switch args[0] {
	case "add":
		if len(args) > 2 {
			return fail(errors.New("usage: ragrep workspace add [path]"))
		}
		path := "."
		if len(args) == 2 {
			path = args[1]
		}
		root, err := client.WorkspaceAdd(ctx, path)
		if err != nil {
			return fail(err)
		}
		fmt.Println(root)
		return 0
	case "remove":
		if len(args) != 2 {
			return fail(errors.New("usage: ragrep workspace remove [path]"))
		}
		if err := client.WorkspaceRemove(ctx, args[1]); err != nil {
			var apiErr *apiError
			if errors.As(err, &apiErr) && apiErr.Code == "workspace_not_found" {
				fmt.Fprintln(os.Stderr, apiErr.Message)
				return 2
			}
			return fail(err)
		}
		return 0
	case "list":
		if len(args) != 1 {
			return fail(errors.New("usage: ragrep workspace list"))
		}
		roots, err := client.WorkspaceList(ctx)
		if err != nil {
			return fail(err)
		}
		if len(roots) == 0 {
			return 2
		}
		for _, root := range roots {
			fmt.Println(root)
		}
		return 0
	default:
		return fail(errors.New("usage: ragrep workspace add|remove|list [path]"))
	}
}

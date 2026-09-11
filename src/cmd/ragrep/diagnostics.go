package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/siroio/ragrep/internal/config"
	"github.com/siroio/ragrep/internal/embed"
	"github.com/siroio/ragrep/internal/store"
)

type doctorCheck struct {
	Name     string `json:"name"`
	Status   string `json:"status"`
	Detail   string `json:"detail,omitempty"`
	Recovery string `json:"recovery,omitempty"`
}

type doctorReport struct {
	Build     buildMetadata  `json:"build"`
	Workspace string         `json:"workspace"`
	DB        string         `json:"db"`
	Expected  doctorExpected `json:"expected"`
	Checks    []doctorCheck  `json:"checks"`
}

type doctorExpected struct {
	SchemaVersion int    `json:"schema_version"`
	Embedding     string `json:"embedding_identity"`
	Runtime       string `json:"runtime"`
	CacheDir      string `json:"cache_dir"`
}

func cmdDoctor(ctx context.Context, args []string) int {
	fs := newFlagSet("doctor")
	db := fs.String("db", "", "index database path")
	asJSON := fs.Bool("json", false, "JSON output")
	if code, handled := parseArgsUsage(fs, args, "Usage:\n  ragrep doctor [--db PATH] [--json]\n"); handled {
		return code
	}
	if fs.NArg() != 0 {
		return fail(errors.New("usage: ragrep doctor [--db PATH] [--json]"))
	}
	report := runDoctor(ctx, *db)
	if *asJSON {
		if err := json.NewEncoder(os.Stdout).Encode(report); err != nil {
			return fail(err)
		}
	} else {
		printDoctor(report)
	}
	for _, check := range report.Checks {
		if check.Status == "fail" {
			return 1
		}
	}
	return 0
}

func runDoctor(ctx context.Context, dbOverride string) doctorReport {
	root, dbPath, cfg, cfgErr := resolveDoctorPaths(dbOverride)
	cacheDir := doctorCacheDir()
	report := doctorReport{
		Build: currentBuildMetadata(), Workspace: root, DB: dbPath,
		Expected: doctorExpected{SchemaVersion: store.SchemaVersion(), Embedding: store.EmbeddingIdentity(), Runtime: embed.RuntimeIdentity() + " (" + runtime.GOOS + "/" + runtime.GOARCH + ")", CacheDir: cacheDir},
	}
	if cfgErr != nil {
		report.Checks = append(report.Checks, doctorCheck{Name: "config", Status: "fail", Detail: "configuration could not be read", Recovery: "fix .ragrep/config.json and run ragrep doctor again"})
	} else if cfg == nil {
		report.Checks = append(report.Checks, doctorCheck{Name: "config", Status: "warn", Detail: "workspace configuration is absent", Recovery: "run 'ragrep init' in the workspace"})
	} else if _, err := os.Stat(filepath.Join(root, ".ragrep", "config.json")); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			report.Checks = append(report.Checks, doctorCheck{Name: "config", Status: "warn", Detail: "workspace configuration is absent", Recovery: "run 'ragrep init' in the workspace"})
		} else {
			report.Checks = append(report.Checks, doctorCheck{Name: "config", Status: "fail", Detail: "configuration file cannot be inspected", Recovery: "check .ragrep/config.json permissions"})
		}
	} else {
		report.Checks = append(report.Checks, doctorCheck{Name: "config", Status: "ok", Detail: "configuration loaded"})
	}

	if _, err := os.Stat(dbPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			report.Checks = append(report.Checks, doctorCheck{Name: "document index", Status: "fail", Detail: "database is missing", Recovery: "run 'ragrep init' or index into this path"})
		} else {
			report.Checks = append(report.Checks, doctorCheck{Name: "document index", Status: "fail", Detail: "database cannot be inspected", Recovery: "check the database path permissions"})
		}
	} else if err := store.Check(dbPath); err != nil {
		detail, recovery := "database is incompatible", "create a fresh database with 'ragrep index --db <new-index.db>' and reindex the documents"
		if errors.Is(err, store.ErrNotDocumentDatabase) {
			detail, recovery = "file is not a ragrep document database", "choose a new database path and run 'ragrep index --db <new-index.db>'"
		}
		report.Checks = append(report.Checks, doctorCheck{Name: "document index", Status: "fail", Detail: detail, Recovery: recovery})
	} else {
		report.Checks = append(report.Checks, doctorCheck{Name: "document index", Status: "ok", Detail: "schema and embedding metadata are compatible"})
	}

	if cacheDir == "" || !embed.ModelCached(cacheDir) {
		report.Checks = append(report.Checks, doctorCheck{Name: "embedding assets", Status: "fail", Detail: "expected model/runtime assets are missing or invalid", Recovery: "run 'ragrep init' to download the pinned assets"})
	} else {
		report.Checks = append(report.Checks, doctorCheck{Name: "embedding assets", Status: "ok", Detail: "pinned model/runtime assets are cached"})
	}

	report.Checks = append(report.Checks, checkDaemon(ctx))
	report.Checks = append(report.Checks, checkServers(cfg)...)
	return report
}

func doctorCacheDir() string {
	base, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(base, "ragrep")
}

func resolveDoctorPaths(dbOverride string) (string, string, *config.Config, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return ".", filepath.FromSlash(config.DefaultDB), nil, err
	}
	if dbOverride != "" || os.Getenv("RAGREP_DB") != "" {
		db := dbOverride
		if db == "" {
			db = os.Getenv("RAGREP_DB")
		}
		abs, err := filepath.Abs(db)
		if err != nil {
			return cwd, db, nil, err
		}
		root, rootErr := workspaceRoot(abs)
		if rootErr != nil {
			root = discoverWorkspaceRoot(cwd)
			if root == "" {
				root = filepath.Dir(abs)
			}
		}
		cfgRoot := discoverWorkspaceRoot(root)
		if cfgRoot == root {
			cfg, cfgErr := config.Load(root)
			if cfgErr != nil {
				return root, abs, nil, cfgErr
			}
			return root, abs, &cfg, nil
		}
		return root, abs, nil, nil
	}
	root := discoverWorkspaceRoot(cwd)
	if root == "" {
		return cwd, filepath.Join(cwd, filepath.FromSlash(config.DefaultDB)), nil, nil
	}
	cfg, err := config.Load(root)
	if err != nil {
		return root, filepath.Join(root, filepath.FromSlash(config.DefaultDB)), nil, err
	}
	db := cfg.DB
	if !filepath.IsAbs(db) {
		db = filepath.Join(root, filepath.FromSlash(db))
	}
	db, err = filepath.Abs(db)
	if err != nil {
		return root, filepath.Join(root, filepath.FromSlash(config.DefaultDB)), &cfg, err
	}
	return root, db, &cfg, nil
}

func discoverWorkspaceRoot(start string) string {
	dir, err := filepath.Abs(start)
	if err != nil {
		return ""
	}
	for {
		if info, err := os.Stat(filepath.Join(dir, ".ragrep")); err == nil && info.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

func checkServers(cfg *config.Config) []doctorCheck {
	if cfg == nil {
		return nil
	}
	if len(cfg.Servers) == 0 {
		return []doctorCheck{{Name: "language servers", Status: "warn", Detail: "none configured (optional)", Recovery: "add an executable under servers in .ragrep/config.json when code retrieval is needed"}}
	}
	languages := make([]string, 0, len(cfg.Servers))
	for lang := range cfg.Servers {
		languages = append(languages, lang)
	}
	sort.Strings(languages)
	checks := make([]doctorCheck, 0, len(languages))
	for _, lang := range languages {
		command := cfg.Servers[lang]
		if command == "" {
			checks = append(checks, doctorCheck{Name: "language server " + lang, Status: "warn", Detail: "configured executable is empty", Recovery: "set the server executable in .ragrep/config.json"})
			continue
		}
		if _, err := exec.LookPath(command); err != nil {
			checks = append(checks, doctorCheck{Name: "language server " + lang, Status: "warn", Detail: "configured executable is unavailable", Recovery: "install the optional server or update .ragrep/config.json"})
		} else {
			checks = append(checks, doctorCheck{Name: "language server " + lang, Status: "ok", Detail: "configured executable is available"})
		}
	}
	return checks
}

func checkDaemon(ctx context.Context) doctorCheck {
	recovery := "start 'ragrep daemon serve' or retry after starting the daemon"
	path, err := daemonDiscoveryPath()
	if err != nil {
		return doctorCheck{Name: "daemon", Status: "warn", Detail: "unreachable", Recovery: recovery}
	}
	discovery, err := readDaemonDiscovery(path)
	if err != nil || !safeDaemonEndpoint(discovery.Endpoint) {
		return doctorCheck{Name: "daemon", Status: "warn", Detail: "unreachable", Recovery: recovery}
	}
	client := daemonClient{endpoint: discovery.Endpoint, token: discovery.Token, client: &http.Client{Timeout: 300 * time.Millisecond, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	status, err := client.Status(ctx)
	if err != nil || status.Status != "running" {
		return doctorCheck{Name: "daemon", Status: "warn", Detail: "unreachable", Recovery: recovery}
	}
	return doctorCheck{Name: "daemon", Status: "ok", Detail: "reachable"}
}

func safeDaemonEndpoint(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	portNumber, err := strconv.Atoi(port)
	return ip != nil && ip.IsLoopback() && err == nil && portNumber > 0 && portNumber <= 65535
}

func printDoctor(report doctorReport) {
	fmt.Printf("ragrep doctor\nversion: %s\nworkspace: %s\ndb: %s\n", report.Build.Version, report.Workspace, report.DB)
	for _, check := range report.Checks {
		fmt.Printf("[%s] %s: %s\n", strings.ToUpper(check.Status), check.Name, check.Detail)
		if check.Recovery != "" {
			fmt.Printf("  recovery: %s\n", check.Recovery)
		}
	}
}

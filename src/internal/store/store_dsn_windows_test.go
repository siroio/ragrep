//go:build windows

package store

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStoreDSNUsesSandboxVFSAndEscapesWindowsPath(t *testing.T) {
	tests := []struct {
		name             string
		path             string
		wantDecodedPath  string
		wantEncodedParts []string
	}{
		{
			name:             "ordinary local path",
			path:             filepath.Join(`C:\tmp`, `folder with space`, `日本語&name=#part.db`),
			wantDecodedPath:  filepath.ToSlash(filepath.Join(`C:\tmp`, `folder with space`, `日本語&name=#part.db`)),
			wantEncodedParts: []string{"%20", "%23"},
		},
		{
			name:             "literal question mark and percent stay in path",
			path:             `C:\tmp\folder with space\100% ready\literal?value%.db`,
			wantDecodedPath:  `C:/tmp/folder with space/100% ready/literal?value%.db`,
			wantEncodedParts: []string{"%3F", "%25"},
		},
		{
			name:             "extended-length local drive path strips prefix",
			path:             `\\?\C:\tmp\extended path\literal?value%.db`,
			wantDecodedPath:  `C:/tmp/extended path/literal?value%.db`,
			wantEncodedParts: []string{"%3F", "%25"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dsn := storeDSN(tt.path)
			if !strings.HasPrefix(dsn, "file:") {
				t.Fatalf("storeDSN() = %q, want file: URI", dsn)
			}
			if strings.Count(dsn, "?") != 1 {
				t.Fatalf("storeDSN() = %q, want exactly one query delimiter", dsn)
			}

			rawPath, rawQuery, ok := strings.Cut(strings.TrimPrefix(dsn, "file:"), "?")
			if !ok {
				t.Fatalf("storeDSN() = %q, want query string", dsn)
			}
			if strings.Contains(rawPath, " ") || strings.Contains(rawPath, "#") {
				t.Fatalf("storeDSN() path segment = %q, want URI-escaped path", rawPath)
			}
			for _, want := range tt.wantEncodedParts {
				if !strings.Contains(rawPath, want) {
					t.Fatalf("storeDSN() path segment = %q, want encoded fragment %q", rawPath, want)
				}
			}

			decodedPath, err := url.PathUnescape(rawPath)
			if err != nil {
				t.Fatalf("PathUnescape(%q): %v", rawPath, err)
			}
			if decodedPath != tt.wantDecodedPath {
				t.Fatalf("decoded path = %q, want %q", decodedPath, tt.wantDecodedPath)
			}
			if strings.HasPrefix(decodedPath, `//?/`) {
				t.Fatalf("decoded path = %q, want SQLite local-drive path without extended-length prefix", decodedPath)
			}

			query, err := url.ParseQuery(rawQuery)
			if err != nil {
				t.Fatalf("ParseQuery(%q): %v", rawQuery, err)
			}
			if got := query.Get("vfs"); got != sandboxVFSName {
				t.Fatalf("vfs = %q, want %q", got, sandboxVFSName)
			}
			if got := query["_pragma"]; len(got) != 1 || got[0] != "busy_timeout(5000)" {
				t.Fatalf("_pragma = %v, want [busy_timeout(5000)]", got)
			}
		})
	}
}

func TestOpenCreatesAndReopensStoreWithWindowsSafeURIPath(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "dir with space", "日本語&db=name#.sqlite")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", filepath.Dir(dbPath), err)
	}

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open(%q): %v", dbPath, err)
	}
	if _, err := s.UpsertDoc("docs/auth.md", "retry with backoff", 123, fakeEmbed); err != nil {
		s.Close()
		t.Fatalf("UpsertDoc(): %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close(): %v", err)
	}

	reopened, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopen Open(%q): %v", dbPath, err)
	}
	t.Cleanup(func() { reopened.Close() })

	doc, err := reopened.GetDoc("docs/auth.md")
	if err != nil {
		t.Fatalf("GetDoc(): %v", err)
	}
	if doc != "retry with backoff" {
		t.Fatalf("GetDoc() = %q, want stored content", doc)
	}

	hits, err := reopened.SearchVector(mustFakeEmbed(t, "title: none | text: retry with backoff"), 1, nil)
	if err != nil {
		t.Fatalf("SearchVector(): %v", err)
	}
	if len(hits) != 1 || hits[0].Doc != "docs/auth.md" {
		t.Fatalf("SearchVector() hits = %+v, want persisted sqlite-vec row", hits)
	}
}

func TestOpenCreatesAndReopensStoreWithExtendedLengthLocalDrivePath(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "extended path", "db 100 percent.sqlite")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", filepath.Dir(dbPath), err)
	}
	absPath, err := filepath.Abs(dbPath)
	if err != nil {
		t.Fatalf("Abs(%q): %v", dbPath, err)
	}
	if len(filepath.VolumeName(absPath)) != 2 {
		t.Skipf("test path is not on a local drive: %q", absPath)
	}
	extendedPath := `\\?\` + absPath

	s, err := Open(extendedPath)
	if err != nil {
		t.Fatalf("Open(%q): %v", extendedPath, err)
	}
	if _, err := s.UpsertDoc("docs/extended.md", "extended path reopen", 456, fakeEmbed); err != nil {
		s.Close()
		t.Fatalf("UpsertDoc(): %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close(): %v", err)
	}

	reopened, err := Open(extendedPath)
	if err != nil {
		t.Fatalf("reopen Open(%q): %v", extendedPath, err)
	}
	t.Cleanup(func() { reopened.Close() })

	doc, err := reopened.GetDoc("docs/extended.md")
	if err != nil {
		t.Fatalf("GetDoc(): %v", err)
	}
	if doc != "extended path reopen" {
		t.Fatalf("GetDoc() = %q, want stored content", doc)
	}
}

func mustFakeEmbed(t *testing.T, text string) []float32 {
	t.Helper()
	v, err := fakeEmbed(text)
	if err != nil {
		t.Fatalf("fakeEmbed(%q): %v", text, err)
	}
	return v
}

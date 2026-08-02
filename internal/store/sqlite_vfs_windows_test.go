//go:build windows

package store

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ncruces/go-sqlite3/vfs"
	"golang.org/x/sys/windows"
)

var sandboxVFSRegistryTestMu sync.Mutex

func TestSandboxVFSIsRegisteredByName(t *testing.T) {
	got := vfs.Find(sandboxVFSName)
	if got == nil {
		t.Fatalf("vfs.Find(%q) returned nil", sandboxVFSName)
	}
	if _, ok := got.(vfs.VFSFilename); !ok {
		t.Fatalf("vfs.Find(%q) returned %T, want vfs.VFSFilename", sandboxVFSName, got)
	}
}

func TestOpenUsesRegisteredSandboxVFS(t *testing.T) {
	sandboxVFSRegistryTestMu.Lock()
	t.Cleanup(sandboxVFSRegistryTestMu.Unlock)

	original := vfs.Find(sandboxVFSName)
	if original == nil {
		t.Fatalf("vfs.Find(%q) returned nil", sandboxVFSName)
	}
	originalFilenameVFS, ok := original.(vfs.VFSFilename)
	if !ok {
		t.Fatalf("vfs.Find(%q) returned %T, want vfs.VFSFilename", sandboxVFSName, original)
	}

	observer := &observingSandboxVFS{VFSFilename: originalFilenameVFS}
	vfs.Register(sandboxVFSName, observer)
	t.Cleanup(func() {
		vfs.Register(sandboxVFSName, original)
	})

	dbPath := filepath.Join(t.TempDir(), "observed.sqlite")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open(%q): %v", dbPath, err)
	}
	t.Cleanup(func() { store.Close() })

	fullPathnameCalls, openFilenameCalls := observer.counts()
	if fullPathnameCalls == 0 && openFilenameCalls == 0 {
		t.Fatalf("store.Open(%q) did not traverse %q: FullPathname=%d OpenFilename=%d", dbPath, sandboxVFSName, fullPathnameCalls, openFilenameCalls)
	}

	vfs.Register(sandboxVFSName, original)
	if restored := vfs.Find(sandboxVFSName); restored != original {
		t.Fatalf("vfs.Find(%q) after restore = %T, want exact original registration %T", sandboxVFSName, restored, original)
	}
}

func TestOpenThroughAncestorJunctionUsesCanonicalTargetPaths(t *testing.T) {
	sandboxVFSRegistryTestMu.Lock()
	t.Cleanup(sandboxVFSRegistryTestMu.Unlock)

	original := vfs.Find(sandboxVFSName)
	if original == nil {
		t.Fatalf("vfs.Find(%q) returned nil", sandboxVFSName)
	}
	originalFilenameVFS, ok := original.(vfs.VFSFilename)
	if !ok {
		t.Fatalf("vfs.Find(%q) returned %T, want vfs.VFSFilename", sandboxVFSName, original)
	}

	observer := &observingSandboxVFS{VFSFilename: originalFilenameVFS}
	vfs.Register(sandboxVFSName, observer)
	t.Cleanup(func() {
		vfs.Register(sandboxVFSName, original)
	})

	targetRoot := filepath.Join(t.TempDir(), "junction-target")
	parent := filepath.Join(targetRoot, "real-parent")
	if err := os.MkdirAll(parent, 0o755); err != nil {
		t.Fatalf("create real parent: %v", err)
	}
	junction := filepath.Join(t.TempDir(), "junction-parent")
	createSandboxDirectoryJunction(t, junction, targetRoot)

	dbPath := filepath.Join(junction, "real-parent", "observed.sqlite")
	wantDBPath := filepath.Join(parent, "observed.sqlite")
	requireApprovedSandboxRecoveryTrigger(t, dbPath)
	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open(%q): %v", dbPath, err)
	}
	if _, err := store.UpsertDoc("docs/junction.md", "retry with backoff", 123, fakeEmbed); err != nil {
		store.Close()
		t.Fatalf("UpsertDoc(): %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close(): %v", err)
	}

	reopened, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopen Open(%q): %v", dbPath, err)
	}
	t.Cleanup(func() { reopened.Close() })

	doc, err := reopened.GetDoc("docs/junction.md")
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
	if len(hits) != 1 || hits[0].Doc != "docs/junction.md" {
		t.Fatalf("SearchVector() hits = %+v, want persisted sqlite-vec row", hits)
	}

	events := observer.openEvents()
	assertObservedCanonicalMainDBPath(t, events, wantDBPath)
	assertObservedCanonicalSidecarPaths(t, events, wantDBPath)
}

func TestSandboxVFSDelegatesVFSOperations(t *testing.T) {
	wantFile := &sandboxVFSFileStub{}
	wantErr := errors.New("delegate error")
	wantFlags := vfs.OPEN_READWRITE | vfs.OPEN_MAIN_DB
	base := &sandboxVFSDelegateStub{
		fullPath:          "delegated/full/path",
		fullPathErr:       wantErr,
		openFile:          wantFile,
		openFlags:         wantFlags,
		openErr:           wantErr,
		openFilenameFile:  wantFile,
		openFilenameFlags: wantFlags,
		openFilenameErr:   wantErr,
		accessOK:          true,
		accessErr:         wantErr,
		deleteErr:         wantErr,
	}
	adapter := sandboxVFS{VFSFilename: base}

	path, err := adapter.FullPathname("input/path")
	if path != base.fullPath || !errors.Is(err, wantErr) {
		t.Fatalf("FullPathname() = %q, %v; want %q, %v", path, err, base.fullPath, wantErr)
	}
	if base.fullPathName != "input/path" {
		t.Fatalf("FullPathname delegated name %q, want %q", base.fullPathName, "input/path")
	}

	file, flags, err := adapter.Open("database", vfs.OPEN_READONLY)
	if file != wantFile || flags != wantFlags || !errors.Is(err, wantErr) {
		t.Fatalf("Open() = %p, %v, %v; want %p, %v, %v", file, flags, err, wantFile, wantFlags, wantErr)
	}
	if base.openName != "database" || base.openInputFlags != vfs.OPEN_READONLY {
		t.Fatalf("Open delegated (%q, %v), want (%q, %v)", base.openName, base.openInputFlags, "database", vfs.OPEN_READONLY)
	}

	filenameFile, filenameFlags, filenameErr := adapter.OpenFilename(nil, vfs.OPEN_CREATE)
	if filenameFile != wantFile || filenameFlags != wantFlags || !errors.Is(filenameErr, wantErr) {
		t.Fatalf("OpenFilename() = %p, %v, %v; want %p, %v, %v", filenameFile, filenameFlags, filenameErr, wantFile, wantFlags, wantErr)
	}
	if base.openFilenameName != nil || base.openFilenameInputFlags != vfs.OPEN_CREATE {
		t.Fatalf("OpenFilename delegated (%v, %v), want (nil, %v)", base.openFilenameName, base.openFilenameInputFlags, vfs.OPEN_CREATE)
	}

	ok, err := adapter.Access("database", vfs.ACCESS_READWRITE)
	if ok != base.accessOK || !errors.Is(err, wantErr) {
		t.Fatalf("Access() = %v, %v; want %v, %v", ok, err, base.accessOK, wantErr)
	}
	if base.accessName != "database" || base.accessFlags != vfs.ACCESS_READWRITE {
		t.Fatalf("Access delegated (%q, %v), want (%q, %v)", base.accessName, base.accessFlags, "database", vfs.ACCESS_READWRITE)
	}

	if err := adapter.Delete("database", true); !errors.Is(err, wantErr) {
		t.Fatalf("Delete() = %v, want %v", err, wantErr)
	}
	if base.deleteName != "database" || !base.deleteSyncDir {
		t.Fatalf("Delete delegated (%q, %v), want (%q, %v)", base.deleteName, base.deleteSyncDir, "database", true)
	}
}

func TestSandboxVFSFullPathnameKeepsSuccessfulDelegateResult(t *testing.T) {
	dir := sandboxTestDirectory(t)
	wantPath := filepath.Join(dir, "new.sqlite")
	base := &sandboxVFSDelegateStub{fullPath: wantPath}
	recoveryCalls := 0
	adapter := sandboxVFS{
		VFSFilename: base,
		recoverPath: func(string) (string, error) {
			recoveryCalls++
			return "recovered/result", nil
		},
	}

	got, err := adapter.FullPathname("input/path")
	if err != nil {
		t.Fatalf("FullPathname() error = %v, want nil", err)
	}
	if got != wantPath {
		t.Fatalf("FullPathname() = %q, want %q", got, wantPath)
	}
	if recoveryCalls != 0 {
		t.Fatalf("recovery called %d times, want 0", recoveryCalls)
	}
}

func TestSandboxVFSFullPathnamePreservesNonPermissionError(t *testing.T) {
	wantErr := errors.New("non-permission failure")
	base := &sandboxVFSDelegateStub{
		fullPath:    "delegate/result",
		fullPathErr: wantErr,
	}
	recoveryCalls := 0
	adapter := sandboxVFS{
		VFSFilename: base,
		recoverPath: func(string) (string, error) {
			recoveryCalls++
			return "recovered/result", nil
		},
	}

	got, err := adapter.FullPathname("input/path")
	if got != "delegate/result" {
		t.Fatalf("FullPathname() = %q, want %q", got, "delegate/result")
	}
	if err != wantErr {
		t.Fatalf("FullPathname() error = %v, want exact error %v", err, wantErr)
	}
	if recoveryCalls != 0 {
		t.Fatalf("recovery called %d times, want 0", recoveryCalls)
	}
}

func TestSandboxVFSFullPathnameRecoversAccessDenied(t *testing.T) {
	const input = "input/path"
	const wantPath = "recovered/path"
	base := &sandboxVFSDelegateStub{
		fullPath:    "delegate/result",
		fullPathErr: fmt.Errorf("sandbox: %w", windows.ERROR_ACCESS_DENIED),
	}
	var recoveredName string
	adapter := sandboxVFS{
		VFSFilename: base,
		recoverPath: func(name string) (string, error) {
			recoveredName = name
			return wantPath, nil
		},
	}

	got, err := adapter.FullPathname(input)
	if err != nil {
		t.Fatalf("FullPathname() error = %v, want nil", err)
	}
	if got != wantPath {
		t.Fatalf("FullPathname() = %q, want %q", got, wantPath)
	}
	if recoveredName != input {
		t.Fatalf("recovery received %q, want %q", recoveredName, input)
	}
}

func TestSandboxVFSFullPathnameRecoversPinnedOKSymlink(t *testing.T) {
	const input = "input/path"
	const wantPath = "recovered/path"
	base := &sandboxVFSDelegateStub{
		fullPath:    "delegate/result",
		fullPathErr: sandboxVFSPrivateErrorCode(sandboxPinnedSQLiteOKSymlink),
	}
	var recoveredName string
	adapter := sandboxVFS{
		VFSFilename: base,
		recoverPath: func(name string) (string, error) {
			recoveredName = name
			return wantPath, nil
		},
	}

	got, err := adapter.FullPathname(input)
	if err != nil {
		t.Fatalf("FullPathname() error = %v, want nil", err)
	}
	if got != wantPath {
		t.Fatalf("FullPathname() = %q, want %q", got, wantPath)
	}
	if recoveredName != input {
		t.Fatalf("recovery received %q, want %q", recoveredName, input)
	}
}

func TestSandboxVFSFullPathnameRecoversPathNotFoundWhenRecoverySucceeds(t *testing.T) {
	const input = "input/path"
	const wantPath = "recovered/path"
	base := &sandboxVFSDelegateStub{
		fullPath:    "delegate/result",
		fullPathErr: windows.ERROR_PATH_NOT_FOUND,
	}
	recoveryCalls := 0
	var recoveredName string
	adapter := sandboxVFS{
		VFSFilename: base,
		recoverPath: func(name string) (string, error) {
			recoveryCalls++
			recoveredName = name
			return wantPath, nil
		},
	}

	got, err := adapter.FullPathname(input)
	if err != nil {
		t.Fatalf("FullPathname() error = %v, want nil", err)
	}
	if got != wantPath {
		t.Fatalf("FullPathname() = %q, want %q", got, wantPath)
	}
	if recoveryCalls != 1 {
		t.Fatalf("recovery called %d times, want 1", recoveryCalls)
	}
	if recoveredName != input {
		t.Fatalf("recovery received %q, want %q", recoveredName, input)
	}
}

func TestSandboxVFSFullPathnamePreservesOriginalPathNotFoundWhenRecoveryFails(t *testing.T) {
	wantErr := windows.ERROR_PATH_NOT_FOUND
	base := &sandboxVFSDelegateStub{
		fullPath:    "delegate/result",
		fullPathErr: wantErr,
	}
	recoveryCalls := 0
	recoveryErr := errors.New("recovery failed")
	adapter := sandboxVFS{
		VFSFilename: base,
		recoverPath: func(string) (string, error) {
			recoveryCalls++
			return "recovered/result", recoveryErr
		},
	}

	got, err := adapter.FullPathname("input/path")
	if got != "delegate/result" {
		t.Fatalf("FullPathname() = %q, want %q", got, "delegate/result")
	}
	if err != wantErr {
		t.Fatalf("FullPathname() error = %v, want exact original error %v", err, wantErr)
	}
	if recoveryCalls != 1 {
		t.Fatalf("recovery called %d times, want 1", recoveryCalls)
	}
}

func TestSandboxVFSFullPathnamePreservesDirectInt512Error(t *testing.T) {
	wantErr := sandboxVFSIntErrorCode(512)
	base := &sandboxVFSDelegateStub{
		fullPath:    "delegate/result",
		fullPathErr: wantErr,
	}
	recoveryCalls := 0
	adapter := sandboxVFS{
		VFSFilename: base,
		recoverPath: func(string) (string, error) {
			recoveryCalls++
			return "recovered/result", nil
		},
	}

	got, err := adapter.FullPathname("input/path")
	if got != "delegate/result" {
		t.Fatalf("FullPathname() = %q, want %q", got, "delegate/result")
	}
	if err != wantErr {
		t.Fatalf("FullPathname() error = %v, want exact error %v", err, wantErr)
	}
	if recoveryCalls != 0 {
		t.Fatalf("recovery called %d times, want 0", recoveryCalls)
	}
}

func TestSandboxVFSFullPathnamePreservesFileNotFoundError(t *testing.T) {
	wantErr := windows.ERROR_FILE_NOT_FOUND
	base := &sandboxVFSDelegateStub{
		fullPath:    "delegate/result",
		fullPathErr: wantErr,
	}
	recoveryCalls := 0
	adapter := sandboxVFS{
		VFSFilename: base,
		recoverPath: func(string) (string, error) {
			recoveryCalls++
			return "recovered/result", nil
		},
	}

	got, err := adapter.FullPathname("input/path")
	if got != "delegate/result" {
		t.Fatalf("FullPathname() = %q, want %q", got, "delegate/result")
	}
	if err != wantErr {
		t.Fatalf("FullPathname() error = %v, want exact error %v", err, wantErr)
	}
	if recoveryCalls != 0 {
		t.Fatalf("recovery called %d times, want 0", recoveryCalls)
	}
}

func TestSandboxVFSFullPathnamePreservesDirectUint64Error(t *testing.T) {
	wantErr := sandboxVFSUint64ErrorCode(512)
	base := &sandboxVFSDelegateStub{
		fullPath:    "delegate/result",
		fullPathErr: wantErr,
	}
	recoveryCalls := 0
	adapter := sandboxVFS{
		VFSFilename: base,
		recoverPath: func(string) (string, error) {
			recoveryCalls++
			return "recovered/result", nil
		},
	}

	got, err := adapter.FullPathname("input/path")
	if got != "delegate/result" {
		t.Fatalf("FullPathname() = %q, want %q", got, "delegate/result")
	}
	if err != wantErr {
		t.Fatalf("FullPathname() error = %v, want exact error %v", err, wantErr)
	}
	if recoveryCalls != 0 {
		t.Fatalf("recovery called %d times, want 0", recoveryCalls)
	}
}

func TestSandboxVFSFullPathnamePreservesPointerShapedUint32Error(t *testing.T) {
	wantCode := sandboxVFSPointerErrorCode(512)
	wantErr := &wantCode
	base := &sandboxVFSDelegateStub{
		fullPath:    "delegate/result",
		fullPathErr: wantErr,
	}
	recoveryCalls := 0
	adapter := sandboxVFS{
		VFSFilename: base,
		recoverPath: func(string) (string, error) {
			recoveryCalls++
			return "recovered/result", nil
		},
	}

	got, err := adapter.FullPathname("input/path")
	if got != "delegate/result" {
		t.Fatalf("FullPathname() = %q, want %q", got, "delegate/result")
	}
	if err != wantErr {
		t.Fatalf("FullPathname() error = %v, want exact error %v", err, wantErr)
	}
	if recoveryCalls != 0 {
		t.Fatalf("recovery called %d times, want 0", recoveryCalls)
	}
}

func TestSandboxVFSFullPathnameUsesDefaultRecovery(t *testing.T) {
	dir := sandboxTestDirectory(t)
	db := filepath.Join(dir, "existing.sqlite")
	if err := os.WriteFile(db, []byte("database"), 0o600); err != nil {
		t.Fatalf("create database: %v", err)
	}

	base := &sandboxVFSDelegateStub{fullPathErr: windows.ERROR_ACCESS_DENIED}
	adapter := sandboxVFS{VFSFilename: base}
	got, err := adapter.FullPathname(db)
	if err != nil {
		t.Fatalf("FullPathname() error = %v, want nil", err)
	}
	assertSandboxPathEqual(t, got, sandboxExpectedCanonicalPath(t, db))
}

func TestSandboxVFSFullPathnameRecoversOSVFSSymlinkResultThroughAncestorJunction(t *testing.T) {
	targetRoot := filepath.Join(t.TempDir(), "junction-target")
	parent := filepath.Join(targetRoot, "real-parent")
	if err := os.MkdirAll(parent, 0o755); err != nil {
		t.Fatalf("create real parent: %v", err)
	}
	junction := filepath.Join(t.TempDir(), "junction-parent")
	createSandboxDirectoryJunction(t, junction, targetRoot)

	db := filepath.Join(junction, "real-parent", "database.sqlite")
	if err := os.WriteFile(filepath.Join(parent, "database.sqlite"), []byte("database"), 0o600); err != nil {
		t.Fatalf("create database through target parent: %v", err)
	}
	requireApprovedSandboxRecoveryTrigger(t, db)

	adapter := sandboxVFS{VFSFilename: vfs.Find("os").(vfs.VFSFilename)}
	got, err := adapter.FullPathname(db)
	if err != nil {
		t.Fatalf("FullPathname(%q) error = %v, want nil", db, err)
	}
	assertSandboxPathEqual(t, got, filepath.Join(parent, "database.sqlite"))
	if strings.Contains(strings.ToLower(sandboxComparablePath(got)), strings.ToLower(sandboxComparablePath(junction))) {
		t.Fatalf("FullPathname(%q) = %q, still contains junction path %q", db, got, junction)
	}
}

func TestRecoverSandboxPathResolvesExistingDatabase(t *testing.T) {
	dir := sandboxTestDirectory(t)
	db := filepath.Join(dir, "existing.sqlite")
	if err := os.WriteFile(db, []byte("database"), 0o600); err != nil {
		t.Fatalf("create database: %v", err)
	}

	got, err := sandboxVFSFullPathnameWithRecovery(db)
	if err != nil {
		t.Fatalf("recover existing database: %v", err)
	}
	assertSandboxPathEqual(t, got, sandboxExpectedCanonicalPath(t, db))
}

func TestRecoverSandboxPathAllowsNewDatabaseAfterParentResolution(t *testing.T) {
	dir := sandboxTestDirectory(t)
	db := filepath.Join(dir, "new.sqlite")

	got, err := sandboxVFSFullPathnameWithRecovery(db)
	if err != nil {
		t.Fatalf("recover new database: %v", err)
	}
	assertSandboxPathEqual(t, got, sandboxExpectedCanonicalPath(t, db))
	if _, err := os.Stat(db); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovery created database: stat error = %v", err)
	}
}

func TestRecoverSandboxPathRejectsMissingParentInsteadOfReturningAbs(t *testing.T) {
	db := filepath.Join(t.TempDir(), "missing-parent", "database.sqlite")

	got, err := sandboxVFSFullPathnameWithRecovery(db)
	if err == nil {
		t.Fatalf("recover missing parent returned %q, want error", got)
	}
	if got != "" {
		t.Fatalf("recover missing parent returned %q, want empty path", got)
	}
}

func TestSandboxVFSFullPathnamePreservesOriginalPathNotFoundForMissingParent(t *testing.T) {
	db := filepath.Join(t.TempDir(), "missing-parent", "database.sqlite")

	got, err := sandboxVFSFullPathnameWithPathError(db)
	if got != db {
		t.Fatalf("FullPathname(%q) = %q, want original delegate path", db, got)
	}
	if !errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
		t.Fatalf("FullPathname(%q) error = %v, want PATH_NOT_FOUND", db, err)
	}
}

func TestRecoverSandboxPathRejectsInaccessibleParent(t *testing.T) {
	blocked := filepath.Join(t.TempDir(), "blocked-parent")
	if err := os.MkdirAll(blocked, 0o755); err != nil {
		t.Fatalf("create blocked parent: %v", err)
	}
	currentUser, err := user.Current()
	if err != nil {
		t.Skipf("current Windows user is unavailable: %v", err)
	}
	if err := denySandboxDirectoryAccess(blocked, currentUser.Username); err != nil {
		t.Skipf("denying parent access is unavailable: %v", err)
	}
	t.Cleanup(func() {
		if err := restoreSandboxDirectoryAccess(blocked, currentUser.Username); err != nil {
			t.Logf("restore parent access: %v", err)
		}
	})

	db := filepath.Join(blocked, "database.sqlite")
	got, err := sandboxVFSFullPathnameWithRecovery(db)
	if err == nil {
		t.Fatalf("recover inaccessible parent returned %q, want error", got)
	}
	if got != "" {
		t.Fatalf("recover inaccessible parent returned %q, want empty path", got)
	}
	if !errors.Is(err, windows.ERROR_ACCESS_DENIED) && !errors.Is(err, os.ErrPermission) {
		t.Fatalf("recover inaccessible parent error = %v, want permission denial", err)
	}
}

func TestSandboxVFSFullPathnamePreservesOriginalPathNotFoundForUnsupportedNamespace(t *testing.T) {
	db := `\\server\share\database.sqlite`

	got, err := sandboxVFSFullPathnameWithPathError(db)
	if got != db {
		t.Fatalf("FullPathname(%q) = %q, want original delegate path", db, got)
	}
	if !errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
		t.Fatalf("FullPathname(%q) error = %v, want PATH_NOT_FOUND", db, err)
	}
}

func TestRecoverSandboxPathAllowsDirectoryJunctionAndCanonicalizesParent(t *testing.T) {
	target := filepath.Join(t.TempDir(), "junction-target")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("create junction target: %v", err)
	}
	junction := filepath.Join(t.TempDir(), "junction-parent")
	createSandboxDirectoryJunction(t, junction, target)
	db := filepath.Join(junction, "database.sqlite")
	if err := os.WriteFile(db, []byte("database"), 0o600); err != nil {
		t.Fatalf("create database through junction: %v", err)
	}

	got, err := sandboxVFSFullPathnameWithRecovery(db)
	if err != nil {
		t.Fatalf("recover junction database: %v", err)
	}
	assertSandboxPathEqual(t, got, sandboxExpectedCanonicalPath(t, db))
	if strings.Contains(strings.ToLower(sandboxComparablePath(got)), strings.ToLower(sandboxComparablePath(junction))) {
		t.Fatalf("recovered path %q still contains junction path %q", got, junction)
	}
}

func TestSandboxVFSFullPathnamePreservesOriginalPathNotFoundForFinalDatabaseReparsePoint(t *testing.T) {
	dir := sandboxTestDirectory(t)
	target := filepath.Join(dir, "target.sqlite")
	link := filepath.Join(dir, "database.sqlite")
	if err := os.WriteFile(target, []byte("database"), 0o600); err != nil {
		t.Fatalf("create reparse target: %v", err)
	}
	if err := os.Symlink(target, link); err != nil {
		targetDirectory := filepath.Join(t.TempDir(), "reparse-target")
		if mkdirErr := os.MkdirAll(targetDirectory, 0o755); mkdirErr != nil {
			t.Fatalf("create directory reparse target: %v", mkdirErr)
		}
		link = filepath.Join(t.TempDir(), "database.sqlite")
		createSandboxDirectoryJunction(t, link, targetDirectory)
	}

	got, err := sandboxVFSFullPathnameWithPathError(link)
	if got != link {
		t.Fatalf("FullPathname(%q) = %q, want original delegate path", link, got)
	}
	if !errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
		t.Fatalf("FullPathname(%q) error = %v, want PATH_NOT_FOUND", link, err)
	}
}

func TestRecoverSandboxPathRejectsFinalDatabaseReparsePoint(t *testing.T) {
	dir := sandboxTestDirectory(t)
	target := filepath.Join(dir, "target.sqlite")
	link := filepath.Join(dir, "database.sqlite")
	if err := os.WriteFile(target, []byte("database"), 0o600); err != nil {
		t.Fatalf("create reparse target: %v", err)
	}
	if err := os.Symlink(target, link); err != nil {
		targetDirectory := filepath.Join(t.TempDir(), "reparse-target")
		if mkdirErr := os.MkdirAll(targetDirectory, 0o755); mkdirErr != nil {
			t.Fatalf("create directory reparse target: %v", mkdirErr)
		}
		link = filepath.Join(t.TempDir(), "database.sqlite")
		createSandboxDirectoryJunction(t, link, targetDirectory)
	}

	got, err := sandboxVFSFullPathnameWithRecovery(link)
	if err == nil {
		t.Fatalf("recover final reparse point returned %q, want error", got)
	}
	if got != "" {
		t.Fatalf("recover final reparse point returned %q, want empty path", got)
	}
}

func TestSandboxVFSFullPathnameRejectsFinalDatabaseReparsePointOnDelegateSuccess(t *testing.T) {
	dir := sandboxTestDirectory(t)
	target := filepath.Join(dir, "target.sqlite")
	link := filepath.Join(dir, "database.sqlite")
	if err := os.WriteFile(target, []byte("database"), 0o600); err != nil {
		t.Fatalf("create reparse target: %v", err)
	}
	if err := os.Symlink(target, link); err != nil {
		targetDirectory := filepath.Join(t.TempDir(), "reparse-target")
		if mkdirErr := os.MkdirAll(targetDirectory, 0o755); mkdirErr != nil {
			t.Fatalf("create directory reparse target: %v", mkdirErr)
		}
		link = filepath.Join(t.TempDir(), "database.sqlite")
		createSandboxDirectoryJunction(t, link, targetDirectory)
	}

	adapter := sandboxVFS{VFSFilename: vfs.Find("os").(vfs.VFSFilename)}
	got, err := adapter.FullPathname(link)
	if err == nil {
		t.Fatalf("FullPathname(%q) = %q, want error", link, got)
	}
	if got != "" {
		t.Fatalf("FullPathname(%q) = %q, want empty path on reparse-point rejection", link, got)
	}
}

func TestRecoverSandboxPathPreservesSpacesAndNonASCII(t *testing.T) {
	dir := filepath.Join(sandboxTestDirectory(t), "space folder 日本語")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create spaces/non-ASCII directory: %v", err)
	}
	db := filepath.Join(dir, "database 日本語.sqlite")
	if err := os.WriteFile(db, []byte("database"), 0o600); err != nil {
		t.Fatalf("create spaces/non-ASCII database: %v", err)
	}

	got, err := sandboxVFSFullPathnameWithRecovery(db)
	if err != nil {
		t.Fatalf("recover spaces/non-ASCII database: %v", err)
	}
	assertSandboxPathEqual(t, got, sandboxExpectedCanonicalPath(t, db))
}

func TestRecoverSandboxPathAcceptsExtendedLengthLocalPath(t *testing.T) {
	dir := sandboxTestDirectory(t)
	db := filepath.Join(dir, "extended.sqlite")
	if err := os.WriteFile(db, []byte("database"), 0o600); err != nil {
		t.Fatalf("create extended-length database: %v", err)
	}
	abs, err := filepath.Abs(db)
	if err != nil {
		t.Fatalf("absolute database path: %v", err)
	}
	if len(filepath.VolumeName(abs)) != 2 {
		t.Skipf("test path is not on a local drive: %q", abs)
	}
	extended := `\\?\` + abs

	got, err := sandboxVFSFullPathnameWithRecovery(extended)
	if err != nil {
		t.Fatalf("recover extended-length database: %v", err)
	}
	if !strings.HasPrefix(strings.ToLower(got), `\\?\`) {
		t.Fatalf("recovered path %q lost its extended-length prefix", got)
	}
	assertSandboxPathEqual(t, got, sandboxExpectedCanonicalPath(t, db))
}

func TestRecoverSandboxPathRejectsUnsupportedNamespaces(t *testing.T) {
	paths := []string{
		`\\server\share\database.sqlite`,
		`\\?\UNC\server\share\database.sqlite`,
		`\\.\PIPE\database.sqlite`,
		`\??\C:\database.sqlite`,
		`\??\UNC\server\share\database.sqlite`,
		`\Device\HarddiskVolume1\database.sqlite`,
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			got, err := recoverSandboxPath(path)
			if err == nil {
				t.Fatalf("recover unsupported namespace returned %q, want error", got)
			}
			if got != "" {
				t.Fatalf("recover unsupported namespace returned %q, want empty path", got)
			}
			if !strings.Contains(strings.ToLower(err.Error()), "unsupported") {
				t.Fatalf("recover unsupported namespace error = %v, want an unsupported-namespace error", err)
			}
		})
	}
}

func sandboxVFSFullPathnameWithRecovery(name string) (string, error) {
	base := &sandboxVFSDelegateStub{fullPathErr: windows.ERROR_ACCESS_DENIED}
	adapter := sandboxVFS{
		VFSFilename: base,
		recoverPath: recoverSandboxPath,
	}
	return adapter.FullPathname(name)
}

func sandboxVFSFullPathnameWithPathError(name string) (string, error) {
	base := &sandboxVFSDelegateStub{
		fullPath:    name,
		fullPathErr: windows.ERROR_PATH_NOT_FOUND,
	}
	adapter := sandboxVFS{
		VFSFilename: base,
		recoverPath: recoverSandboxPath,
	}
	return adapter.FullPathname(name)
}

func requireApprovedSandboxRecoveryTrigger(t *testing.T, path string) {
	t.Helper()
	rawPath, rawErr := vfs.Find("os").(vfs.VFSFilename).FullPathname(path)
	if isWindowsPermissionDenied(rawErr) || hasPinnedSQLiteOKSymlinkCode(rawErr) || errors.Is(rawErr, windows.ERROR_PATH_NOT_FOUND) {
		return
	}
	if rawErr == nil {
		t.Skipf("os VFS FullPathname(%q) = (%q, nil); ordinary success is not an approved sandbox recovery trigger", path, rawPath)
	}
	t.Skipf("os VFS FullPathname(%q) = (%q, %v); approved recovery only applies to permission denial, pinned code %d, or exact PATH_NOT_FOUND", path, rawPath, rawErr, sandboxPinnedSQLiteOKSymlink)
}

func sandboxTestDirectory(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "sandbox path")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create test directory: %v", err)
	}
	return dir
}

func createSandboxDirectoryJunction(t *testing.T, junction, target string) {
	t.Helper()
	if output, err := exec.Command("cmd.exe", "/d", "/c", "mklink", "/J", junction, target).CombinedOutput(); err != nil {
		t.Skipf("creating a directory junction is unavailable: %v (%s)", err, strings.TrimSpace(string(output)))
	}
	t.Cleanup(func() { _ = os.Remove(junction) })
}

func denySandboxDirectoryAccess(path, account string) error {
	argument := account + ":(OI)(CI)F"
	output, err := exec.Command("icacls.exe", path, "/deny", argument).CombinedOutput()
	if err != nil {
		return fmt.Errorf("icacls deny: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func restoreSandboxDirectoryAccess(path, account string) error {
	output, err := exec.Command("icacls.exe", path, "/remove:d", account).CombinedOutput()
	if err != nil {
		return fmt.Errorf("icacls restore: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func sandboxExpectedCanonicalPath(t *testing.T, path string) string {
	t.Helper()
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("absolute path: %v", err)
	}
	parentPointer, err := windows.UTF16PtrFromString(filepath.Dir(abs))
	if err != nil {
		t.Fatalf("encode expected parent path: %v", err)
	}
	parentHandle, err := windows.CreateFile(
		parentPointer,
		windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		t.Fatalf("open expected parent handle: %v", err)
	}
	defer windows.CloseHandle(parentHandle)

	buffer := make([]uint16, 32768)
	length, err := windows.GetFinalPathNameByHandle(parentHandle, &buffer[0], uint32(len(buffer)), 0)
	if err != nil {
		t.Fatalf("resolve expected parent handle: %v", err)
	}
	if length == 0 || length >= uint32(len(buffer)) {
		t.Fatalf("unexpected expected parent path length: %d", length)
	}
	return filepath.Join(windows.UTF16ToString(buffer[:length]), filepath.Base(abs))
}

func sandboxComparablePath(path string) string {
	path = strings.ReplaceAll(path, "/", `\`)
	if strings.HasPrefix(path, `\\?\`) {
		path = path[4:]
	}
	return strings.ToLower(filepath.Clean(path))
}

func assertSandboxPathEqual(t *testing.T, got, want string) {
	t.Helper()
	if sandboxComparablePath(got) != sandboxComparablePath(want) {
		t.Fatalf("recovered path = %q, want canonical path %q", got, want)
	}
}

func assertObservedCanonicalMainDBPath(t *testing.T, events []sandboxVFSOpenEvent, wantDBPath string) {
	t.Helper()
	wantComparable := sandboxComparablePath(wantDBPath)
	for _, event := range events {
		if event.flags&vfs.OPEN_MAIN_DB == 0 {
			continue
		}
		if sandboxComparablePath(event.path) == wantComparable {
			return
		}
	}
	t.Fatalf("observer open events = %+v, want an OPEN_MAIN_DB path equal to %q", events, wantDBPath)
}

func assertObservedCanonicalSidecarPaths(t *testing.T, events []sandboxVFSOpenEvent, wantDBPath string) {
	t.Helper()
	wantComparable := sandboxComparablePath(wantDBPath)
	for _, event := range events {
		if event.flags&vfs.OPEN_MAIN_JOURNAL == 0 && event.flags&vfs.OPEN_WAL == 0 {
			continue
		}
		if !strings.HasPrefix(sandboxComparablePath(event.path), wantComparable) {
			t.Fatalf("observer sidecar event = %+v, want canonical sidecar path rooted at %q", event, wantDBPath)
		}
	}
}

type sandboxVFSDelegateStub struct {
	fullPath               string
	fullPathErr            error
	fullPathName           string
	openFile               vfs.File
	openFlags              vfs.OpenFlag
	openErr                error
	openName               string
	openInputFlags         vfs.OpenFlag
	openFilenameFile       vfs.File
	openFilenameFlags      vfs.OpenFlag
	openFilenameErr        error
	openFilenameName       *vfs.Filename
	openFilenameInputFlags vfs.OpenFlag
	accessOK               bool
	accessErr              error
	accessName             string
	accessFlags            vfs.AccessFlag
	deleteErr              error
	deleteName             string
	deleteSyncDir          bool
}

type observingSandboxVFS struct {
	vfs.VFSFilename
	mu                sync.Mutex
	fullPathnameCalls int
	openFilenameCalls int
	openFilenamePaths []sandboxVFSOpenEvent
}

type sandboxVFSOpenEvent struct {
	flags vfs.OpenFlag
	path  string
}

func (v *observingSandboxVFS) FullPathname(name string) (string, error) {
	v.mu.Lock()
	v.fullPathnameCalls++
	v.mu.Unlock()
	return v.VFSFilename.FullPathname(name)
}

func (v *observingSandboxVFS) OpenFilename(name *vfs.Filename, flags vfs.OpenFlag) (vfs.File, vfs.OpenFlag, error) {
	v.mu.Lock()
	v.openFilenameCalls++
	if name != nil {
		v.openFilenamePaths = append(v.openFilenamePaths, sandboxVFSOpenEvent{
			flags: flags,
			path:  name.String(),
		})
	}
	v.mu.Unlock()
	return v.VFSFilename.OpenFilename(name, flags)
}

func (v *observingSandboxVFS) counts() (fullPathnameCalls, openFilenameCalls int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.fullPathnameCalls, v.openFilenameCalls
}

func (v *observingSandboxVFS) openEvents() []sandboxVFSOpenEvent {
	v.mu.Lock()
	defer v.mu.Unlock()
	events := make([]sandboxVFSOpenEvent, len(v.openFilenamePaths))
	copy(events, v.openFilenamePaths)
	return events
}

func (s *sandboxVFSDelegateStub) FullPathname(name string) (string, error) {
	s.fullPathName = name
	return s.fullPath, s.fullPathErr
}

func (s *sandboxVFSDelegateStub) Open(name string, flags vfs.OpenFlag) (vfs.File, vfs.OpenFlag, error) {
	s.openName = name
	s.openInputFlags = flags
	return s.openFile, s.openFlags, s.openErr
}

func (s *sandboxVFSDelegateStub) OpenFilename(name *vfs.Filename, flags vfs.OpenFlag) (vfs.File, vfs.OpenFlag, error) {
	s.openFilenameName = name
	s.openFilenameInputFlags = flags
	return s.openFilenameFile, s.openFilenameFlags, s.openFilenameErr
}

func (s *sandboxVFSDelegateStub) Access(name string, flags vfs.AccessFlag) (bool, error) {
	s.accessName = name
	s.accessFlags = flags
	return s.accessOK, s.accessErr
}

func (s *sandboxVFSDelegateStub) Delete(name string, syncDir bool) error {
	s.deleteName = name
	s.deleteSyncDir = syncDir
	return s.deleteErr
}

type sandboxVFSFileStub struct{}

type sandboxVFSPrivateErrorCode uint32
type sandboxVFSIntErrorCode int
type sandboxVFSUint64ErrorCode uint64
type sandboxVFSPointerErrorCode uint32

func (e sandboxVFSPrivateErrorCode) Error() string {
	return fmt.Sprintf("sqlite error %d", uint32(e))
}

func (e sandboxVFSIntErrorCode) Error() string {
	return fmt.Sprintf("sqlite error %d", int(e))
}

func (e sandboxVFSUint64ErrorCode) Error() string {
	return fmt.Sprintf("sqlite error %d", uint64(e))
}

func (e *sandboxVFSPointerErrorCode) Error() string {
	return fmt.Sprintf("sqlite error %d", uint32(*e))
}

func (sandboxVFSFileStub) Close() error { return nil }

func (sandboxVFSFileStub) ReadAt([]byte, int64) (int, error) { return 0, io.EOF }

func (sandboxVFSFileStub) WriteAt(p []byte, _ int64) (int, error) { return len(p), nil }

func (sandboxVFSFileStub) Truncate(int64) error { return nil }

func (sandboxVFSFileStub) Sync(vfs.SyncFlag) error { return nil }

func (sandboxVFSFileStub) Size() (int64, error) { return 0, nil }

func (sandboxVFSFileStub) Lock(vfs.LockLevel) error { return nil }

func (sandboxVFSFileStub) Unlock(vfs.LockLevel) error { return nil }

func (sandboxVFSFileStub) CheckReservedLock() (bool, error) { return false, nil }

func (sandboxVFSFileStub) SectorSize() int { return 0 }

func (sandboxVFSFileStub) DeviceCharacteristics() vfs.DeviceCharacteristic { return 0 }

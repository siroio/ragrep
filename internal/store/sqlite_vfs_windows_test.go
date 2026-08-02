//go:build windows

package store

import (
	"errors"
	"io"
	"testing"

	"github.com/ncruces/go-sqlite3/vfs"
)

func TestSandboxVFSIsRegisteredByName(t *testing.T) {
	got := vfs.Find(sandboxVFSName)
	if got == nil {
		t.Fatalf("vfs.Find(%q) returned nil", sandboxVFSName)
	}
	if _, ok := got.(vfs.VFSFilename); !ok {
		t.Fatalf("vfs.Find(%q) returned %T, want vfs.VFSFilename", sandboxVFSName, got)
	}
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

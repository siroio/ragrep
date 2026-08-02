//go:build windows

package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"unsafe"

	"github.com/ncruces/go-sqlite3/vfs"
	"golang.org/x/sys/windows"
)

const sandboxVFSName = "ragrep-sandbox"

// ncruces/go-sqlite3 v0.19.0 returns this private _ErrorCode value when the
// OS VFS hits a symlink/junction path it cannot open directly.
const sandboxPinnedSQLiteOKSymlink = 512

type sandboxVFS struct {
	vfs.VFSFilename
	recoverPath func(string) (string, error)
}

var (
	_ vfs.VFS         = (*sandboxVFS)(nil)
	_ vfs.VFSFilename = (*sandboxVFS)(nil)
)

func init() {
	vfs.Register(sandboxVFSName, &sandboxVFS{
		VFSFilename: vfs.Find("os").(vfs.VFSFilename),
	})
}

func (s *sandboxVFS) FullPathname(name string) (string, error) {
	path, err := s.VFSFilename.FullPathname(name)
	if err == nil {
		if err := inspectSandboxDatabasePath(path); err != nil {
			return "", fmt.Errorf("inspect SQLite database path %q: %w", path, err)
		}
		return path, nil
	}

	recoverPath := s.recoverPath
	if recoverPath == nil {
		recoverPath = recoverSandboxPath
	}
	if isWindowsPermissionDenied(err) || hasPinnedSQLiteOKSymlinkCode(err) {
		return recoverPath(name)
	}
	if errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
		recovered, recoveryErr := recoverPath(name)
		if recoveryErr == nil {
			return recovered, nil
		}
		return path, err
	}
	return path, err
}

func isWindowsPermissionDenied(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, os.ErrPermission)
}

func hasPinnedSQLiteOKSymlinkCode(err error) bool {
	return errorHasNumericCode(err, sandboxPinnedSQLiteOKSymlink)
}

func errorHasNumericCode(err error, code uint32) bool {
	value := reflect.ValueOf(err)
	if !value.IsValid() {
		return false
	}
	switch value.Kind() {
	case reflect.Uint8, reflect.Uint16, reflect.Uint32:
		return value.Uint() == uint64(code)
	default:
		return false
	}
}

func recoverSandboxPath(name string) (string, error) {
	if err := rejectUnsupportedWindowsNamespace(name); err != nil {
		return "", err
	}

	absName, err := filepath.Abs(name)
	if err != nil {
		return "", fmt.Errorf("resolve SQLite path %q to an absolute path: %w", name, err)
	}
	if err := rejectUnsupportedWindowsNamespace(absName); err != nil {
		return "", err
	}

	parent := filepath.Dir(absName)
	base := filepath.Base(absName)
	if base == "." || base == string(filepath.Separator) || base == "" {
		return "", fmt.Errorf("resolve SQLite path %q: invalid database filename", name)
	}

	canonicalParent, err := resolveWindowsDirectory(parent)
	if err != nil {
		return "", fmt.Errorf("resolve SQLite parent directory %q: %w", parent, err)
	}

	finalPath := filepath.Join(canonicalParent, base)
	if err := inspectSandboxDatabasePath(finalPath); err != nil {
		return "", fmt.Errorf("inspect SQLite database path %q: %w", finalPath, err)
	}
	return finalPath, nil
}

func inspectSandboxDatabasePath(path string) error {
	if err := inspectWindowsFinalComponent(path); err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return nil
		}
		return err
	}
	return nil
}

func rejectUnsupportedWindowsNamespace(path string) error {
	normalized := strings.ToLower(strings.ReplaceAll(path, "/", `\`))
	switch {
	case strings.HasPrefix(normalized, `\\.\`):
		return fmt.Errorf("resolve SQLite path %q: unsupported device namespace", path)
	case strings.HasPrefix(normalized, `\??\`), strings.HasPrefix(normalized, `\device\`), strings.HasPrefix(normalized, `\global??\`):
		return fmt.Errorf("resolve SQLite path %q: unsupported device namespace", path)
	case strings.HasPrefix(normalized, `\\?\unc\`):
		return fmt.Errorf("resolve SQLite path %q: unsupported UNC namespace", path)
	case strings.HasPrefix(normalized, `\\?\`):
		remainder := normalized[4:]
		if len(remainder) < 3 || remainder[1] != ':' || remainder[2] != '\\' {
			return fmt.Errorf("resolve SQLite path %q: unsupported device namespace", path)
		}
	case strings.HasPrefix(normalized, `\\`):
		return fmt.Errorf("resolve SQLite path %q: unsupported UNC namespace", path)
	}
	return nil
}

func resolveWindowsDirectory(path string) (canonical string, err error) {
	pathPointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", fmt.Errorf("encode directory path: %w", err)
	}
	handle, err := windows.CreateFile(
		pathPointer,
		windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return "", err
	}
	defer func() {
		if closeErr := windows.CloseHandle(handle); closeErr != nil && err == nil {
			err = fmt.Errorf("close directory handle: %w", closeErr)
		}
	}()

	canonical, err = finalWindowsPath(handle)
	if err != nil {
		return "", err
	}
	if err := rejectUnsupportedWindowsNamespace(canonical); err != nil {
		return "", err
	}
	return canonical, nil
}

func finalWindowsPath(handle windows.Handle) (string, error) {
	const initialBufferSize = 256
	const maximumBufferSize = 32768

	for bufferSize := uint32(initialBufferSize); bufferSize <= maximumBufferSize; {
		buffer := make([]uint16, bufferSize)
		length, err := windows.GetFinalPathNameByHandle(handle, &buffer[0], bufferSize, 0)
		if err != nil {
			return "", err
		}
		if length < bufferSize {
			return windows.UTF16ToString(buffer[:length]), nil
		}
		if length >= maximumBufferSize {
			return "", fmt.Errorf("final path exceeds %d UTF-16 code units", maximumBufferSize-1)
		}
		bufferSize = length + 1
	}
	return "", fmt.Errorf("final path buffer size overflow")
}

type sandboxFileAttributeTagInfo struct {
	fileAttributes uint32
	reparseTag     uint32
}

func inspectWindowsFinalComponent(path string) (err error) {
	pathPointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return fmt.Errorf("encode final path: %w", err)
	}
	handle, err := windows.CreateFile(
		pathPointer,
		windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := windows.CloseHandle(handle); closeErr != nil && err == nil {
			err = fmt.Errorf("close final component handle: %w", closeErr)
		}
	}()

	var info sandboxFileAttributeTagInfo
	if err := windows.GetFileInformationByHandleEx(
		handle,
		windows.FileAttributeTagInfo,
		(*byte)(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		return err
	}
	if info.fileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fmt.Errorf("final component is a reparse point (tag 0x%08x)", info.reparseTag)
	}
	return nil
}

//go:build windows

package store

import "github.com/ncruces/go-sqlite3/vfs"

const sandboxVFSName = "ragrep-sandbox"

type sandboxVFS struct {
	vfs.VFSFilename
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

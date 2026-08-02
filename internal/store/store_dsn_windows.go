//go:build windows

package store

import (
	"net/url"
	"path/filepath"
)

func storeDSN(path string) string {
	query := url.Values{}
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("vfs", sandboxVFSName)

	return "file:" + (&url.URL{Path: filepath.ToSlash(path)}).EscapedPath() + "?" + query.Encode()
}

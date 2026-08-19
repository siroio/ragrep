//go:build windows

package store

import (
	"net/url"
	"path/filepath"
	"strings"
)

func storeDSN(path string) string {
	query := url.Values{}
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("vfs", sandboxVFSName)

	return "file:" + (&url.URL{Path: filepath.ToSlash(stripExtendedLengthLocalDrivePrefix(path))}).EscapedPath() + "?" + query.Encode()
}

func stripExtendedLengthLocalDrivePrefix(path string) string {
	const extendedPrefix = `\\?\`
	if !strings.HasPrefix(path, extendedPrefix) {
		return path
	}

	localDrivePath := path[len(extendedPrefix):]
	if len(localDrivePath) < 3 || localDrivePath[1] != ':' || localDrivePath[2] != '\\' {
		return path
	}
	drive := localDrivePath[0]
	if drive >= 'a' && drive <= 'z' || drive >= 'A' && drive <= 'Z' {
		return localDrivePath
	}
	return path
}

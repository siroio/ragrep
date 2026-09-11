//go:build !windows

package store

import "net/url"

func storeDSN(path string) string {
	return "file:" + path + "?_pragma=busy_timeout(5000)"
}

func storeReadOnlyDSN(path string) string {
	return "file:" + (&url.URL{Path: path}).EscapedPath() + "?_pragma=busy_timeout(5000)&mode=ro"
}

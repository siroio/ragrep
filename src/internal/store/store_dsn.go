//go:build !windows

package store

func storeDSN(path string) string {
	return "file:" + path + "?_pragma=busy_timeout(5000)"
}

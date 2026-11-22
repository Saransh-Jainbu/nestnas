//go:build windows

package server

// Disk space checks are not implemented on Windows yet; quotas still apply.
func diskFree(path string) (uint64, bool)  { return 0, false }
func diskTotal(path string) (uint64, bool) { return 0, false }

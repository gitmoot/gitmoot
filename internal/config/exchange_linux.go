//go:build linux

package config

import "golang.org/x/sys/unix"

// exchangeFiles atomically swaps two paths (renameat2 RENAME_EXCHANGE), so a
// conditional writer can install its file and still inspect exactly what it
// replaced.
func exchangeFiles(a, b string) error {
	return unix.Renameat2(unix.AT_FDCWD, a, unix.AT_FDCWD, b, unix.RENAME_EXCHANGE)
}

const canExchangeFiles = true

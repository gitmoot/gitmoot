//go:build !unix

package agyaddon

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

func openRegularNoFollow(path string) (*os.File, fs.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return nil, nil, errors.New("is a symlink")
	}
	if !info.Mode().IsRegular() {
		return nil, nil, errors.New("not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	return file, info, nil
}

// CheckPrivateEntry refuses symlinks and wrong file types on non-Unix builds.
func CheckPrivateEntry(info fs.FileInfo, want fs.FileMode) error {
	if info.Mode()&fs.ModeSymlink != 0 {
		return errors.New("is a symlink")
	}
	if info.Mode().Type() != want {
		return fmt.Errorf("has file type %v", info.Mode().Type())
	}
	return nil
}

func withLockFile(_ string, fn func() error) error {
	return fn()
}

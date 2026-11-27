//go:build unix

package agyaddon

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

func openRegularNoFollow(path string) (*os.File, fs.FileInfo, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, nil, errors.New("not a regular file")
	}
	return file, info, nil
}

// CheckPrivateEntry refuses symlinks, wrong file types, foreign owners, or any
// group/other permission bits.
func CheckPrivateEntry(info fs.FileInfo, want fs.FileMode) error {
	if info.Mode()&fs.ModeSymlink != 0 {
		return errors.New("is a symlink")
	}
	if info.Mode().Type() != want {
		return fmt.Errorf("has file type %v", info.Mode().Type())
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("owner is unavailable")
	}
	if uid := uint32(os.Geteuid()); stat.Uid != uid {
		return fmt.Errorf("is owned by uid %d, not %d", stat.Uid, uid)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("has mode %04o; group and other access must be off", perm)
	}
	return nil
}

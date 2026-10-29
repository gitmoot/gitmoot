//go:build !linux

package workflow

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/gitmoot/gitmoot/internal/config"
)

// Mount identity is only provable through Linux mountinfo and statx; elsewhere a configured mount
// always refuses, so a missing mount can never be mistaken for a usable one.
func verifyWorktreeMount(cfg config.WorktreeRoot) (uint64, error) {
	return 0, errors.New("[worktrees] mount checks require Linux")
}

// WorktreeStorageDevice cannot prove a network mount off Linux; retain coverage.
func WorktreeStorageDevice(path string, _ config.WorktreeRoot) (uint64, bool, error) {
	for p := filepath.Clean(path); ; p = filepath.Dir(p) {
		info, err := os.Stat(p)
		if err == nil {
			st, ok := info.Sys().(*syscall.Stat_t)
			if !ok {
				return 0, false, fmt.Errorf("no device number for %s", p)
			}
			return uint64(st.Dev), false, nil
		}
		if !os.IsNotExist(err) || p == filepath.Dir(p) {
			return 0, false, fmt.Errorf("resolve device for %s: %w", path, err)
		}
	}
}

func mkdirBelowWorktreeRoot(cfg config.WorktreeRoot, rel string) error {
	if cfg.HasMount() {
		_, err := verifyWorktreeMount(cfg)
		return err
	}
	info, err := os.Stat(cfg.Root)
	if err != nil {
		return fmt.Errorf("root %s: %w", cfg.Root, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("root %s is not a directory", cfg.Root)
	}
	return os.MkdirAll(filepath.Join(cfg.Root, rel), 0o755)
}

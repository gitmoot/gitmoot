package config

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// lockConfigEdits serialises gitmoot's own read-modify-write edits of the
// config file (#2502): the daemon now edits it on its own (following a renamed
// seat agent), so a concurrent `org seat bind/remove` or `config set`
// edit must not interleave with it and lose one of the two writes.
// The lock lives beside the config, is advisory, and does not stop a hand edit
// in an editor. Writers that delegate to another writer must not take it too:
// flock is per open file description, so nesting would deadlock.
func lockConfigEdits(paths Paths) (func(), error) {
	lockPath := filepath.Join(filepath.Dir(paths.ConfigFile), ".config.lock")
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open config edit lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("lock config edits: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}

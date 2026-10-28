package workflow

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gitmoot/gitmoot/internal/config"
)

// ErrWorktreeRootUnavailable means the configured [worktrees] root (#2375) cannot be proven to be
// where the operator put it: config unreadable, root missing, or not on the expected mount. Work
// that would create, reopen or clean a worktree there is refused; it never falls back to the home.
var ErrWorktreeRootUnavailable = errors.New("worktree root unavailable")

// WorktreePlacement resolves where gitmoot-created worktrees live for one resolved home.
// Legacy (<home>/worktrees) is always recognised so existing worktrees keep their cleanup
// identity; Root is where NEW worktrees are allocated (Legacy unless [worktrees] is configured).
type WorktreePlacement struct {
	Home   string
	Legacy string
	Root   string
	config config.WorktreeRoot
}

// ResolveWorktreePlacement reads [worktrees] from <home>/config.toml. home is the RESOLVED
// gitmoot home (the directory holding config.toml), never a raw --home flag.
func ResolveWorktreePlacement(home string) (WorktreePlacement, error) {
	home = filepath.Clean(strings.TrimSpace(home))
	if home == "." || home == "" {
		return WorktreePlacement{}, errors.New("worktree home is required")
	}
	cfg, err := config.LoadWorktreeRoot(config.Paths{Home: home, ConfigFile: filepath.Join(home, config.ConfigName)})
	if err != nil {
		return WorktreePlacement{}, fmt.Errorf("%w: %v", ErrWorktreeRootUnavailable, err)
	}
	legacy := filepath.Join(home, "worktrees")
	p := WorktreePlacement{Home: home, Legacy: legacy, Root: legacy, config: cfg}
	if cfg.Configured() {
		p.Root = cfg.Root
	}
	return p, nil
}

// External reports whether new worktrees go to a configured root instead of <home>/worktrees.
func (p WorktreePlacement) External() bool { return p.config.Configured() }

// DelegationPath is DelegationWorktreePath under the allocation root.
func (p WorktreePlacement) DelegationPath(repo, parentJobID, delegationID string, retryAttempt int) (string, error) {
	return delegationWorktreePathUnder(p.Root, repo, parentJobID, delegationID, retryAttempt)
}

// UnderExternalRoot reports whether path is lexically inside the configured root.
func (p WorktreePlacement) UnderExternalRoot(path string) bool {
	return p.External() && pathWithin(p.config.Root, filepath.Clean(strings.TrimSpace(path)))
}

// Preflight proves the configured root is usable: it exists as a directory (gitmoot never creates
// it) and, with mount keys, sits on exactly the configured mount. The default layout always passes.
func (p WorktreePlacement) Preflight() error {
	if !p.External() {
		return nil
	}
	info, err := os.Stat(p.config.Root)
	if err != nil {
		return fmt.Errorf("%w: root %s: %v", ErrWorktreeRootUnavailable, p.config.Root, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: root %s is not a directory", ErrWorktreeRootUnavailable, p.config.Root)
	}
	if p.config.HasMount() {
		if _, err := verifyWorktreeMount(p.config); err != nil {
			return fmt.Errorf("%w: %v", ErrWorktreeRootUnavailable, err)
		}
	}
	return nil
}

// PrepareParent creates the parent directories of a new worktree path. In the default layout it is
// os.MkdirAll, as before. Under a configured root it creates each missing component BELOW the
// root through a directory handle proven to be on the configured mount, so a mount that vanishes
// mid-way can never leave new directories on the local disk.
func (p WorktreePlacement) PrepareParent(path string) error {
	parent := filepath.Dir(filepath.Clean(path))
	if !p.External() {
		return os.MkdirAll(parent, 0o755)
	}
	rel, err := filepath.Rel(p.config.Root, parent)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("worktree parent %s is outside the configured root %s", parent, p.config.Root)
	}
	if err := mkdirBelowWorktreeRoot(p.config, rel); err != nil {
		return fmt.Errorf("%w: %v", ErrWorktreeRootUnavailable, err)
	}
	return nil
}

// PreflightWorktreePath refuses to reopen a recorded worktree under the configured root while that
// root is unavailable. Paths outside it (the legacy layout, a task worktree) are not affected.
func PreflightWorktreePath(home, path string) error {
	placement, err := ResolveWorktreePlacement(home)
	if err != nil {
		return err
	}
	if !placement.UnderExternalRoot(path) {
		return nil
	}
	return placement.Preflight()
}

func pathWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

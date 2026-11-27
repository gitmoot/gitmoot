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

// CleanupRefusalWorktreeRootUnavailable is the cleanup refusal code for a resource under a
// configured [worktrees] root whose mount preflight failed. It is retryable once the mount is back.
const CleanupRefusalWorktreeRootUnavailable = "worktree_root_unavailable"

// WorktreePlacement resolves where gitmoot-created worktrees live for one resolved home.
// Legacy (<home>/worktrees) is always recognised so existing worktrees keep their cleanup
// identity; Root is where NEW worktrees are allocated (Legacy unless [worktrees] is configured).
// [[worktrees.retired]] roots (#2763) are recognised for existing worktrees only, never allocated.
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

// Retired lists the [[worktrees.retired]] roots. They identify EXISTING worktrees only (reopen,
// cleanup, doctor accounting), each after PreflightWorktreeRoot; nothing is ever allocated there.
func (p WorktreePlacement) Retired() []config.WorktreeRoot { return p.config.Retired }

// existingRoots lists the non-legacy roots an existing worktree may sit under: the configured
// root first, then every retired root. Each needs PreflightWorktreeRoot before a path under it
// is trusted.
func (p WorktreePlacement) existingRoots() []config.WorktreeRoot {
	var roots []config.WorktreeRoot
	if p.External() {
		current := p.config
		current.Retired = nil
		roots = append(roots, current)
	}
	return append(roots, p.config.Retired...)
}

// Preflight proves the configured root is usable: it exists as a directory (gitmoot never creates
// it) and, with mount keys, sits on exactly the configured mount. The default layout always passes.
func (p WorktreePlacement) Preflight() error {
	if !p.External() {
		return nil
	}
	return PreflightWorktreeRoot(p.config)
}

// PreflightWorktreeRoot proves one configured or retired root is usable: it exists as a directory
// and, with mount keys, sits on exactly that mount.
func PreflightWorktreeRoot(root config.WorktreeRoot) error {
	info, err := os.Stat(root.Root)
	if err != nil {
		return fmt.Errorf("%w: root %s: %v", ErrWorktreeRootUnavailable, root.Root, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: root %s is not a directory", ErrWorktreeRootUnavailable, root.Root)
	}
	if root.HasMount() {
		if _, err := verifyWorktreeMount(root); err != nil {
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

// PreflightWorktreePath refuses to reopen a recorded worktree under the configured root, or under
// a retired root, while that root is unavailable. Paths outside them (the legacy layout, a task
// worktree) are not affected.
func PreflightWorktreePath(home, path string) error {
	placement, err := ResolveWorktreePlacement(home)
	if err != nil {
		return err
	}
	path = filepath.Clean(strings.TrimSpace(path))
	for _, root := range placement.existingRoots() {
		if !pathWithin(root.Root, path) {
			continue
		}
		if err := PreflightWorktreeRoot(root); err != nil {
			return err
		}
	}
	return nil
}

func pathWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

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
	Home    string
	Legacy  string
	Root    string
	active  config.WorktreeRoot
	retired []config.WorktreeRoot
}

// ResolveWorktreePlacement reads [worktrees] from <home>/config.toml. home is the RESOLVED
// gitmoot home (the directory holding config.toml), never a raw --home flag.
func ResolveWorktreePlacement(home string) (WorktreePlacement, error) {
	home = filepath.Clean(strings.TrimSpace(home))
	if home == "." || home == "" {
		return WorktreePlacement{}, errors.New("worktree home is required")
	}
	roots, err := config.LoadWorktreeRoots(config.Paths{Home: home, ConfigFile: filepath.Join(home, config.ConfigName)})
	if err != nil {
		return WorktreePlacement{}, fmt.Errorf("%w: %v", ErrWorktreeRootUnavailable, err)
	}
	legacy := filepath.Join(home, "worktrees")
	p := WorktreePlacement{Home: home, Legacy: legacy, Root: legacy, active: roots.Active, retired: roots.Retired}
	if roots.Active.Configured() {
		p.Root = roots.Active.Root
	}
	return p, nil
}

// External reports whether new worktrees go to a configured root instead of <home>/worktrees.
func (p WorktreePlacement) External() bool { return p.active.Configured() }

// DelegationPath is DelegationWorktreePath under the allocation root.
func (p WorktreePlacement) DelegationPath(repo, parentJobID, delegationID string, retryAttempt int) (string, error) {
	return delegationWorktreePathUnder(p.Root, repo, parentJobID, delegationID, retryAttempt)
}

// Retired lists the [[worktrees.retired]] roots. They identify EXISTING worktrees only (reopen,
// cleanup, doctor accounting), each after PreflightWorktreeRoot; nothing is ever allocated there.
func (p WorktreePlacement) Retired() []config.WorktreeRoot { return p.retired }

// worktreeRootKind is why a root is recognised for an existing worktree.
type worktreeRootKind int

const (
	// legacyWorktreeRoot is <home>/worktrees: inside the home, so there is no mount to prove.
	legacyWorktreeRoot worktreeRootKind = iota
	// activeWorktreeRoot is the configured [worktrees] root.
	activeWorktreeRoot
	// retiredWorktreeRoot is one [[worktrees.retired]] root.
	retiredWorktreeRoot
)

// existingWorktreeRoot is one root an existing worktree may sit under. The config forbids
// overlapping roots, so a path has at most one.
type existingWorktreeRoot struct {
	config.WorktreeRoot
	kind worktreeRootKind
}

// preflight proves the root usable before a path under it is trusted and returns its mount ID
// (0 without mount keys). Only the legacy root, which lives in the home, is exempt.
func (r existingWorktreeRoot) preflight() (uint64, error) {
	if r.kind == legacyWorktreeRoot {
		return 0, nil
	}
	return worktreeRootPreflight(r.WorktreeRoot)
}

// worktreeRootPreflight is preflightWorktreeRoot behind a variable so tests can make a root lose
// its mount between two proofs without real mounts.
var worktreeRootPreflight = preflightWorktreeRoot

// existingRoots lists every root an existing worktree may sit under: the legacy root, the
// configured root when there is one, then every retired root.
func (p WorktreePlacement) existingRoots() []existingWorktreeRoot {
	roots := []existingWorktreeRoot{{WorktreeRoot: config.WorktreeRoot{Root: p.Legacy}, kind: legacyWorktreeRoot}}
	if p.External() {
		roots = append(roots, existingWorktreeRoot{WorktreeRoot: p.active, kind: activeWorktreeRoot})
	}
	for _, retired := range p.retired {
		roots = append(roots, existingWorktreeRoot{WorktreeRoot: retired, kind: retiredWorktreeRoot})
	}
	return roots
}

// owningRoot is the existing root strictly containing path, if any.
func (p WorktreePlacement) owningRoot(path string) (existingWorktreeRoot, bool) {
	for _, root := range p.existingRoots() {
		if pathWithin(root.Root, path) {
			return root, true
		}
	}
	return existingWorktreeRoot{}, false
}

// Preflight proves the configured root is usable: it exists as a directory (gitmoot never creates
// it) and, with mount keys, sits on exactly the configured mount. The default layout always passes.
func (p WorktreePlacement) Preflight() error {
	if !p.External() {
		return nil
	}
	return PreflightWorktreeRoot(p.active)
}

// PreflightWorktreeRoot proves one configured or retired root is usable: it exists as a directory
// and, with mount keys, sits on exactly that mount.
func PreflightWorktreeRoot(root config.WorktreeRoot) error {
	_, err := preflightWorktreeRoot(root)
	return err
}

// preflightWorktreeRoot is PreflightWorktreeRoot returning the proven mount ID (0 without mount
// keys).
func preflightWorktreeRoot(root config.WorktreeRoot) (uint64, error) {
	info, err := os.Stat(root.Root)
	if err != nil {
		return 0, fmt.Errorf("%w: root %s: %v", ErrWorktreeRootUnavailable, root.Root, err)
	}
	if !info.IsDir() {
		return 0, fmt.Errorf("%w: root %s is not a directory", ErrWorktreeRootUnavailable, root.Root)
	}
	if !root.HasMount() {
		return 0, nil
	}
	id, err := verifyWorktreeMount(root)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrWorktreeRootUnavailable, err)
	}
	return id, nil
}

// proveWorktreeAbsent is how cleanup takes a missing worktree path for a removed one (#2763
// review): it returns nil only when path is absent while its owning root is proven. A missing path
// under a configured or retired root proves nothing by itself, because an unmounted root shows an
// empty directory or none, and the worktree is still on the backing mount. So the owning root is
// preflighted immediately before AND after the path is seen absent, and both proofs must name the
// same mount; otherwise the root is unavailable (ErrWorktreeRootUnavailable, retryable) and the
// caller must not record a removal. A path that exists is an error too.
func proveWorktreeAbsent(home, path string) error {
	placement, err := ResolveWorktreePlacement(home)
	if err != nil {
		return err
	}
	path = filepath.Clean(strings.TrimSpace(path))
	root, owned := placement.owningRoot(path)
	var before uint64
	if owned {
		if before, err = root.preflight(); err != nil {
			return err
		}
	}
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("cleanup path %s exists", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if !owned {
		return nil
	}
	after, err := root.preflight()
	if err != nil {
		return err
	}
	if after != before {
		return fmt.Errorf("%w: root %s moved from mount %d to mount %d while %s was checked", ErrWorktreeRootUnavailable, root.Root, before, after, path)
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
	rel, err := filepath.Rel(p.active.Root, parent)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("worktree parent %s is outside the configured root %s", parent, p.active.Root)
	}
	if err := mkdirBelowWorktreeRoot(p.active, rel); err != nil {
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
	if root, owned := placement.owningRoot(filepath.Clean(strings.TrimSpace(path))); owned {
		_, err = root.preflight()
	}
	return err
}

func pathWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

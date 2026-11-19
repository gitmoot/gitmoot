package remote

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/execbackend"
	"github.com/gitmoot/gitmoot/internal/execbackend/e2b"
)

// Remote workspaces (#2678) are provider sandboxes that outlive the process
// that created them: `gitmoot workspace open` provisions one, every later
// `workspace exec` is a separate process, and the daemon renews and destroys
// it. So, unlike a job instance, a workspace is never held in Backend's
// in-process map. Every call re-attaches from the sandbox ID recorded in the
// workspaces table, through the control plane's sandbox detail.

// WorkspaceJobIDPrefix marks a workspace sandbox's job_id metadata, so the job
// attempt reconciler can tell a workspace from an unledgered job sandbox.
const WorkspaceJobIDPrefix = execbackend.WorkspaceJobIDPrefix

const metadataWorkspaceID = "workspace_id"

// DefaultWorkspaceGuestHome is the guest user's home in the E2B templates;
// envd runs every process as that user.
const DefaultWorkspaceGuestHome = "/home/user"

// WorkspaceRoot is the work tree inside a guest home.
func WorkspaceRoot(guestHome string) string { return path.Join(guestHome, "work") }

// WorkspaceLogDir is where the exec supervisor keeps each command's output
// inside the guest; `workspace close` copies it out before teardown.
func WorkspaceLogDir(guestHome string) string {
	return path.Join(guestHome, ".gitmoot", "workspace", "logs")
}

// syncRemoteWorkspaceScript runs in the guest home. $1 is the work tree, $2
// the commit to check out, $3 the commit gitmoot-base names, $4 the branch
// and $5 the workspace PATH. Like the review sync, the archive carries only
// an object store and its shallow boundary, never a host .git, so no remote,
// config or credential helper reaches the guest. The branch tracks
// gitmoot-base, so plain `git log @{upstream}..` shows the workspace's own
// commits. Debian's /etc/profile resets PATH for every login shell, which
// `ws sh` (bash -lc) is, so ~/.bash_profile puts the workspace PATH back.
const syncRemoteWorkspaceScript = `set -eu
root=$1
head=$2
base=$3
branch=$4
workspace_path=$5
rm -rf "$root" .gitmoot-ws-sync
mkdir .gitmoot-ws-sync
tar -xzf .gitmoot-ws-sync.tar.gz -C .gitmoot-ws-sync
git init -q "$root"
rm -rf "$root/.git/objects"
mv .gitmoot-ws-sync/objects "$root/.git/objects"
if [ -f .gitmoot-ws-sync/shallow ]; then
  mv .gitmoot-ws-sync/shallow "$root/.git/shallow"
fi
rm -rf .gitmoot-ws-sync .gitmoot-ws-sync.tar.gz
mkdir -p .gitmoot/workspace/logs
{
  echo '# gitmoot workspace: /etc/profile resets PATH; restore the workspace PATH.'
  echo 'if [ -f "$HOME/.profile" ]; then . "$HOME/.profile"; fi'
  printf "PATH='%s'\nexport PATH\n" "$workspace_path"
} > .bash_profile
cd "$root"
git config user.name gitmoot
git config user.email gitmoot@localhost
git branch gitmoot-base "$base"
git checkout -q -b "$branch" "$head"
git branch -q --set-upstream-to=gitmoot-base "$branch"
git rev-parse HEAD`

// WorkspaceSource is what a workspace checks out: Head on Branch, with Base
// (an ancestor of Head) as gitmoot-base. Checkout is the host clone the
// commits are exported from; it must already hold both.
type WorkspaceSource struct {
	Checkout string
	Head     string
	Base     string
	Branch   string
}

// Workspace is one attached workspace sandbox: its ID and an authenticated
// data-plane client for it.
type Workspace struct {
	ID        string
	GuestHome string
	envd      *e2b.Envd
}

// ProvisionWorkspace creates a workspace sandbox that the provider retires
// after ttl unless SetWorkspaceTimeout extends it. No keepalive runs in this
// process: the caller's ledger decides every extension.
func (b *Backend) ProvisionWorkspace(ctx context.Context, workspaceID string, ttl time.Duration) (*Workspace, error) {
	if b == nil {
		return nil, errors.New("remote execution backend is nil")
	}
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" {
		return nil, errors.New("workspace id is required")
	}
	if ttl <= 0 || ttl > ProviderMaxTTL {
		return nil, fmt.Errorf("workspace TTL %s must be positive and at most %s", ttl, ProviderMaxTTL)
	}
	metadata := map[string]string{
		metadataJobID:       WorkspaceJobIDPrefix + workspaceID,
		metadataWorkspaceID: workspaceID,
		metadataBootID:      b.bootID,
	}
	sandbox, credential, err := b.client.Create(ctx, b.templateID, ttl, e2b.CreateOptions{Metadata: metadata})
	if err != nil {
		return nil, fmt.Errorf("provision workspace sandbox: %w", err)
	}
	envd, err := e2b.NewEnvd(sandbox, credential, b.envd)
	if err != nil {
		_, deleteErr := b.client.Delete(context.WithoutCancel(ctx), sandbox.ID)
		return nil, errors.Join(fmt.Errorf("construct workspace data plane: %w", err), deleteErr)
	}
	return &Workspace{ID: sandbox.ID, GuestHome: b.workspaceGuestHome(), envd: envd}, nil
}

// AttachWorkspace re-attaches to a workspace sandbox from its ID. A sandbox
// the provider no longer has reports e2b.Gone with a nil workspace.
func (b *Backend) AttachWorkspace(ctx context.Context, sandboxID string) (*Workspace, e2b.State, error) {
	if b == nil {
		return nil, e2b.Unknown, errors.New("remote execution backend is nil")
	}
	sandbox, credential, state, err := b.client.Connect(ctx, sandboxID)
	if err != nil {
		return nil, state, fmt.Errorf("attach workspace sandbox %q: %w", sandboxID, err)
	}
	envd, err := e2b.NewEnvd(sandbox, credential, b.envd)
	if err != nil {
		return nil, e2b.Unknown, fmt.Errorf("construct workspace data plane: %w", err)
	}
	return &Workspace{ID: sandbox.ID, GuestHome: b.workspaceGuestHome(), envd: envd}, e2b.Present, nil
}

// SetWorkspaceTimeout moves the provider's deadline for a workspace sandbox
// to ttl from now. It needs no data-plane credential.
func (b *Backend) SetWorkspaceTimeout(ctx context.Context, sandboxID string, ttl time.Duration) (e2b.State, error) {
	if b == nil {
		return e2b.Unknown, errors.New("remote execution backend is nil")
	}
	return b.client.SetTimeout(ctx, sandboxID, ttl)
}

// DestroyWorkspace deletes a workspace sandbox. Gone is the only conclusive
// success; a sandbox that was already gone is Gone too.
func (b *Backend) DestroyWorkspace(ctx context.Context, sandboxID string) (e2b.State, error) {
	if b == nil {
		return e2b.Unknown, errors.New("remote execution backend is nil")
	}
	state, err := b.client.Delete(ctx, sandboxID)
	if err != nil && state == e2b.Gone {
		return e2b.Gone, nil
	}
	return state, err
}

func (b *Backend) workspaceGuestHome() string {
	if b.guestHome != "" {
		return b.guestHome
	}
	return DefaultWorkspaceGuestHome
}

// Sync ships source into the workspace and checks out its branch. It returns
// the workspace HEAD, which must be source.Head exactly.
func (w *Workspace) Sync(ctx context.Context, hostTemp string, source WorkspaceSource, workspacePATH string) (string, error) {
	if w == nil || w.envd == nil {
		return "", errors.New("workspace is not attached")
	}
	if source.Head == "" || source.Base == "" || source.Branch == "" || source.Checkout == "" {
		return "", errors.New("workspace source needs a checkout, head, base and branch")
	}
	historyRoot, err := os.MkdirTemp(hostTemp, "sync-")
	if err != nil {
		return "", fmt.Errorf("create workspace sync directory: %w", err)
	}
	defer os.RemoveAll(historyRoot)
	history, err := exportExactHistory(ctx, source.Checkout, historyRoot, source.Head, source.Base)
	if err != nil {
		return "", fmt.Errorf("export workspace history: %w", err)
	}
	if err := uploadWorkspaceArchive(ctx, w.envd, path.Join(w.GuestHome, ".gitmoot-ws-sync.tar.gz"), history, nil); err != nil {
		return "", fmt.Errorf("upload workspace source: %w", err)
	}
	result, err := runEnvd(ctx, w.envd, e2b.StartRequest{
		Name:           "sh",
		Args:           []string{"-c", syncRemoteWorkspaceScript, "gitmoot-ws-sync", WorkspaceRoot(w.GuestHome), source.Head, source.Base, source.Branch, workspacePATH},
		Dir:            w.GuestHome,
		MaxOutputBytes: 64 << 10,
	})
	if err != nil {
		return "", fmt.Errorf("check out workspace branch: %w: %s", err, strings.TrimSpace(result.Stderr))
	}
	head := strings.TrimSpace(result.Stdout)
	if head != source.Head {
		return "", fmt.Errorf("check out workspace branch: HEAD %q is not %s", head, source.Head)
	}
	return head, nil
}

// Start launches one process in the workspace and streams it.
func (w *Workspace) Start(ctx context.Context, request e2b.StartRequest) (*e2b.Stream, error) {
	if w == nil || w.envd == nil {
		return nil, errors.New("workspace is not attached")
	}
	return w.envd.Start(ctx, request)
}

// Run runs one process to completion.
func (w *Workspace) Run(ctx context.Context, request e2b.StartRequest) (execbackend.ExecResult, error) {
	if w == nil || w.envd == nil {
		return execbackend.ExecResult{}, errors.New("workspace is not attached")
	}
	return runEnvd(ctx, w.envd, request)
}

// SendInput writes to the stdin of a process started with Stdin.
func (w *Workspace) SendInput(ctx context.Context, pid int, data []byte) error {
	if w == nil || w.envd == nil {
		return errors.New("workspace is not attached")
	}
	return w.envd.SendInput(ctx, pid, data)
}

// SendSignal signals one workspace process.
func (w *Workspace) SendSignal(ctx context.Context, pid int, signal e2b.Signal) error {
	if w == nil || w.envd == nil {
		return errors.New("workspace is not attached")
	}
	return w.envd.SendSignal(ctx, pid, signal)
}

// Home is the guest home the workspace lives under.
func (w *Workspace) Home() string { return w.GuestHome }

// Root is the workspace's work tree inside the guest.
func (w *Workspace) Root() string { return WorkspaceRoot(w.GuestHome) }

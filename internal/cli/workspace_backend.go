package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/execbackend"
	"github.com/gitmoot/gitmoot/internal/execbackend/e2b"
	remoteexec "github.com/gitmoot/gitmoot/internal/execbackend/remote"
)

// workspaceLocalProvider is `[workspaces] provider = "local"`: workspaces on
// the existing local execution backend, under <home>/execbackends/local. It
// costs nothing and isolates nothing; it exists so development and
// integration tests can drive the real CLI without spending.
const workspaceLocalProvider = config.WorkspacesLocalProvider

// workspaceBackend is one provider's workspace lifecycle. Every call after
// open re-attaches from the instance ID in the workspaces table.
type workspaceBackend interface {
	provision(ctx context.Context, workspaceID string, ttl time.Duration) (string, workspaceSandbox, error)
	attach(ctx context.Context, instanceID string) (workspaceSandbox, e2b.State, error)
	extend(ctx context.Context, instanceID string, ttl time.Duration) (e2b.State, error)
	destroy(ctx context.Context, instanceID string) (e2b.State, error)
	// find looks up the instance tagged with workspaceID in the provider's
	// inventory, for a workspace whose create answer was lost. authoritative
	// reports that an empty result proves nothing was allocated.
	find(ctx context.Context, workspaceID string) (instanceID string, authoritative bool, err error)
	// deadlineGrace is how long past a requested deadline the provider may
	// still be running a sandbox. enforced is false when the provider has no
	// deadline at all, so nothing may be settled by expiry.
	deadlineGrace() (grace time.Duration, enforced bool)
}

// workspaceSandbox is one attached workspace instance.
type workspaceSandbox interface {
	Home() string
	Root() string
	Sync(ctx context.Context, hostTemp string, source remoteexec.WorkspaceSource, workspacePATH string) (string, error)
	Start(ctx context.Context, request e2b.StartRequest) (workspaceStream, error)
	Run(ctx context.Context, request e2b.StartRequest) (execbackend.ExecResult, error)
	SendInput(ctx context.Context, pid int, data []byte) error
	// UploadHomeFile writes a new file named name (one plain name gitmoot
	// chose) directly in the guest home, for a file operation's input.
	UploadHomeFile(ctx context.Context, name string, reader io.Reader) error
}

type workspaceStream interface {
	Wait() (execbackend.ExecResult, error)
}

// remoteWorkspaceBackend is the E2B/sandboxd remote backend.
type remoteWorkspaceBackend struct{ backend *remoteexec.Backend }

func (r remoteWorkspaceBackend) provision(ctx context.Context, workspaceID string, ttl time.Duration) (string, workspaceSandbox, error) {
	sandbox, err := r.backend.ProvisionWorkspace(ctx, workspaceID, ttl)
	if err != nil {
		return "", nil, err
	}
	return sandbox.ID, remoteWorkspaceSandbox{sandbox}, nil
}

func (r remoteWorkspaceBackend) attach(ctx context.Context, instanceID string) (workspaceSandbox, e2b.State, error) {
	sandbox, state, err := r.backend.AttachWorkspace(ctx, instanceID)
	if err != nil {
		return nil, state, err
	}
	return remoteWorkspaceSandbox{sandbox}, state, nil
}

func (r remoteWorkspaceBackend) extend(ctx context.Context, instanceID string, ttl time.Duration) (e2b.State, error) {
	return r.backend.SetWorkspaceTimeout(ctx, instanceID, ttl)
}

func (r remoteWorkspaceBackend) destroy(ctx context.Context, instanceID string) (e2b.State, error) {
	return r.backend.DestroyWorkspace(ctx, instanceID)
}

// find lists the account's sandboxes. E2B's list cannot prove absence, so an
// empty result is never authoritative.
func (r remoteWorkspaceBackend) find(ctx context.Context, workspaceID string) (string, bool, error) {
	id, err := r.backend.FindWorkspace(ctx, workspaceID)
	return id, false, err
}

// deadlineGrace is the remote backend's own provider TTL grace, the bound
// the job reaper also relies on.
func (r remoteWorkspaceBackend) deadlineGrace() (time.Duration, bool) {
	return r.backend.ProviderTTLGrace(), true
}

type remoteWorkspaceSandbox struct{ *remoteexec.Workspace }

func (s remoteWorkspaceSandbox) Start(ctx context.Context, request e2b.StartRequest) (workspaceStream, error) {
	return s.Workspace.Start(ctx, request)
}

// localWorkspaceBackend runs workspaces as host processes in local
// execution-backend instances. Each instance is <root>/<id>; its workspace
// directory is the work tree and the instance directory is the guest home.
type localWorkspaceBackend struct{ backend *execbackend.LocalBackend }

func (l localWorkspaceBackend) provision(ctx context.Context, workspaceID string, _ time.Duration) (string, workspaceSandbox, error) {
	instance, err := l.backend.Provision(ctx, execbackend.JobScope{JobID: execbackend.WorkspaceJobIDPrefix + workspaceID})
	if err != nil {
		return "", nil, err
	}
	sandbox, err := l.sandbox(instance)
	if err != nil {
		_ = l.backend.Destroy(context.WithoutCancel(ctx), instance)
		return "", nil, err
	}
	return instance.ID, sandbox, nil
}

func (l localWorkspaceBackend) attach(ctx context.Context, instanceID string) (workspaceSandbox, e2b.State, error) {
	instance, err := l.backend.Attach(ctx, instanceID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, e2b.Gone, err
		}
		return nil, e2b.Unknown, err
	}
	sandbox, err := l.sandbox(instance)
	if err != nil {
		return nil, e2b.Unknown, err
	}
	return sandbox, e2b.Present, nil
}

func (l localWorkspaceBackend) extend(ctx context.Context, instanceID string, _ time.Duration) (e2b.State, error) {
	if _, err := l.backend.Attach(ctx, instanceID); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return e2b.Gone, err
		}
		return e2b.Unknown, err
	}
	return e2b.Present, nil
}

// destroy stops every process of the instance, then removes it. Removing
// the directory alone would leave a running command (and the host exec still
// feeding its watchdog) alive after close reported the workspace closed.
func (l localWorkspaceBackend) destroy(ctx context.Context, instanceID string) (e2b.State, error) {
	instance, err := l.backend.Attach(ctx, instanceID)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return e2b.Unknown, err
	}
	if err == nil {
		home, err := l.backend.InstanceHome(instance)
		if err != nil {
			return e2b.Unknown, err
		}
		if err := execbackend.StopWorkspaceProcesses(home, workspaceKillGrace+2*time.Second); err != nil {
			return e2b.Unknown, err
		}
	}
	if err := l.backend.Destroy(ctx, &execbackend.Instance{ID: instanceID}); err != nil {
		return e2b.Unknown, err
	}
	return e2b.Gone, nil
}

// deadlineGrace: local instances have no provider deadline; they end only
// when destroy confirms it.
func (localWorkspaceBackend) deadlineGrace() (time.Duration, bool) { return 0, false }

// find scans the local instances, which is a complete inventory.
func (l localWorkspaceBackend) find(_ context.Context, workspaceID string) (string, bool, error) {
	id, err := l.backend.FindInstanceByJobID(execbackend.WorkspaceJobIDPrefix + workspaceID)
	return id, true, err
}

func (l localWorkspaceBackend) sandbox(instance *execbackend.Instance) (*localWorkspaceSandbox, error) {
	home, err := l.backend.InstanceHome(instance)
	if err != nil {
		return nil, err
	}
	return &localWorkspaceSandbox{backend: l.backend, instance: instance, home: home, stdins: map[int]io.WriteCloser{}}, nil
}

type localWorkspaceSandbox struct {
	backend  *execbackend.LocalBackend
	instance *execbackend.Instance
	home     string

	mu     sync.Mutex
	stdins map[int]io.WriteCloser
}

func (s *localWorkspaceSandbox) Home() string { return s.home }
func (s *localWorkspaceSandbox) Root() string { return filepath.Clean(s.instance.Workspace) }

// localWorkspaceSyncScript mirrors the remote sync: an object-only fetch from
// the host clone (no remote or config is recorded), the branch at head
// tracking gitmoot-base, and the login-shell PATH. $1 work tree, $2 head,
// $3 base, $4 branch, $5 PATH, $6 host clone.
const localWorkspaceSyncScript = `set -eu
root=$1
head=$2
base=$3
branch=$4
workspace_path=$5
source=$6
rm -rf "$root"
git init -q "$root"
cd "$root"
git -c gc.auto=0 fetch --quiet --no-tags --no-write-fetch-head "$source" "$head" "$base"
git config user.name gitmoot
git config user.email gitmoot@localhost
git branch gitmoot-base "$base"
git checkout -q -b "$branch" "$head"
git branch -q --set-upstream-to=gitmoot-base "$branch"
cd ..
mkdir -p .gitmoot/workspace/logs
{
  echo '# gitmoot workspace: /etc/profile resets PATH; restore the workspace PATH.'
  echo 'if [ -f "$HOME/.profile" ]; then . "$HOME/.profile"; fi'
  printf "PATH='%s'\nexport PATH\n" "$workspace_path"
} > .bash_profile
git -C "$root" rev-parse HEAD`

func (s *localWorkspaceSandbox) Sync(ctx context.Context, _ string, source remoteexec.WorkspaceSource, workspacePATH string) (string, error) {
	result, err := s.Run(ctx, e2b.StartRequest{
		Name: "sh",
		Args: []string{"-c", localWorkspaceSyncScript, "gitmoot-ws-sync", s.Root(), source.Head, source.Base, source.Branch, workspacePATH, source.Checkout},
		Dir:  s.home,
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

type localWorkspaceStream struct {
	result chan localWorkspaceResult
}

type localWorkspaceResult struct {
	result execbackend.ExecResult
	err    error
}

func (s *localWorkspaceStream) Wait() (execbackend.ExecResult, error) {
	result := <-s.result
	return result.result, result.err
}

// Start runs one process in the instance with exactly the environment the
// request names plus the fixed base PATH and HOME envd would give it, never
// this process's environment.
func (s *localWorkspaceSandbox) Start(ctx context.Context, request e2b.StartRequest) (workspaceStream, error) {
	stdout := &workspaceTail{max: request.MaxOutputBytes, out: request.Output}
	errOut := request.ErrOutput
	if errOut == nil {
		errOut = request.Output
	}
	stderr := &workspaceTail{max: request.MaxOutputBytes, out: errOut}
	env := []string{"PATH=" + workspaceBasePATH, "HOME=" + s.home}
	env = append(env, request.Env...)
	process := execbackend.WorkspaceProcess{Dir: request.Dir, Env: env, Name: request.Name, Args: request.Args, Stdout: stdout, Stderr: stderr}
	var stdin io.WriteCloser
	if request.Stdin {
		reader, writer, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		defer reader.Close()
		process.Stdin, stdin = reader, writer
	}
	cmd, forget, err := s.backend.StartWorkspaceProcess(s.instance, process)
	if err != nil {
		if stdin != nil {
			_ = stdin.Close()
		}
		return nil, err
	}
	pid := cmd.Process.Pid
	if stdin != nil {
		s.mu.Lock()
		s.stdins[pid] = stdin
		s.mu.Unlock()
	}
	if request.OnStart != nil {
		request.OnStart(pid)
	}
	stream := &localWorkspaceStream{result: make(chan localWorkspaceResult, 1)}
	go func() {
		waitErr := cmd.Wait()
		forget()
		if stdin != nil {
			s.mu.Lock()
			delete(s.stdins, pid)
			s.mu.Unlock()
			_ = stdin.Close()
		}
		result := execbackend.ExecResult{Command: request.Name, Args: request.Args, Stdout: stdout.String(), Stderr: stderr.String()}
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			waitErr = &e2b.ExitError{Code: exitErr.ExitCode(), Status: exitErr.String()}
		}
		stream.result <- localWorkspaceResult{result: result, err: waitErr}
	}()
	return stream, nil
}

func (s *localWorkspaceSandbox) Run(ctx context.Context, request e2b.StartRequest) (execbackend.ExecResult, error) {
	stream, err := s.Start(ctx, request)
	if err != nil {
		return execbackend.ExecResult{}, err
	}
	return stream.Wait()
}

func (s *localWorkspaceSandbox) UploadHomeFile(_ context.Context, name string, reader io.Reader) error {
	return s.backend.WriteWorkspaceHomeFile(s.instance, name, reader)
}

func (s *localWorkspaceSandbox) SendInput(_ context.Context, pid int, data []byte) error {
	s.mu.Lock()
	stdin := s.stdins[pid]
	s.mu.Unlock()
	if stdin == nil {
		return fmt.Errorf("local workspace process %d has no stdin", pid)
	}
	_, err := stdin.Write(data)
	return err
}

// workspaceTail passes output through and keeps a bounded tail of it.
type workspaceTail struct {
	mu   sync.Mutex
	max  int
	out  io.Writer
	data []byte
}

func (t *workspaceTail) Write(data []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.data = append(t.data, data...)
	if t.max > 0 && len(t.data) > t.max {
		t.data = append(t.data[:0], t.data[len(t.data)-t.max:]...)
	}
	if t.out != nil {
		if _, err := t.out.Write(data); err != nil {
			return 0, err
		}
	}
	return len(data), nil
}

func (t *workspaceTail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.data)
}

// workspaceRootIdentityScript prints the root's st_dev and st_ino from the
// same os.lstat the supervisor's fstat compares against, and refuses a root
// that is not a directory (a symlink included: lstat does not follow it).
const workspaceRootIdentityScript = `import os, stat, sys
info = os.lstat(sys.argv[1])
if not stat.S_ISDIR(info.st_mode):
    sys.stderr.write("workspace root %s is not a directory\n" % sys.argv[1])
    sys.exit(3)
print(info.st_dev, info.st_ino)`

// workspaceRootIdentity reads the root directory's (st_dev, st_ino) inside
// the workspace, with python3, which the exec supervisor needs anyway.
func workspaceRootIdentity(ctx context.Context, sandbox workspaceSandbox) (uint64, uint64, error) {
	result, err := sandbox.Run(ctx, e2b.StartRequest{
		Name:           "python3",
		Args:           []string{"-c", workspaceRootIdentityScript, sandbox.Root()},
		Dir:            sandbox.Home(),
		MaxOutputBytes: 4 << 10,
	})
	if err != nil {
		return 0, 0, fmt.Errorf("read the workspace root identity: %w: %s", err, strings.TrimSpace(result.Stderr))
	}
	fields := strings.Fields(result.Stdout)
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("read the workspace root identity: unexpected output %q", result.Stdout)
	}
	dev, devErr := strconv.ParseUint(fields[0], 10, 64)
	ino, inoErr := strconv.ParseUint(fields[1], 10, 64)
	if devErr != nil || inoErr != nil || (dev == 0 && ino == 0) {
		return 0, 0, fmt.Errorf("read the workspace root identity: unexpected output %q", result.Stdout)
	}
	return dev, ino, nil
}

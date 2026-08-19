package execbackend

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/gitmoot/gitmoot/internal/subprocess"
)

// ExecutionBackend is the job-scoped lifecycle contract decided in #1535.
// One instance is provisioned for one job run and remains attached across all
// Mailbox delivery/repair turns. SyncIn transports checkout state but never
// credentials; Exec must stream while it runs; Collect returns the existing
// bounded ChangeSet transport; Cancel and Destroy are idempotent.
type ExecutionBackend interface {
	Name() Backend
	Provision(context.Context, JobScope) (*Instance, error)
	Attach(context.Context, string) (*Instance, error)
	SyncIn(context.Context, *Instance, Materials) error
	Exec(context.Context, *Instance, Command) (Stream, error)
	Collect(context.Context, *Instance) (ChangeSet, error)
	Cancel(context.Context, *Instance) error
	Destroy(context.Context, *Instance) error
}

// Reaper is the optional provider recovery contract. It reconciles durable
// instances whose owning daemon process is no longer alive.
type Reaper interface {
	Reap(context.Context) ([]string, error)
}

type JobScope struct {
	JobID               string
	LifecycleGeneration int64
}

// Materials are non-secret inputs staged into one execution instance. The
// local provider uses the existing BuildChangeSet/ImportChangeSet pair to copy
// any uncommitted source state after creating its detached Git worktree.
type Materials struct {
	SourceWorktree string
}

type Instance struct {
	ID                  string
	JobID               string
	LifecycleGeneration int64
	Workspace           string
	BaseHEAD            string

	root           string
	sourceWorktree string
	gitCommonDir   string
}

// Command describes one streaming command inside an instance. Dir must be the
// instance workspace or one of its descendants. Env entries are appended to
// the inherited environment. Output receives stdout/stderr live while Result
// retains them independently.
type Command struct {
	Dir            string
	Env            []string
	BaseEnv        []string
	ScratchDirs    []string
	MaxOutputBytes int
	Name           string
	Args           []string
	Output         io.Writer
	OnStart        func(pid int)
}

type ExecResult struct {
	Command string
	Args    []string
	Stdout  string
	Stderr  string
}

type Stream interface {
	Wait() (ExecResult, error)
}

const (
	localMetadataName  = "instance.json"
	localWorkspaceName = "workspace"
)

type localInstanceMetadata struct {
	Version             int    `json:"version"`
	ID                  string `json:"id"`
	JobID               string `json:"job_id"`
	LifecycleGeneration int64  `json:"lifecycle_generation"`
	OwnerPID            int    `json:"owner_pid"`
	OwnerBootID         string `json:"owner_boot_id,omitempty"`
	OwnerStartTime      string `json:"owner_start_time,omitempty"`
	SourceWorktree      string `json:"source_worktree,omitempty"`
	GitCommonDir        string `json:"git_common_dir,omitempty"`
	Workspace           string `json:"workspace"`
	BaseHEAD            string `json:"base_head,omitempty"`
	State               string `json:"state"`
}

// LocalBackend executes a job in a detached Git worktree on the same
// filesystem as its host checkout. The absolute gitdir pointer is usable in
// this topology, so remote-only bundle hydration is deliberately absent.
type LocalBackend struct {
	root string

	mu     sync.Mutex
	active map[string]map[*localExec]struct{}

	// afterWorkspaceCreated is a test-only crash seam. It fires after Git has
	// registered the worktree but before SyncIn marks the instance running.
	afterWorkspaceCreated func(*Instance)
}

func NewLocalBackend(root string) (*LocalBackend, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, errors.New("local execution-backend root is required")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve local execution-backend root: %w", err)
	}
	return &LocalBackend{root: filepath.Clean(absolute), active: make(map[string]map[*localExec]struct{})}, nil
}

func (b *LocalBackend) Name() Backend { return Local }

func (b *LocalBackend) Provision(_ context.Context, scope JobScope) (*Instance, error) {
	if b == nil {
		return nil, errors.New("local execution backend is nil")
	}
	if strings.TrimSpace(scope.JobID) == "" {
		return nil, errors.New("local execution backend job id is required")
	}
	if err := os.MkdirAll(b.root, 0o700); err != nil {
		return nil, fmt.Errorf("create local execution-backend root: %w", err)
	}
	id, err := localInstanceID()
	if err != nil {
		return nil, err
	}
	instanceRoot := filepath.Join(b.root, id)
	if err := os.Mkdir(instanceRoot, 0o700); err != nil {
		return nil, fmt.Errorf("reserve local execution instance %q: %w", id, err)
	}
	instance := &Instance{
		ID:                  id,
		JobID:               strings.TrimSpace(scope.JobID),
		LifecycleGeneration: scope.LifecycleGeneration,
		Workspace:           filepath.Join(instanceRoot, localWorkspaceName),
		root:                instanceRoot,
	}
	meta := metadataForInstance(instance, "provisioning")
	if err := writeLocalMetadata(instanceRoot, meta); err != nil {
		_ = os.Remove(instanceRoot)
		return nil, err
	}
	return instance, nil
}

func (b *LocalBackend) Attach(_ context.Context, id string) (*Instance, error) {
	instanceRoot, err := b.instanceRoot(id)
	if err != nil {
		return nil, err
	}
	meta, err := readLocalMetadata(instanceRoot)
	if err != nil {
		return nil, fmt.Errorf("attach local execution instance %q: %w", id, err)
	}
	if meta.ID != strings.TrimSpace(id) {
		return nil, fmt.Errorf("attach local execution instance %q: metadata id is %q", id, meta.ID)
	}
	return instanceFromMetadata(instanceRoot, meta), nil
}

func (b *LocalBackend) SyncIn(ctx context.Context, instance *Instance, materials Materials) error {
	if err := b.validateInstance(instance); err != nil {
		return err
	}
	source := strings.TrimSpace(materials.SourceWorktree)
	if source == "" {
		return errors.New("local execution backend source worktree is required")
	}
	absolute, err := filepath.Abs(source)
	if err != nil {
		return fmt.Errorf("resolve local execution backend source worktree: %w", err)
	}
	source = filepath.Clean(absolute)
	base, err := localGitOutput(ctx, source, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("read local execution backend base HEAD: %w", err)
	}
	base = strings.TrimSpace(base)
	commonDir, err := localGitOutput(ctx, source, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return fmt.Errorf("read local execution backend Git common directory: %w", err)
	}
	instance.sourceWorktree = source
	instance.gitCommonDir = filepath.Clean(strings.TrimSpace(commonDir))
	instance.BaseHEAD = base
	if err := writeLocalMetadata(instance.root, metadataForInstance(instance, "syncing")); err != nil {
		return err
	}
	if output, err := localGitCombined(ctx, source, "worktree", "add", "--detach", "--force", instance.Workspace, base); err != nil {
		return fmt.Errorf("create local execution workspace: %w: %s", err, strings.TrimSpace(output))
	}
	if b.afterWorkspaceCreated != nil {
		b.afterWorkspaceCreated(instance)
	}
	changes, err := BuildChangeSet(ctx, source, base)
	if err != nil {
		return fmt.Errorf("capture local execution sync input: %w", err)
	}
	if err := ImportChangeSet(ctx, instance.Workspace, changes); err != nil {
		return fmt.Errorf("materialize local execution sync input: %w", err)
	}
	return writeLocalMetadata(instance.root, metadataForInstance(instance, "running"))
}

func (b *LocalBackend) Exec(ctx context.Context, instance *Instance, command Command) (Stream, error) {
	if err := b.validateInstance(instance); err != nil {
		return nil, err
	}
	if strings.TrimSpace(command.Name) == "" {
		return nil, errors.New("local execution backend command is required")
	}
	dir, err := localCommandDir(instance.Workspace, command.Dir)
	if err != nil {
		return nil, err
	}
	execCtx, cancel := context.WithCancel(ctx)
	running := &localExec{cancel: cancel, done: make(chan struct{})}
	b.register(instance.ID, running)
	stream := &localStream{result: make(chan localExecResult, 1)}
	go func() {
		defer close(running.done)
		defer b.unregister(instance.ID, running)
		defer cancel()
		var runner localCommandRunner = subprocess.GroupRunner{MaxOutputBytes: command.MaxOutputBytes}
		if command.BaseEnv != nil {
			runner = subprocess.CuratedGroupRunner{
				BaseEnv:        command.BaseEnv,
				ScratchDirs:    command.ScratchDirs,
				MaxOutputBytes: command.MaxOutputBytes,
			}
		}
		var result subprocess.Result
		var runErr error
		if command.Output != nil {
			result, runErr = runner.RunEnvStreamWithPID(execCtx, dir, command.Env, command.Output, command.OnStart, command.Name, command.Args...)
		} else {
			result, runErr = runner.RunEnvWithPID(execCtx, dir, command.Env, command.OnStart, command.Name, command.Args...)
		}
		stream.result <- localExecResult{result: ExecResult{
			Command: result.Command,
			Args:    result.Args,
			Stdout:  result.Stdout,
			Stderr:  result.Stderr,
		}, err: runErr}
	}()
	return stream, nil
}

func (b *LocalBackend) Collect(ctx context.Context, instance *Instance) (ChangeSet, error) {
	if err := b.validateInstance(instance); err != nil {
		return ChangeSet{}, err
	}
	if strings.TrimSpace(instance.BaseHEAD) == "" {
		return ChangeSet{}, errors.New("local execution instance has no synced base HEAD")
	}
	return BuildChangeSet(ctx, instance.Workspace, instance.BaseHEAD)
}

func (b *LocalBackend) Cancel(ctx context.Context, instance *Instance) error {
	if err := b.validateInstance(instance); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	b.mu.Lock()
	runs := make([]*localExec, 0, len(b.active[instance.ID]))
	for run := range b.active[instance.ID] {
		runs = append(runs, run)
	}
	b.mu.Unlock()
	for _, run := range runs {
		run.cancel()
	}
	for _, run := range runs {
		select {
		case <-run.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return b.Destroy(ctx, instance)
}

func (b *LocalBackend) Destroy(ctx context.Context, instance *Instance) error {
	if instance == nil {
		return nil
	}
	instanceRoot, err := b.instanceRoot(instance.ID)
	if err != nil {
		return err
	}
	if _, err := os.Stat(instanceRoot); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect local execution instance %q: %w", instance.ID, err)
	}
	meta, metaErr := readLocalMetadata(instanceRoot)
	if metaErr == nil {
		instance = instanceFromMetadata(instanceRoot, meta)
	}
	_ = writeLocalMetadata(instanceRoot, metadataForInstance(instance, "destroying"))
	if _, err := os.Lstat(instance.Workspace); err == nil {
		if strings.TrimSpace(instance.sourceWorktree) == "" {
			if err := os.Remove(instance.Workspace); err != nil {
				return fmt.Errorf("remove orphaned-but-present local execution workspace %q: %w", instance.Workspace, err)
			}
		} else if output, err := localGitDirCombined(ctx, instance.gitCommonDir, "worktree", "remove", "--force", instance.Workspace); err != nil {
			return fmt.Errorf("remove local execution worktree %q: %w: %s", instance.Workspace, err, strings.TrimSpace(output))
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect local execution workspace %q: %w", instance.Workspace, err)
	}
	if err := os.Remove(filepath.Join(instanceRoot, localMetadataName)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove local execution metadata %q: %w", instance.ID, err)
	}
	if err := os.Remove(instanceRoot); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove local execution instance %q: %w", instance.ID, err)
	}
	return nil
}

// Reap destroys instances whose recorded owner process is no longer the same
// live daemon process. A partially-created non-empty directory that Git never
// registered remains the known #1572 orphaned-but-present failure shape and is
// returned loudly rather than recursively deleted.
func (b *LocalBackend) Reap(ctx context.Context) ([]string, error) {
	entries, err := os.ReadDir(b.root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scan local execution-backend root: %w", err)
	}
	var reaped []string
	var reapErrs []error
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		root := filepath.Join(b.root, entry.Name())
		meta, err := readLocalMetadata(root)
		if err != nil {
			continue
		}
		if localOwnerAlive(meta.OwnerPID, meta.OwnerBootID, meta.OwnerStartTime) {
			continue
		}
		instance := instanceFromMetadata(root, meta)
		if err := b.Destroy(ctx, instance); err != nil {
			reapErrs = append(reapErrs, fmt.Errorf("reap local execution instance %q: %w", meta.ID, err))
			continue
		}
		reaped = append(reaped, meta.ID)
	}
	return reaped, errors.Join(reapErrs...)
}

func (b *LocalBackend) validateInstance(instance *Instance) error {
	if instance == nil {
		return errors.New("local execution instance is required")
	}
	root, err := b.instanceRoot(instance.ID)
	if err != nil {
		return err
	}
	if filepath.Clean(instance.root) != root || filepath.Clean(instance.Workspace) != filepath.Join(root, localWorkspaceName) {
		return fmt.Errorf("local execution instance %q is outside backend root", instance.ID)
	}
	return nil
}

func (b *LocalBackend) instanceRoot(id string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" || filepath.Base(id) != id || id == "." || id == ".." {
		return "", fmt.Errorf("invalid local execution instance id %q", id)
	}
	root := filepath.Join(b.root, id)
	if filepath.Dir(root) != b.root {
		return "", fmt.Errorf("local execution instance %q escapes backend root", id)
	}
	return root, nil
}

func (b *LocalBackend) register(id string, running *localExec) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.active[id] == nil {
		b.active[id] = make(map[*localExec]struct{})
	}
	b.active[id][running] = struct{}{}
}

func (b *LocalBackend) unregister(id string, running *localExec) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.active[id], running)
	if len(b.active[id]) == 0 {
		delete(b.active, id)
	}
}

type localExec struct {
	cancel context.CancelFunc
	done   chan struct{}
}

type localExecResult struct {
	result ExecResult
	err    error
}

type localCommandRunner interface {
	subprocess.EnvPIDRunner
	subprocess.EnvPIDStreamRunner
}

type localStream struct {
	result chan localExecResult
}

func (s *localStream) Wait() (ExecResult, error) {
	result := <-s.result
	return result.result, result.err
}

func localCommandDir(workspace, dir string) (string, error) {
	workspace = filepath.Clean(workspace)
	if strings.TrimSpace(dir) == "" {
		return workspace, nil
	}
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve local execution command directory: %w", err)
	}
	absolute = filepath.Clean(absolute)
	rel, err := filepath.Rel(workspace, absolute)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("local execution command directory %q escapes workspace %q", dir, workspace)
	}
	return absolute, nil
}

func localInstanceID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate local execution instance id: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

func metadataForInstance(instance *Instance, state string) localInstanceMetadata {
	return localInstanceMetadata{
		Version:             1,
		ID:                  instance.ID,
		JobID:               instance.JobID,
		LifecycleGeneration: instance.LifecycleGeneration,
		OwnerPID:            os.Getpid(),
		OwnerBootID:         localBootID(),
		OwnerStartTime:      localProcessStartTime(os.Getpid()),
		SourceWorktree:      instance.sourceWorktree,
		GitCommonDir:        instance.gitCommonDir,
		Workspace:           instance.Workspace,
		BaseHEAD:            instance.BaseHEAD,
		State:               state,
	}
}

func instanceFromMetadata(root string, meta localInstanceMetadata) *Instance {
	return &Instance{
		ID:                  meta.ID,
		JobID:               meta.JobID,
		LifecycleGeneration: meta.LifecycleGeneration,
		Workspace:           meta.Workspace,
		BaseHEAD:            meta.BaseHEAD,
		root:                root,
		sourceWorktree:      meta.SourceWorktree,
		gitCommonDir:        meta.GitCommonDir,
	}
}

func writeLocalMetadata(root string, meta localInstanceMetadata) error {
	temporary, err := os.CreateTemp(root, ".instance-*.json")
	if err != nil {
		return fmt.Errorf("create local execution metadata: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("protect local execution metadata: %w", err)
	}
	encoder := json.NewEncoder(temporary)
	if err := encoder.Encode(meta); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("encode local execution metadata: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close local execution metadata: %w", err)
	}
	if err := os.Rename(temporaryName, filepath.Join(root, localMetadataName)); err != nil {
		return fmt.Errorf("publish local execution metadata: %w", err)
	}
	return nil
}

func readLocalMetadata(root string) (localInstanceMetadata, error) {
	data, err := os.ReadFile(filepath.Join(root, localMetadataName))
	if err != nil {
		return localInstanceMetadata{}, err
	}
	var meta localInstanceMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return localInstanceMetadata{}, err
	}
	if meta.Version != 1 || strings.TrimSpace(meta.ID) == "" {
		return localInstanceMetadata{}, fmt.Errorf("unsupported local execution metadata version %d", meta.Version)
	}
	return meta, nil
}

func localGitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	output, err := localGitCombined(ctx, dir, args...)
	return output, err
}

func localGitCombined(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func localGitDirCombined(ctx context.Context, gitDir string, args ...string) (string, error) {
	if strings.TrimSpace(gitDir) == "" {
		return "", errors.New("Git common directory is unavailable")
	}
	cmd := exec.CommandContext(ctx, "git", append([]string{"--git-dir", gitDir}, args...)...)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func localBootID() string {
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func localProcessStartTime(pid int) string {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return ""
	}
	closeParen := strings.LastIndexByte(string(data), ')')
	if closeParen < 0 || closeParen+2 >= len(data) {
		return ""
	}
	fields := strings.Fields(string(data[closeParen+2:]))
	if len(fields) <= 19 {
		return ""
	}
	return fields[19]
}

func localOwnerAlive(pid int, bootID, startTime string) bool {
	if pid <= 0 {
		return false
	}
	currentBoot := localBootID()
	if strings.TrimSpace(bootID) != "" && currentBoot != "" && strings.TrimSpace(bootID) != currentBoot {
		return false
	}
	if strings.TrimSpace(startTime) != "" {
		return localProcessStartTime(pid) == strings.TrimSpace(startTime)
	}
	return syscall.Kill(pid, 0) == nil
}

// InstanceRunner adapts ExecutionBackend.Exec to the subprocess runner
// capabilities runtime adapters already consume. All Run variants stream when
// requested and preserve PID callbacks; LookPath is host-equivalent for the
// same-filesystem local provider.
type InstanceRunner struct {
	Backend        ExecutionBackend
	Instance       *Instance
	BaseEnv        []string
	ScratchDirs    []string
	MaxOutputBytes int
}

func (r InstanceRunner) run(ctx context.Context, dir string, env []string, out io.Writer, onPID subprocess.PIDCallback, command string, args ...string) (subprocess.Result, error) {
	if r.Backend == nil || r.Instance == nil {
		return subprocess.Result{}, errors.New("execution-backend instance runner is not attached")
	}
	stream, err := r.Backend.Exec(ctx, r.Instance, Command{
		Dir:            dir,
		Env:            env,
		BaseEnv:        r.BaseEnv,
		ScratchDirs:    r.ScratchDirs,
		MaxOutputBytes: r.MaxOutputBytes,
		Name:           command,
		Args:           args,
		Output:         out,
		OnStart:        onPID,
	})
	if err != nil {
		return subprocess.Result{}, err
	}
	result, err := stream.Wait()
	return subprocess.Result{Command: result.Command, Args: result.Args, Stdout: result.Stdout, Stderr: result.Stderr}, err
}

func (r InstanceRunner) Run(ctx context.Context, dir string, command string, args ...string) (subprocess.Result, error) {
	return r.run(ctx, dir, nil, nil, nil, command, args...)
}

func (r InstanceRunner) RunEnv(ctx context.Context, dir string, env []string, command string, args ...string) (subprocess.Result, error) {
	return r.run(ctx, dir, env, nil, nil, command, args...)
}

func (r InstanceRunner) RunWithPID(ctx context.Context, dir string, onPID subprocess.PIDCallback, command string, args ...string) (subprocess.Result, error) {
	return r.run(ctx, dir, nil, nil, onPID, command, args...)
}

func (r InstanceRunner) RunEnvWithPID(ctx context.Context, dir string, env []string, onPID subprocess.PIDCallback, command string, args ...string) (subprocess.Result, error) {
	return r.run(ctx, dir, env, nil, onPID, command, args...)
}

func (r InstanceRunner) RunStream(ctx context.Context, dir string, out io.Writer, command string, args ...string) (subprocess.Result, error) {
	return r.run(ctx, dir, nil, out, nil, command, args...)
}

func (r InstanceRunner) RunEnvStream(ctx context.Context, dir string, env []string, out io.Writer, command string, args ...string) (subprocess.Result, error) {
	return r.run(ctx, dir, env, out, nil, command, args...)
}

func (r InstanceRunner) RunStreamWithPID(ctx context.Context, dir string, out io.Writer, onPID subprocess.PIDCallback, command string, args ...string) (subprocess.Result, error) {
	return r.run(ctx, dir, nil, out, onPID, command, args...)
}

func (r InstanceRunner) RunEnvStreamWithPID(ctx context.Context, dir string, env []string, out io.Writer, onPID subprocess.PIDCallback, command string, args ...string) (subprocess.Result, error) {
	return r.run(ctx, dir, env, out, onPID, command, args...)
}

func (InstanceRunner) LookPath(file string) (string, error) { return exec.LookPath(file) }

var (
	_ ExecutionBackend              = (*LocalBackend)(nil)
	_ Reaper                        = (*LocalBackend)(nil)
	_ subprocess.EnvPIDStreamRunner = InstanceRunner{}
)

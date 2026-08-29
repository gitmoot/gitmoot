// Package remote implements the provider-backed execution lifecycle.
// Construction is deliberately not wired into the daemon until Slice D.
package remote

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/gitmoot/gitmoot/internal/execbackend"
	"github.com/gitmoot/gitmoot/internal/execbackend/e2b"
)

const (
	workspacePath   = "/home/user/workspace"
	syncArchivePath = "/home/user/.gitmoot-sync.tar.gz"

	metadataJobID               = "job_id"
	metadataAttempt             = "attempt"
	metadataLifecycleGeneration = "lifecycle_generation"
	metadataDaemonFencingToken  = "daemon_fencing_token"
	metadataBootID              = "boot_id"
	metadataOwnerPID            = "owner_pid"
	metadataOwnerPIDNamespace   = "owner_pid_namespace"
	metadataOwnerStartTime      = "owner_start_time"
)

const syncWorkspaceScript = `set -eu
rm -rf /home/user/workspace /home/user/.gitmoot-sync
mkdir -p /home/user/.gitmoot-sync
tar -xzf /home/user/.gitmoot-sync.tar.gz -C /home/user/.gitmoot-sync
mv /home/user/.gitmoot-sync/workspace /home/user/workspace
cd /home/user/workspace
git init -q
git config user.name gitmoot
git config user.email gitmoot@localhost
git add -A
git commit -q --allow-empty -m 'gitmoot sync base'
if [ -s /home/user/.gitmoot-sync/changes.patch ]; then
  git apply --binary --whitespace=nowarn /home/user/.gitmoot-sync/changes.patch
fi
rm -rf /home/user/.gitmoot-sync /home/user/.gitmoot-sync.tar.gz
git rev-parse HEAD`

const collectPatchScript = `set -eu
git add -A
git diff --binary --full-index --no-ext-diff --no-renames --cached HEAD --`

// Options configures a remote Backend. TemplateID is required. Envd is copied
// into each sandbox-scoped data-plane client.
type Options struct {
	TemplateID string
	Envd       e2b.EnvdOptions
}

// Backend owns E2B sandboxes for one daemon process.
type Backend struct {
	client     *e2b.Client
	templateID string
	envd       e2b.EnvdOptions

	bootID       string
	pidNamespace string
	ownerPID     int
	ownerStart   string
	ownerAlive   func(pid int, bootID, startTime string) bool

	mu        sync.Mutex
	sandboxes map[string]*sandboxState
}

type sandboxState struct {
	mu sync.Mutex

	sandbox      e2b.Sandbox
	envd         *e2b.Envd
	jobID        string
	generation   int64
	hostWorktree string
	hostBase     string
	remoteBase   string
}

var _ execbackend.ExecutionBackend = (*Backend)(nil)
var _ execbackend.Reaper = (*Backend)(nil)
var _ execbackend.InventoryReaper = (*Backend)(nil)
var _ execbackend.CredentialMaterialInstaller = (*Backend)(nil)

// NewBackend constructs an unwired E2B lifecycle provider.
func NewBackend(client *e2b.Client, options Options) (*Backend, error) {
	if client == nil {
		return nil, errors.New("remote execution backend E2B client is required")
	}
	templateID := strings.TrimSpace(options.TemplateID)
	if templateID == "" {
		return nil, errors.New("remote execution backend template ID is required")
	}
	pid := os.Getpid()
	return &Backend{
		client:       client,
		templateID:   templateID,
		envd:         options.Envd,
		bootID:       hostBootID(),
		pidNamespace: processPIDNamespace(),
		ownerPID:     pid,
		ownerStart:   processStartTime(pid),
		ownerAlive:   processOwnerAlive,
		sandboxes:    make(map[string]*sandboxState),
	}, nil
}

func (b *Backend) Name() execbackend.Backend { return execbackend.Remote }

func (b *Backend) Provision(ctx context.Context, scope execbackend.JobScope) (*execbackend.Instance, error) {
	if b == nil {
		return nil, errors.New("remote execution backend is nil")
	}
	jobID := strings.TrimSpace(scope.JobID)
	if jobID == "" {
		return nil, errors.New("remote execution backend job id is required")
	}
	if scope.TTL <= 0 {
		return nil, errors.New("remote execution backend TTL must be positive")
	}
	attempt := scope.Attempt
	if attempt == 0 {
		attempt = 1
	}
	if attempt < 1 {
		return nil, errors.New("remote execution backend attempt must be positive")
	}
	metadata := map[string]string{
		metadataJobID:               jobID,
		metadataAttempt:             strconv.Itoa(attempt),
		metadataLifecycleGeneration: strconv.FormatInt(scope.LifecycleGeneration, 10),
		metadataBootID:              b.bootID,
		metadataOwnerPID:            strconv.Itoa(b.ownerPID),
		metadataOwnerPIDNamespace:   b.pidNamespace,
		metadataOwnerStartTime:      b.ownerStart,
	}
	if token := strings.TrimSpace(scope.DaemonFencingToken); token != "" {
		metadata[metadataDaemonFencingToken] = token
	}
	sandbox, credential, err := b.client.Create(ctx, b.templateID, scope.TTL, e2b.CreateOptions{Metadata: metadata})
	if err != nil {
		return nil, fmt.Errorf("provision remote execution sandbox: %w", err)
	}
	envd, envdErr := e2b.NewEnvd(sandbox, credential, b.envd)
	if envdErr != nil {
		_, deleteErr := b.client.Delete(context.WithoutCancel(ctx), sandbox.ID)
		return nil, errors.Join(fmt.Errorf("construct remote sandbox data plane: %w", envdErr), deleteErr)
	}
	state := &sandboxState{
		sandbox:    sandbox,
		envd:       envd,
		jobID:      jobID,
		generation: scope.LifecycleGeneration,
	}
	b.mu.Lock()
	b.sandboxes[sandbox.ID] = state
	b.mu.Unlock()
	return state.instance(), nil
}

// Attach reuses only credentials retained by this process. Durable reattach is
// owned by #1539 and cannot be reconstructed from an ID alone.
func (b *Backend) Attach(_ context.Context, id string) (*execbackend.Instance, error) {
	state, err := b.stateByID(id)
	if err != nil {
		return nil, err
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.instance(), nil
}

func (b *Backend) SyncIn(ctx context.Context, instance *execbackend.Instance, materials execbackend.Materials) error {
	state, err := b.stateFor(instance)
	if err != nil {
		return err
	}
	hostWorktree := strings.TrimSpace(materials.SourceWorktree)
	if hostWorktree == "" {
		return errors.New("remote execution backend source worktree is required")
	}
	hostWorktree, err = filepath.Abs(hostWorktree)
	if err != nil {
		return fmt.Errorf("resolve remote source worktree: %w", err)
	}
	hostWorktree = filepath.Clean(hostWorktree)
	hostBase, err := hostGitOutput(ctx, hostWorktree, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("read remote execution host base HEAD: %w", err)
	}
	hostBase = strings.TrimSpace(hostBase)
	inputChanges, err := execbackend.BuildChangeSet(ctx, hostWorktree, hostBase)
	if err != nil {
		return fmt.Errorf("capture remote execution sync input: %w", err)
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	if err := execbackend.StageAt(ctx, hostWorktree, hostBase, func(stage string) error {
		var inputPatch []byte
		if len(inputChanges.Patch) > 0 || len(inputChanges.Manifest) > 0 {
			if err := execbackend.ImportChangeSet(ctx, stage, inputChanges); err != nil {
				return fmt.Errorf("materialize remote sync staging tree: %w", err)
			}
			if _, err := hostGitOutput(ctx, stage, "add", "-A"); err != nil {
				return fmt.Errorf("index remote sync input: %w", err)
			}
			inputPatch, err = hostGitBytes(ctx, stage, "diff", "--binary", "--full-index", "--no-ext-diff", "--no-renames", "--cached", "HEAD", "--")
			if err != nil {
				return fmt.Errorf("capture remote sync patch: %w", err)
			}
			if _, err := hostGitOutput(ctx, stage, "reset", "--hard", "HEAD"); err != nil {
				return fmt.Errorf("restore remote sync base: %w", err)
			}
			if _, err := hostGitOutput(ctx, stage, "clean", "-fdx"); err != nil {
				return fmt.Errorf("clean remote sync base: %w", err)
			}
		}
		return uploadWorkspaceArchive(ctx, state.envd, stage, inputPatch)
	}); err != nil {
		return fmt.Errorf("stage remote execution workspace: %w", err)
	}
	result, err := runEnvd(ctx, state.envd, e2b.StartRequest{
		Name: "sh",
		Args: []string{"-c", syncWorkspaceScript},
		Dir:  "/home/user",
	})
	if err != nil {
		return fmt.Errorf("initialize remote execution workspace: %w", err)
	}
	remoteBase := strings.TrimSpace(result.Stdout)
	if remoteBase == "" || strings.ContainsAny(remoteBase, "\r\n\t ") {
		return fmt.Errorf("initialize remote execution workspace: invalid base HEAD %q", remoteBase)
	}
	state.hostWorktree = hostWorktree
	state.hostBase = hostBase
	state.remoteBase = remoteBase
	instance.BaseHEAD = hostBase
	return nil
}

// InstallCredentialMaterial writes only the short-lived broker identity. The
// upstream model credential is never passed to this method or the sandbox.
func (b *Backend) InstallCredentialMaterial(ctx context.Context, instance *execbackend.Instance, material execbackend.CredentialMaterial) error {
	state, err := b.stateFor(instance)
	if err != nil {
		return err
	}
	files := []struct {
		path string
		data []byte
	}{
		{execbackend.CredentialCACertificatePath, material.CACertificate},
		{execbackend.CredentialClientCertificatePath, material.ClientCertificate},
		{execbackend.CredentialClientPrivateKeyPath, material.ClientPrivateKey},
		{execbackend.CredentialClientConfigPath, material.ClientConfig},
	}
	for _, file := range files {
		if len(file.data) == 0 {
			return fmt.Errorf("remote credential material %s is empty", path.Base(file.path))
		}
		if err := state.envd.Upload(ctx, file.path, bytes.NewReader(file.data)); err != nil {
			return fmt.Errorf("upload remote credential material %s: %w", path.Base(file.path), err)
		}
	}
	result, err := runEnvd(ctx, state.envd, e2b.StartRequest{
		Name: "sh", Args: []string{"-c", "chmod 700 " + execbackend.CredentialMaterialDir + " && chmod 600 " + execbackend.CredentialMaterialDir + "/*"},
		Dir: "/home/user", MaxOutputBytes: 256,
	})
	if err != nil {
		return fmt.Errorf("protect remote credential material: %w", err)
	}
	if strings.TrimSpace(result.Stderr) != "" {
		return errors.New("protect remote credential material returned stderr")
	}
	return nil
}

func (b *Backend) Exec(ctx context.Context, instance *execbackend.Instance, command execbackend.Command) (execbackend.Stream, error) {
	state, err := b.stateFor(instance)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(command.Name) == "" {
		return nil, errors.New("remote execution backend command is required")
	}
	dir := strings.TrimSpace(command.Dir)
	if !path.IsAbs(dir) {
		return nil, fmt.Errorf("remote execution command directory must be absolute: %q", command.Dir)
	}
	dir = path.Clean(dir)
	if dir != workspacePath && !strings.HasPrefix(dir, workspacePath+"/") {
		return nil, fmt.Errorf("remote execution command directory %q escapes workspace %q", command.Dir, workspacePath)
	}
	state.mu.Lock()
	envd := state.envd
	ready := state.remoteBase != ""
	state.mu.Unlock()
	if !ready {
		return nil, errors.New("remote execution instance has not been synced")
	}
	// OnStart is intentionally not forwarded. A sandbox PID checked against host
	// /proc can alias an unrelated host process and authorize a wrong kill.
	stream, err := envd.Start(ctx, e2b.StartRequest{
		Name:           command.Name,
		Args:           append([]string(nil), command.Args...),
		Dir:            dir,
		Env:            append([]string(nil), command.Env...),
		Output:         command.Output,
		MaxOutputBytes: command.MaxOutputBytes,
	})
	if err != nil {
		return nil, fmt.Errorf("start remote execution command: %w", err)
	}
	return stream, nil
}

func (b *Backend) Collect(ctx context.Context, instance *execbackend.Instance) (execbackend.ChangeSet, error) {
	state, err := b.stateFor(instance)
	if err != nil {
		return execbackend.ChangeSet{}, err
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.remoteBase == "" || state.hostBase == "" || state.hostWorktree == "" {
		return execbackend.ChangeSet{}, errors.New("remote execution instance has no synced base HEAD")
	}
	head, err := runEnvd(ctx, state.envd, e2b.StartRequest{
		Name:           "git",
		Args:           []string{"rev-parse", "HEAD"},
		Dir:            workspacePath,
		MaxOutputBytes: 256,
	})
	if err != nil {
		return execbackend.ChangeSet{}, fmt.Errorf("read remote execution HEAD: %w", err)
	}
	if got := strings.TrimSpace(head.Stdout); got != state.remoteBase {
		return execbackend.ChangeSet{}, fmt.Errorf("sandbox-created commits are forbidden: sandbox HEAD %s, expected base %s", got, state.remoteBase)
	}
	patchResult, err := runEnvd(ctx, state.envd, e2b.StartRequest{
		Name:           "sh",
		Args:           []string{"-c", collectPatchScript},
		Dir:            workspacePath,
		MaxOutputBytes: execbackend.MaxChangeSetPatchBytes + 1,
	})
	if err != nil {
		return execbackend.ChangeSet{}, fmt.Errorf("capture remote execution patch: %w", err)
	}
	patch := []byte(patchResult.Stdout)
	if len(patch) > execbackend.MaxChangeSetPatchBytes {
		return execbackend.ChangeSet{}, fmt.Errorf("remote execution patch is larger than limit %d", execbackend.MaxChangeSetPatchBytes)
	}

	var changes execbackend.ChangeSet
	err = execbackend.StageAt(ctx, state.hostWorktree, state.hostBase, func(stage string) error {
		if len(patch) > 0 {
			cmd := exec.CommandContext(ctx, "git", "-C", stage, "apply", "--binary", "--whitespace=nowarn", "-")
			cmd.Stdin = strings.NewReader(string(patch))
			if output, applyErr := cmd.CombinedOutput(); applyErr != nil {
				return fmt.Errorf("apply remote execution patch: %w: %s", applyErr, strings.TrimSpace(string(output)))
			}
		}
		var buildErr error
		changes, buildErr = execbackend.BuildChangeSet(ctx, stage, state.hostBase)
		return buildErr
	})
	if err != nil {
		return execbackend.ChangeSet{}, fmt.Errorf("collect remote execution changes: %w", err)
	}
	return changes, nil
}

func (b *Backend) Cancel(ctx context.Context, instance *execbackend.Instance) error {
	return b.Destroy(ctx, instance)
}

func (b *Backend) Destroy(ctx context.Context, instance *execbackend.Instance) error {
	if instance == nil || strings.TrimSpace(instance.ID) == "" {
		return nil
	}
	b.mu.Lock()
	_, tracked := b.sandboxes[instance.ID]
	b.mu.Unlock()
	if !tracked {
		return nil
	}
	state, err := b.client.Delete(ctx, instance.ID)
	if err != nil {
		return fmt.Errorf("destroy remote execution sandbox %q: %w", instance.ID, err)
	}
	if state != e2b.Gone {
		return fmt.Errorf("destroy remote execution sandbox %q was inconclusive: %s", instance.ID, state)
	}
	b.mu.Lock()
	delete(b.sandboxes, instance.ID)
	b.mu.Unlock()
	return nil
}

// Reap is account-global, unlike LocalBackend.Reap's filesystem-root scope.
// boot_id separates machines, but is shared by every PID namespace on one
// kernel. Require both identities before interpreting PID/start-time liveness,
// so one container cannot reap another container's live sandbox.
func (b *Backend) Reap(ctx context.Context) ([]string, error) {
	report, err := b.ReapInventory(ctx)
	return report.Destroyed, err
}

// ReapInventory returns the provider observations used for the reap decision
// and the directly deleted subset. E2B cannot prove all-state completeness, so
// consumers may reconcile positive observations but not infer absence.
func (b *Backend) ReapInventory(ctx context.Context) (execbackend.ReapReport, error) {
	if b == nil {
		return execbackend.ReapReport{}, errors.New("remote execution backend is nil")
	}
	sandboxes, err := b.client.List(ctx)
	if err != nil {
		return execbackend.ReapReport{}, fmt.Errorf("list remote execution sandboxes for reap: %w", err)
	}
	report := execbackend.ReapReport{
		// E2B has no all-state total. A successful List proves every returned
		// sandbox exists, but a dropped continuation header can still hide a
		// later page, so absence from this inventory is not authoritative.
		InventoryObserved: true,
		Inventory:         make([]execbackend.ProviderInstance, 0, len(sandboxes)),
	}
	var reaped []string
	var reapErrs []error
	for _, sandbox := range sandboxes {
		metadata := sandbox.Metadata
		attempt, _ := strconv.Atoi(strings.TrimSpace(metadata[metadataAttempt]))
		generation := int64(-1)
		if parsed, parseErr := strconv.ParseInt(strings.TrimSpace(metadata[metadataLifecycleGeneration]), 10, 64); parseErr == nil && parsed >= 0 {
			generation = parsed
		}
		report.Inventory = append(report.Inventory, execbackend.ProviderInstance{
			ID:                  sandbox.ID,
			JobID:               strings.TrimSpace(metadata[metadataJobID]),
			Attempt:             attempt,
			LifecycleGeneration: generation,
			DaemonFencingToken:  strings.TrimSpace(metadata[metadataDaemonFencingToken]),
			BootID:              strings.TrimSpace(metadata[metadataBootID]),
		})
		if strings.TrimSpace(metadata[metadataBootID]) == "" || strings.TrimSpace(metadata[metadataBootID]) != b.bootID {
			continue
		}
		if !matchingNonEmptyIdentity(metadata[metadataOwnerPIDNamespace], b.pidNamespace) {
			continue
		}
		pid, parseErr := strconv.Atoi(strings.TrimSpace(metadata[metadataOwnerPID]))
		if parseErr != nil || b.ownerAlive(pid, metadata[metadataBootID], metadata[metadataOwnerStartTime]) {
			continue
		}
		state, deleteErr := b.client.Delete(ctx, sandbox.ID)
		if deleteErr != nil || state != e2b.Gone {
			reapErrs = append(reapErrs, errors.Join(fmt.Errorf("reap remote execution sandbox %q", sandbox.ID), deleteErr))
			continue
		}
		b.mu.Lock()
		delete(b.sandboxes, sandbox.ID)
		b.mu.Unlock()
		reaped = append(reaped, sandbox.ID)
	}
	report.Destroyed = reaped
	return report, errors.Join(reapErrs...)
}

func matchingNonEmptyIdentity(left, right string) bool {
	left = strings.TrimSpace(left)
	right = strings.TrimSpace(right)
	return left != "" && right != "" && left == right
}

func (b *Backend) stateFor(instance *execbackend.Instance) (*sandboxState, error) {
	if instance == nil {
		return nil, errors.New("remote execution instance is required")
	}
	state, err := b.stateByID(instance.ID)
	if err != nil {
		return nil, err
	}
	if instance.Workspace != workspacePath || instance.JobID != state.jobID || instance.LifecycleGeneration != state.generation {
		return nil, fmt.Errorf("remote execution instance %q does not match provider state", instance.ID)
	}
	return state, nil
}

func (b *Backend) stateByID(id string) (*sandboxState, error) {
	if b == nil {
		return nil, errors.New("remote execution backend is nil")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, errors.New("remote execution instance id is required")
	}
	b.mu.Lock()
	state := b.sandboxes[id]
	b.mu.Unlock()
	if state == nil {
		return nil, fmt.Errorf("remote execution instance %q is not attached", id)
	}
	return state, nil
}

func (s *sandboxState) instance() *execbackend.Instance {
	return &execbackend.Instance{
		ID:                  s.sandbox.ID,
		JobID:               s.jobID,
		LifecycleGeneration: s.generation,
		Workspace:           workspacePath,
		BaseHEAD:            s.hostBase,
	}
}

func runEnvd(ctx context.Context, envd *e2b.Envd, request e2b.StartRequest) (execbackend.ExecResult, error) {
	stream, err := envd.Start(ctx, request)
	if err != nil {
		return execbackend.ExecResult{}, err
	}
	return stream.Wait()
}

func uploadWorkspaceArchive(ctx context.Context, envd *e2b.Envd, stage string, inputPatch []byte) error {
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() {
		archiveErr := writeWorkspaceArchive(writer, stage, inputPatch)
		_ = writer.CloseWithError(archiveErr)
		done <- archiveErr
	}()
	uploadErr := envd.Upload(ctx, syncArchivePath, reader)
	_ = reader.CloseWithError(uploadErr)
	archiveErr := <-done
	return errors.Join(uploadErr, archiveErr)
}

func writeWorkspaceArchive(destination io.Writer, root string, inputPatch []byte) error {
	gzipWriter := gzip.NewWriter(destination)
	tarWriter := tar.NewWriter(gzipWriter)
	if err := tarWriter.WriteHeader(&tar.Header{Name: "workspace/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		return errors.Join(err, tarWriter.Close(), gzipWriter.Close())
	}
	walkErr := filepath.Walk(root, func(filePath string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, filePath)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		if relative == ".git" {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			link, err = os.Readlink(filePath)
			if err != nil {
				return err
			}
		}
		header, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		header.Name = path.Join("workspace", filepath.ToSlash(relative))
		header.Uid, header.Gid = 0, 0
		header.Uname, header.Gname = "", ""
		if err := tarWriter.WriteHeader(header); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		file, err := os.Open(filePath)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(tarWriter, file)
		closeErr := file.Close()
		return errors.Join(copyErr, closeErr)
	})
	if walkErr == nil {
		walkErr = tarWriter.WriteHeader(&tar.Header{Name: "changes.patch", Mode: 0o600, Size: int64(len(inputPatch))})
		if walkErr == nil && len(inputPatch) > 0 {
			_, walkErr = tarWriter.Write(inputPatch)
		}
	}
	return errors.Join(walkErr, tarWriter.Close(), gzipWriter.Close())
}

func hostGitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	output, err := hostGitBytes(ctx, dir, args...)
	return string(output), err
}

func hostGitBytes(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

func hostBootID() string {
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func processPIDNamespace() string {
	identity, err := os.Readlink("/proc/self/ns/pid")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(identity)
}

func processStartTime(pid int) string {
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

func processOwnerAlive(pid int, bootID, startTime string) bool {
	if pid <= 0 {
		return false
	}
	currentBoot := hostBootID()
	if strings.TrimSpace(bootID) != "" && currentBoot != "" && strings.TrimSpace(bootID) != currentBoot {
		return false
	}
	if strings.TrimSpace(startTime) != "" {
		return processStartTime(pid) == strings.TrimSpace(startTime)
	}
	return syscall.Kill(pid, 0) == nil
}

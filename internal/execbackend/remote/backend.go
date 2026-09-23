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
	"time"

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
	// ShouldRenewTTL is asked before each keepalive renewal for a job. Returning
	// false stops buying provider time for work nobody wants any more - a killed
	// tree's in-flight job, or one that has already left the running state. Nil
	// renews on the deadline alone.
	ShouldRenewTTL func(jobID string) bool
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

	shouldRenewTTL func(jobID string) bool

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
	diffBase     string
	remoteBase   string

	// stopKeepalive ends the TTL refresh loop. Nil when no refresh is running,
	// which is the case whenever the requested TTL fits inside the provider's
	// ceiling in one go.
	stopKeepalive context.CancelFunc
}

var _ execbackend.ExecutionBackend = (*Backend)(nil)
var _ execbackend.InventoryReaper = (*Backend)(nil)
var _ execbackend.ObservedInstanceDestroyer = (*Backend)(nil)
var _ execbackend.CredentialMaterialInstaller = (*Backend)(nil)
var _ execbackend.InstanceFileInstaller = (*Backend)(nil)

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
		client:         client,
		templateID:     templateID,
		envd:           options.Envd,
		shouldRenewTTL: options.ShouldRenewTTL,
		bootID:         hostBootID(),
		pidNamespace:   processPIDNamespace(),
		ownerPID:       pid,
		ownerStart:     processStartTime(pid),
		ownerAlive:     processOwnerAlive,
		sandboxes:      make(map[string]*sandboxState),
	}, nil
}

func (b *Backend) Name() execbackend.Backend { return execbackend.Remote }

// ProviderMaxTTL is the largest sandbox timeout E2B accepts on create. Asking
// for more is refused outright: `POST /sandboxes` returns HTTP 400 with
// {"code":400,"message":"Timeout cannot be greater than 1 hours"}.
//
// THIS MADE REMOTE REVIEWS IMPOSSIBLE, not merely awkward. The review class
// floor (#2191) is three hours, the lifecycle adds a teardown grace, and the sum
// went to the provider verbatim - so every review was rejected before a sandbox
// existed. Measured on 658ab6ef with the job-type allowlist lifted.
const ProviderMaxTTL = time.Hour

// ttlRefreshLead is how long before expiry the keepalive renews. E2B's
// SetTimeout replaces the TTL measured from the time of the request, so renewing
// early simply moves the deadline forward; renewing late loses the sandbox.
const ttlRefreshLead = 10 * time.Minute

// ttlRetryInitialBackoff and ttlRetryMaxBackoff bound the renewal retries inside
// that lead. Ten minutes of exponential backoff from 5s gives roughly a dozen
// attempts, which covers a transient provider blip without hammering it.
const (
	ttlRetryInitialBackoff = 5 * time.Second
	ttlRetryMaxBackoff     = 2 * time.Minute
)

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
	// Clamp to what the provider will accept, and remember whether the job asked
	// for longer. A clamp ALONE would be a silent downgrade: a three-hour review
	// would die at one hour with no explanation, which is worse than the refusal
	// it replaces. The keepalive below is what makes the clamp honest.
	createTTL := scope.TTL
	needsRefresh := false
	if createTTL > ProviderMaxTTL {
		createTTL = ProviderMaxTTL
		needsRefresh = true
	}
	sandbox, credential, err := b.client.Create(ctx, b.templateID, createTTL, e2b.CreateOptions{Metadata: metadata})
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
	if needsRefresh {
		var liveness func() bool
		if b.shouldRenewTTL != nil {
			liveness = func() bool { return b.shouldRenewTTL(jobID) }
		}
		b.startTTLKeepaliveWithLiveness(state, scope.TTL, liveness)
	}
	return state.instance(), nil
}

// startTTLKeepalive renews a sandbox's provider TTL for as long as the job's
// requested lifetime exceeds the provider's per-request ceiling.
//
// It is DELIBERATELY BOUNDED by the requested TTL rather than running until the
// sandbox is destroyed. An unbounded keepalive turns any lost owner into an
// immortal billed sandbox - the failure mode #1539's reaper exists to prevent -
// so the loop stops on its own at the deadline the caller asked for, and the
// provider's own timeout then collects the sandbox even if this process dies.
//
// It runs on context.Background rather than the provision context: the caller's
// context ends when Provision returns, and a refresher tied to it would stop
// immediately. stopTTLKeepalive is the only thing that ends it early.
func (b *Backend) startTTLKeepalive(state *sandboxState, requested time.Duration) {
	b.startTTLKeepaliveWithLiveness(state, requested, nil)
}

// startTTLKeepaliveWithLiveness is startTTLKeepalive with an optional predicate
// asked before each renewal.
//
// THE PREDICATE EXISTS BECAUSE THE DEADLINE IS NOT THE ONLY REASON TO STOP.
// `job kill` is graceful by contract - it stops new delegations and lets
// in-flight work finish - so a killed tree's running remote job keeps its
// sandbox. That was harmless while the sandbox lapsed at the provider's one-hour
// ceiling. Once this keepalive renews to the full requested TTL, a review-class
// job holds a billed instance for three hours after the operator killed its
// tree, which round 2 review of #2226 identified as tripling the blast radius of
// #1539.
//
// Renewing is the only thing that stops. The job still runs to completion, which
// is what kill promises; it simply stops buying more provider time.
//
// A nil predicate renews on the deadline alone, which is the correct behaviour
// for any caller that cannot observe job state.
func (b *Backend) startTTLKeepaliveWithLiveness(state *sandboxState, requested time.Duration, shouldRenew func() bool) {
	if state == nil || requested <= ProviderMaxTTL {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	state.mu.Lock()
	state.stopKeepalive = cancel
	sandboxID := state.sandbox.ID
	state.mu.Unlock()

	deadline := time.Now().Add(requested)
	interval := ProviderMaxTTL - ttlRefreshLead
	if interval <= 0 {
		interval = ProviderMaxTTL / 2
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !b.keepaliveTick(ctx, sandboxID, deadline, shouldRenew) {
					return
				}
			}
		}
	}()
}

// keepaliveTick is one iteration of the renewal loop, extracted so it can be
// tested at the depth it actually runs. The loop's own interval is 50 minutes,
// so a test driving the goroutine would either sleep for an hour or prove
// nothing about this logic.
//
// It reports whether the loop should continue.
func (b *Backend) keepaliveTick(ctx context.Context, sandboxID string, deadline time.Time, shouldRenew func() bool) bool {
	return b.keepaliveTickUsing(ctx, sandboxID, deadline, shouldRenew, func(ctx context.Context, id string, ttl time.Duration) error {
		_, err := b.client.SetTimeout(ctx, id, ttl)
		return err
	})
}

// keepaliveTickUsing is keepaliveTick with the provider call injected, so a test
// drives the same branching production takes.
func (b *Backend) keepaliveTickUsing(ctx context.Context, sandboxID string, deadline time.Time, shouldRenew func() bool, setTimeout func(context.Context, string, time.Duration) error) bool {
	if time.Until(deadline) <= 0 {
		return false
	}
	// Ask BEFORE renewing, not after: the point is to avoid buying provider time
	// for work nobody wants any more.
	if shouldRenew != nil && !shouldRenew() {
		return false
	}
	b.renewWithinLeadUsing(ctx, sandboxID, deadline, setTimeout)
	return true
}

// renewWithinLead renews the provider TTL, RETRYING UNTIL THE LEAD IS SPENT.
//
// THE RETRY IS THE WHOLE VALUE OF THE LEAD. The first version renewed once per
// tick and ignored the error, with a comment claiming the next tick would try
// again "while time remains" - which was false. Ticks are 50 minutes apart and
// the sandbox expires 60 minutes after the last successful renewal, so a single
// transient failure left the next attempt 40 minutes too late: the job died at
// ~1h regardless of its requested TTL. That is precisely the silent downgrade
// the keepalive exists to prevent, reintroduced by the keepalive itself. Round 1
// review of #2226 found it.
//
// Renewal stays BEST-EFFORT: exhausting the lead never kills the job early or
// panics the goroutine, it just leaves the sandbox on the TTL it already has.
func (b *Backend) renewWithinLead(ctx context.Context, sandboxID string, deadline time.Time) {
	b.renewWithinLeadUsing(ctx, sandboxID, deadline, func(ctx context.Context, id string, ttl time.Duration) error {
		_, err := b.client.SetTimeout(ctx, id, ttl)
		return err
	})
}

// renewWithinLeadUsing is renewWithinLead with the provider call injected. The
// seam is a PARAMETER rather than a field on Backend so production has exactly
// one path and no test-only state can be left set by accident.
func (b *Backend) renewWithinLeadUsing(ctx context.Context, sandboxID string, deadline time.Time, setTimeout func(context.Context, string, time.Duration) error) {
	leadExpiry := time.Now().Add(ttlRefreshLead)
	backoff := ttlRetryInitialBackoff
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return
		}
		extend := remaining
		if extend > ProviderMaxTTL {
			extend = ProviderMaxTTL
		}
		if err := setTimeout(ctx, sandboxID, extend); err == nil {
			return
		}
		// Stop once a further attempt could not land before the CURRENT provider
		// TTL lapses. Retrying past that point cannot save the sandbox and only
		// burns provider calls.
		if time.Now().Add(backoff).After(leadExpiry) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > ttlRetryMaxBackoff {
			backoff = ttlRetryMaxBackoff
		}
	}
}

// stopTTLKeepalive ends any refresh loop for this sandbox. Safe to call for a
// sandbox that never had one.
func (b *Backend) stopTTLKeepalive(state *sandboxState) {
	if state == nil {
		return
	}
	state.mu.Lock()
	stop := state.stopKeepalive
	state.stopKeepalive = nil
	state.mu.Unlock()
	if stop != nil {
		stop()
	}
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
	diffBase := strings.TrimSpace(materials.DiffBaseHEAD)
	if diffBase == "" {
		diffBase = hostBase
	}
	inputChanges, err := execbackend.BuildChangeSetFromBase(ctx, hostWorktree, hostBase, diffBase)
	if err != nil {
		return fmt.Errorf("capture remote execution sync input: %w", err)
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	if err := execbackend.StageAt(ctx, hostWorktree, diffBase, func(stage string) error {
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
	if diffBase != hostBase {
		if _, err := runEnvd(ctx, state.envd, e2b.StartRequest{
			Name: "git",
			Args: []string{"add", "-N", "."},
			Dir:  workspacePath,
		}); err != nil {
			return fmt.Errorf("expose projected review additions: %w", err)
		}
	}
	state.hostWorktree = hostWorktree
	state.hostBase = hostBase
	state.diffBase = diffBase
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

// InstallInstanceFile streams one runtime-owned file into the sandbox.
// Material is kept outside the repository, protected by the requested mode,
// and destroyed with the instance.
func (b *Backend) InstallInstanceFile(ctx context.Context, instance *execbackend.Instance, destination string, reader io.Reader, mode os.FileMode) (string, error) {
	state, err := b.stateFor(instance)
	if err != nil {
		return "", err
	}
	destination = path.Clean(strings.TrimSpace(destination))
	if !strings.HasPrefix(destination, execbackend.RuntimeMaterialDir+"/") {
		return "", fmt.Errorf("remote runtime file %q must be below %s", destination, execbackend.RuntimeMaterialDir)
	}
	if reader == nil {
		return "", errors.New("remote runtime file reader is required")
	}
	if mode != 0o600 && mode != 0o700 {
		return "", fmt.Errorf("remote runtime file %q has unsupported mode %04o", destination, mode)
	}
	if _, err := runEnvd(ctx, state.envd, e2b.StartRequest{
		Name: "mkdir", Args: []string{"-p", path.Dir(destination)}, Dir: "/home/user", MaxOutputBytes: 256,
	}); err != nil {
		return "", fmt.Errorf("create remote runtime directory: %w", err)
	}
	if err := state.envd.Upload(ctx, destination, reader); err != nil {
		return "", fmt.Errorf("upload remote runtime file %s: %w", path.Base(destination), err)
	}
	if _, err := runEnvd(ctx, state.envd, e2b.StartRequest{
		Name: "chmod", Args: []string{fmt.Sprintf("%04o", mode.Perm()), destination}, Dir: "/home/user", MaxOutputBytes: 256,
	}); err != nil {
		return "", fmt.Errorf("protect remote runtime file %s: %w", path.Base(destination), err)
	}
	return destination, nil
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
	if state.diffBase != "" && state.diffBase != state.hostBase {
		return execbackend.ChangeSet{}, errors.New("collect is unavailable for a projected review diff workspace")
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
	tracked_state, tracked := b.sandboxes[instance.ID]
	b.mu.Unlock()
	if !tracked {
		return nil
	}
	// Stop renewing BEFORE deleting, so a tick cannot race the delete and hand a
	// fresh hour to a sandbox that is being torn down.
	b.stopTTLKeepalive(tracked_state)
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

// ReapInventory observes account-wide sandboxes without deleting any. A
// reapable observation still requires an exact local ledger match before
// DestroyObserved may delete it. E2B cannot prove all-state completeness.
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
		// sandbox exists, but absence from this inventory is not authoritative.
		InventoryObserved: true,
		Inventory:         make([]execbackend.ProviderInstance, 0, len(sandboxes)),
	}
	for _, sandbox := range sandboxes {
		metadata := sandbox.Metadata
		attempt, _ := strconv.Atoi(strings.TrimSpace(metadata[metadataAttempt]))
		generation := int64(-1)
		if parsed, parseErr := strconv.ParseInt(strings.TrimSpace(metadata[metadataLifecycleGeneration]), 10, 64); parseErr == nil && parsed >= 0 {
			generation = parsed
		}
		bootID := strings.TrimSpace(metadata[metadataBootID])
		instance := execbackend.ProviderInstance{
			ID:                  sandbox.ID,
			JobID:               strings.TrimSpace(metadata[metadataJobID]),
			Attempt:             attempt,
			LifecycleGeneration: generation,
			DaemonFencingToken:  strings.TrimSpace(metadata[metadataDaemonFencingToken]),
			BootID:              bootID,
		}
		switch {
		case bootID == "":
			// Legacy or unowned metadata is never deletion authority.
		case bootID != b.bootID:
			// A prior host boot cannot still own a running process. The ledger
			// decides whether this is OUR prior boot, not another host's.
			instance.Reapable = true
		case matchingNonEmptyIdentity(metadata[metadataOwnerPIDNamespace], b.pidNamespace):
			pid, parseErr := strconv.Atoi(strings.TrimSpace(metadata[metadataOwnerPID]))
			startTime := strings.TrimSpace(metadata[metadataOwnerStartTime])
			instance.Reapable = parseErr == nil && pid > 0 && startTime != "" && !b.ownerAlive(pid, bootID, startTime)
		}
		report.Inventory = append(report.Inventory, instance)
	}
	return report, nil
}

// DestroyObserved is called only after the ledger matches an observed
// instance's full identity to an active local attempt. An ambiguous provider
// response cannot release that attempt's cost reservation.
func (b *Backend) DestroyObserved(ctx context.Context, instance execbackend.ProviderInstance) error {
	if b == nil || !instance.Reapable || strings.TrimSpace(instance.ID) == "" {
		return errors.New("remote execution observed instance is not eligible for destruction")
	}
	state, err := b.client.Delete(ctx, instance.ID)
	if err != nil {
		return fmt.Errorf("destroy observed remote execution sandbox %q: %w", instance.ID, err)
	}
	if state != e2b.Gone {
		return fmt.Errorf("destroy observed remote execution sandbox %q was inconclusive: %s", instance.ID, state)
	}
	b.mu.Lock()
	delete(b.sandboxes, instance.ID)
	b.mu.Unlock()
	return nil
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

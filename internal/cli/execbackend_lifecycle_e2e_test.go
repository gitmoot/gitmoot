package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/execbackend"
	gitutil "github.com/gitmoot/gitmoot/internal/git"
	"github.com/gitmoot/gitmoot/internal/permissionpolicy"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/subprocess"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

type localProvisionBackend struct {
	provisionCalls int
	syncInCalls    int
	scope          execbackend.JobScope
	materials      execbackend.Materials
	instance       *execbackend.Instance
}

func (*localProvisionBackend) Name() execbackend.Backend { return execbackend.Local }

func (b *localProvisionBackend) Provision(_ context.Context, scope execbackend.JobScope) (*execbackend.Instance, error) {
	b.provisionCalls++
	b.scope = scope
	if b.instance == nil {
		b.instance = &execbackend.Instance{ID: "local-provision", JobID: scope.JobID, LifecycleGeneration: scope.LifecycleGeneration, Workspace: "/local-provision"}
	}
	return b.instance, nil
}

func (*localProvisionBackend) Attach(context.Context, string) (*execbackend.Instance, error) {
	return nil, nil
}

func (b *localProvisionBackend) SyncIn(_ context.Context, _ *execbackend.Instance, materials execbackend.Materials) error {
	b.syncInCalls++
	b.materials = materials
	return nil
}

func (*localProvisionBackend) Exec(context.Context, *execbackend.Instance, execbackend.Command) (execbackend.Stream, error) {
	return nil, nil
}

func (*localProvisionBackend) Collect(context.Context, *execbackend.Instance) (execbackend.ChangeSet, error) {
	return execbackend.ChangeSet{}, nil
}

func (*localProvisionBackend) Cancel(context.Context, *execbackend.Instance) error { return nil }

func (*localProvisionBackend) Destroy(context.Context, *execbackend.Instance) error { return nil }

func TestProvisionExecutionBackendLocalBehaviorUnchanged(t *testing.T) {
	backend := &localProvisionBackend{}
	worker := jobWorker{ExecutionBackendFactory: func(got execbackend.Backend) (execbackend.ExecutionBackend, error) {
		if got != execbackend.Local {
			t.Fatalf("factory backend = %q, want %q", got, execbackend.Local)
		}
		return backend, nil
	}}
	job := db.Job{ID: "local-credential-gate", LifecycleGeneration: 4}

	const ttl = 9 * time.Minute
	lifecycle, instance, err := worker.provisionExecutionBackend(context.Background(), execbackend.Local, runtime.ShellRuntime, job, ttl, "/checkout")
	if err != nil {
		t.Fatalf("provisionExecutionBackend(local): %v", err)
	}
	if lifecycle != backend || instance != backend.instance {
		t.Fatalf("local lifecycle = %T, instance = %+v; want injected backend and its instance", lifecycle, instance)
	}
	if backend.provisionCalls != 1 || backend.syncInCalls != 1 {
		t.Fatalf("local Provision calls = %d, SyncIn calls = %d; want 1, 1", backend.provisionCalls, backend.syncInCalls)
	}
	if backend.scope.JobID != job.ID || backend.scope.LifecycleGeneration != job.LifecycleGeneration || backend.scope.TTL != ttl {
		t.Fatalf("local provision scope = %+v, want job id %q generation %d ttl %s", backend.scope, job.ID, job.LifecycleGeneration, ttl)
	}
	if backend.materials.SourceWorktree != "/checkout" {
		t.Fatalf("local SyncIn source = %q, want /checkout", backend.materials.SourceWorktree)
	}
}

func TestExecutionChangeSetCollectorRequiresLiveOwnedInstance(t *testing.T) {
	backend, err := execbackend.NewLocalBackend(filepath.Join(t.TempDir(), "instances"), nil)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := backend.Provision(context.Background(), execbackend.JobScope{JobID: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Destroy(context.Background(), instance) })

	for name, lifecycleAndInstance := range map[string]struct {
		lifecycle execbackend.ExecutionBackend
		instance  *execbackend.Instance
	}{
		"neither":       {},
		"backend only":  {lifecycle: backend},
		"instance only": {instance: instance},
	} {
		if collector := executionChangeSetCollector(lifecycleAndInstance.lifecycle, lifecycleAndInstance.instance, execbackend.Local, "owner"); collector != nil {
			t.Fatalf("%s: backend-less job received a live ChangeSet collector", name)
		}
	}
	collector := executionChangeSetCollector(backend, instance, execbackend.Local, "owner")
	if _, err := collector(context.Background(), execbackend.Backend("e2b"), "owner"); err == nil || !strings.Contains(err.Error(), "does not match live instance backend") {
		t.Fatalf("backend ownership error = %v", err)
	}
	if _, err := collector(context.Background(), execbackend.Local, "intruder"); err == nil || !strings.Contains(err.Error(), "does not own live instance") {
		t.Fatalf("job ownership error = %v", err)
	}
}

func TestLocalBackendInstanceRunnerPreservesCuratedEnvironment(t *testing.T) {
	checkout := createDaemonWorkerGitCheckout(t, "curated-backend")
	backend, err := execbackend.NewLocalBackend(filepath.Join(t.TempDir(), "instances"), nil)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := backend.Provision(context.Background(), execbackend.JobScope{JobID: "curated"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Destroy(context.Background(), instance) })
	if err := backend.SyncIn(context.Background(), instance, execbackend.Materials{SourceWorktree: checkout}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITMOOT_BACKEND_MUST_NOT_LEAK", "host-secret")
	runner := graftRuntimeBaseRunner(
		execbackend.InstanceRunner{Backend: backend, Instance: instance},
		subprocess.CuratedGroupRunner{BaseEnv: []string{"PATH=" + os.Getenv("PATH"), "CURATED=yes"}},
	)
	result, err := runner.Run(context.Background(), instance.Workspace, "sh", "-c", "env")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result.Stdout, "GITMOOT_BACKEND_MUST_NOT_LEAK=") {
		t.Fatalf("backend inherited an environment entry omitted by curation:\n%s", result.Stdout)
	}
	if !strings.Contains(result.Stdout, "CURATED=yes") {
		t.Fatalf("backend dropped curated environment:\n%s", result.Stdout)
	}
}

func TestDefaultExecutionBackendUsesConfiguredIdentity(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("no privilege to apply a configured non-root identity")
	}
	uid, gid := configuredLocalTestIdentity(t)
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatal(err)
	}
	localParent, err := os.MkdirTemp(os.TempDir(), "gitmoot-configured-local-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(localParent, 0o755); err != nil {
		t.Fatal(err)
	}
	localRoot := filepath.Join(localParent, "instances")
	t.Cleanup(func() {
		_ = os.Remove(localRoot)
		_ = os.Remove(localParent)
	})
	content := fmt.Sprintf("[remote_exec]\nbackend = \"local\"\nlocal_uid = %d\nlocal_gid = %d\nlocal_root = %q\n", uid, gid, localRoot)
	if err := os.WriteFile(paths.ConfigFile, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	worker := jobWorker{ConfigHome: home, ConfigHomeExplicit: true}
	lifecycle, err := worker.defaultExecutionBackend(execbackend.Local)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := lifecycle.Provision(context.Background(), execbackend.JobScope{JobID: "configured-identity"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lifecycle.Destroy(context.Background(), instance) })
	checkout := createDaemonWorkerGitCheckout(t, "configured-identity")
	if err := lifecycle.SyncIn(context.Background(), instance, execbackend.Materials{SourceWorktree: checkout}); err != nil {
		t.Fatal(err)
	}
	stream, err := lifecycle.Exec(context.Background(), instance, execbackend.Command{Dir: instance.Workspace, Name: "id", Args: []string{"-u"}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := stream.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimSpace(result.Stdout), strconv.FormatUint(uint64(uid), 10); got != want {
		t.Fatalf("configured command-reported euid = %q, want %s", got, want)
	}
}

func configuredLocalTestIdentity(t *testing.T) (uint32, uint32) {
	t.Helper()
	data, err := os.ReadFile("/etc/passwd")
	if err != nil {
		t.Skipf("no unprivileged identity configured: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) < 4 {
			continue
		}
		uid, uidErr := strconv.ParseUint(fields[2], 10, 32)
		gid, gidErr := strconv.ParseUint(fields[3], 10, 32)
		if uidErr == nil && gidErr == nil && uid > 0 && gid > 0 && uid < uint64(^uint32(0)) && gid < uint64(^uint32(0)) {
			return uint32(uid), uint32(gid)
		}
	}
	t.Skip("no unprivileged identity configured in /etc/passwd")
	return 0, 0
}

type byteIdentityHostFinalizer struct {
	checkout    string
	expectedSHA string
	called      *bool
}

func (f byteIdentityHostFinalizer) FinalizeImplementation(ctx context.Context, _ db.Job, payload workflow.JobPayload) (workflow.JobPayload, error) {
	*f.called = true
	expected, err := os.ReadFile(f.expectedSHA)
	if err != nil {
		return payload, err
	}
	content, err := os.ReadFile(filepath.Join(f.checkout, "artifact.bin"))
	if err != nil {
		return payload, err
	}
	digest := sha256.Sum256(content)
	if got, want := hex.EncodeToString(digest[:]), strings.TrimSpace(string(expected)); got != want {
		return payload, fmt.Errorf("host import hash %s is not backend hash %s", got, want)
	}
	git := gitutil.NewHostClient(f.checkout)
	if err := git.CommitAll(ctx, "test: finalize backend bytes"); err != nil {
		return payload, err
	}
	payload.HeadSHA, err = git.HeadSHA(ctx)
	return payload, err
}

// TestLocalExecutionBackendShellImplementRoundTripE2E is the P2b no-LLM
// acceptance path: a real shell implement job runs in a distinct local backend
// workspace, Mailbox imports through the production collector before result
// observation, and a host finalizer commits bytes whose SHA-256 exactly matches
// the backend-produced file.
func TestLocalExecutionBackendShellImplementRoundTripE2E(t *testing.T) {
	ctx := context.Background()
	home, _, store := heartbeatLoopE2EHome(t)
	const branch = "gitmoot-delegation-parent-local-roundtrip"
	checkout := createDaemonWorkerGitCheckout(t, branch)
	baseHEAD := strings.TrimSpace(runGitOutput(t, checkout, "rev-parse", "HEAD"))
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	expectedSHA := filepath.Join(t.TempDir(), "backend.sha256")
	backendPWD := filepath.Join(t.TempDir(), "backend.pwd")
	script := `printf 'backend\000produced\377bytes\n' > artifact.bin
sha256sum artifact.bin | cut -d ' ' -f 1 > "$EXPECTED_SHA"
pwd > "$BACKEND_PWD"
printf '%s' '{"gitmoot_result":{"decision":"implemented","summary":"backend bytes produced","findings":[],"changes_made":["created artifact.bin"],"tests_run":[],"needs":[],"delegations":[]}}'`
	seedDaemonWorkerAgent(t, store, "local-coder", runtime.ShellRuntime, script, []string{"ask", "implement"}, "owner/repo")
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-local-backend", RepoFullName: "owner/repo", GoalID: "goal-local-backend",
		Title: "Local backend", State: string(workflow.TaskImplementing), Branch: branch, WorktreePath: checkout,
	}); err != nil {
		t.Fatal(err)
	}
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "owner/repo", Branch: branch, Owner: "local-coder"}); err != nil || !acquired {
		t.Fatalf("AcquireLock acquired=%v err=%v", acquired, err)
	}
	mailbox := workflow.NewMailbox(store, workflow.PayloadDeliveryWorktreeResolver)
	parent, err := mailbox.Enqueue(ctx, workflow.JobRequest{
		ID: "local-backend-parent", Agent: "local-coder", Action: "ask", Repo: "owner/repo", Instructions: "delegate the byte round-trip",
	})
	if err != nil {
		t.Fatal(err)
	}
	job, err := mailbox.Enqueue(ctx, workflow.JobRequest{
		ID: "local-backend-shell-implement", Agent: "local-coder", Action: "implement",
		Repo: "owner/repo", Branch: branch, GoalID: "goal-local-backend", TaskID: "task-local-backend",
		TaskTitle: "Local backend", WorktreePath: checkout, HeadSHA: baseHEAD,
		ParentJobID: parent.ID, DelegationID: "local-roundtrip", DelegationDepth: 1, DelegatedBy: "local-coder", RootJobID: parent.ID,
		ShellEnv: []string{"EXPECTED_SHA=" + expectedSHA, "BACKEND_PWD=" + backendPWD},
	})
	if err != nil {
		t.Fatal(err)
	}
	finalizerCalled := false
	worker := executionBackendJobWorker(store, os.Stderr, home)
	worker.PermissionPolicyEffectGit = func(string) permissionpolicy.EffectGit {
		return &permissionPolicyEffectGitFake{remote: map[string]struct{}{branch: {}}}
	}
	worker.WorkflowFactory = func(string) workflow.Engine {
		return workflow.Engine{
			Store:                   store,
			ResolveDeliveryWorktree: workflow.PayloadDeliveryWorktreeResolver,
			ImplementationFinalizer: byteIdentityHostFinalizer{checkout: checkout, expectedSHA: expectedSHA, called: &finalizerCalled},
		}
	}
	if err := worker.run(ctx, job); err != nil {
		t.Fatalf("worker.run: %v", err)
	}
	if !finalizerCalled {
		t.Fatal("host finalizer did not run")
	}
	completed, err := store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.State != string(workflow.JobSucceeded) {
		t.Fatalf("job state = %q, want succeeded", completed.State)
	}
	payload, err := workflow.ParseJobPayload(completed.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if payload.ResultObservation == nil || fmt.Sprint(payload.ResultObservation.TouchedFiles) != "[artifact.bin]" {
		t.Fatalf("result observation = %+v, want imported artifact.bin", payload.ResultObservation)
	}
	pwdBytes, err := os.ReadFile(backendPWD)
	if err != nil {
		t.Fatal(err)
	}
	backendWorkspace := strings.TrimSpace(string(pwdBytes))
	if backendWorkspace == checkout {
		t.Fatalf("shell ran in host checkout %q, want distinct backend workspace", checkout)
	}
	if _, err := os.Stat(backendWorkspace); !os.IsNotExist(err) {
		t.Fatalf("backend workspace still exists after job teardown: %v", err)
	}
	want := []byte{'b', 'a', 'c', 'k', 'e', 'n', 'd', 0, 'p', 'r', 'o', 'd', 'u', 'c', 'e', 'd', 0xff, 'b', 'y', 't', 'e', 's', '\n'}
	show := exec.Command("git", "-C", checkout, "show", "HEAD:artifact.bin")
	got, err := show.Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("committed artifact is not byte-identical: got %x want %x", got, want)
	}
	if status := strings.TrimSpace(runGitOutput(t, checkout, "status", "--porcelain")); status != "" {
		t.Fatalf("host finalizer left a dirty tree: %q", status)
	}
}

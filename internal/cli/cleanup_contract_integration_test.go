package cli

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	gitutil "github.com/gitmoot/gitmoot/internal/git"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

type cleanupCLIWorld struct {
	home     string
	paths    config.Paths
	store    *db.Store
	checkout string
	path     string
	job      db.Job
	payload  workflow.JobPayload
}

func newCleanupCLIWorld(t *testing.T, state, action string) cleanupCLIWorld {
	t.Helper()
	world := cleanupCLIWorld{home: t.TempDir(), checkout: t.TempDir()}
	world.paths = config.PathsForHome(world.home)
	if err := config.Initialize(world.paths); err != nil {
		t.Fatal(err)
	}
	// A FIXTURE cap for the budget-gated consumer path (#2360 v2); never a live default.
	appendCleanupFixtureBudget(t, world.paths.ConfigFile, 1<<30)
	var err error
	world.store, err = db.Open(world.paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = world.store.Close() })
	world.git(t, "init", "--initial-branch=main")
	world.git(t, "config", "user.email", "test@example.com")
	world.git(t, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(world.checkout, "result.txt"), []byte("durable producer input\n"), 0600); err != nil {
		t.Fatal(err)
	}
	world.git(t, "add", "result.txt")
	world.git(t, "commit", "-m", "producer input")
	const jobID = "cleanup-owned-review"
	world.path, err = workflow.DelegationWorktreePath(world.paths.Home, "owner/repo", jobID, "readonly-seat", 0)
	if err != nil {
		t.Fatal(err)
	}
	world.git(t, "worktree", "add", "--detach", world.path, "HEAD")
	world.payload = workflow.JobPayload{
		Repo: "owner/repo", Branch: "main", PullRequest: 42, TaskID: "review-pr-42",
		HeadSHA:      strings.TrimSpace(world.git(t, "rev-parse", "HEAD")),
		WorktreePath: world.path, ReadOnlyWorktree: true, ReadOnlySeat: true,
		Result: &workflow.AgentResult{Decision: "approved", Summary: "terminal review evidence"},
	}
	encoded, err := json.Marshal(world.payload)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := world.store.UpsertRepo(ctx, db.Repo{Owner: "owner", Name: "repo", DefaultBranch: "main", CheckoutPath: world.checkout}); err != nil {
		t.Fatal(err)
	}
	if err := world.store.CreateJob(ctx, db.Job{ID: jobID, Agent: "reviewer", Type: action, State: state, Payload: string(encoded)}); err != nil {
		t.Fatal(err)
	}
	world.job, err = world.store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	return world
}

func (world cleanupCLIWorld) git(t *testing.T, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = world.checkout
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return string(output)
}

func (world cleanupCLIWorld) cleanup(t *testing.T, args ...string) map[string]any {
	t.Helper()
	return world.cleanupStatus(t, 0, args...)
}

func (world cleanupCLIWorld) cleanupStatus(t *testing.T, want int, args ...string) map[string]any {
	t.Helper()
	var stdout, stderr bytes.Buffer
	args = append([]string{args[0], "--home", world.home, "--json"}, args[1:]...)
	if code := runJobCleanup(args, &stdout, &stderr); code != want {
		t.Fatalf("cleanup %v exit=%d stdout=%s stderr=%s", args, code, stdout.String(), stderr.String())
	}
	var result map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestCleanupCLIHistoricalReplayPreservesRecreatedGeneration(t *testing.T) {
	world := newCleanupCLIWorld(t, "succeeded", "review")
	ctx := context.Background()
	engine := daemonWorkflowEngine(world.store, nil, world.checkout, world.paths.Home)
	// These fixtures own all workspace users; unrelated host process permissions are not part of this scenario.
	engine.WorktreeLiveness = func(string) (bool, bool) { return false, true }
	removed, err := engine.ReclaimTerminalDelegationWorktreeOutcome(ctx, world.job.ID)
	if err != nil || !removed {
		t.Fatalf("initial cleanup removed=%t err=%v", removed, err)
	}
	if _, err := os.Stat(world.path); !os.IsNotExist(err) {
		t.Fatalf("removed workspace stat: %v", err)
	}
	inspection := world.cleanup(t, "inspect", world.job.ID)
	originalReceipt := inspection["receipt"].(map[string]any)
	resource := originalReceipt["resource_id"].(string)
	token := originalReceipt["eligibility_token"].(string)
	if err := world.store.UpdateJobState(ctx, world.job.ID, "failed"); err != nil {
		t.Fatal(err)
	}
	retried, err := workflow.RetryJob(ctx, world.store, world.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retried.LifecycleGeneration != world.job.LifecycleGeneration+1 {
		t.Fatalf("retry generation: %+v", retried)
	}
	world.git(t, "worktree", "add", "--detach", world.path, "HEAD")
	payload, err := workflow.ParseJobPayload(retried.Payload)
	if err != nil {
		t.Fatal(err)
	}
	payload.WorktreePath = world.path
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := world.store.UpdateJobPayload(ctx, world.job.ID, string(encoded)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(world.path, "new-generation.txt"), []byte("must survive old replay\n"), 0600); err != nil {
		t.Fatal(err)
	}
	replayed := world.cleanup(t, "apply", "--resource", resource, "--token", token, world.job.ID)
	if replayed["state"] != "already_removed" {
		t.Fatalf("replay result: %+v", replayed)
	}
	receipt := replayed["receipt"].(map[string]any)
	if receipt["receipt_id"] != originalReceipt["receipt_id"] || receipt["removed_at"] != originalReceipt["removed_at"] {
		t.Fatalf("historical receipt changed: original=%+v replay=%+v", originalReceipt, receipt)
	}
	content, err := os.ReadFile(filepath.Join(world.path, "new-generation.txt"))
	if err != nil || string(content) != "must survive old replay\n" {
		t.Fatalf("new workspace damaged: %q %v", content, err)
	}
	current, err := world.store.GetJob(ctx, world.job.ID)
	if err != nil || current.State != "queued" || current.LifecycleGeneration != retried.LifecycleGeneration {
		t.Fatalf("replay changed active lifecycle: %+v %v", current, err)
	}
	// Old completion events must not hide a later terminal generation from the
	// aged scheduler. Its own receipt, not the replayed token, authorizes removal.
	payload.Result = &workflow.AgentResult{Decision: "approved", Summary: "replacement generation completed"}
	encoded, err = json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := world.store.UpdateJobPayload(ctx, world.job.ID, string(encoded)); err != nil {
		t.Fatal(err)
	}
	if err := world.store.UpdateJobState(ctx, world.job.ID, "succeeded"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	backdateJobUpdatedAt(t, world.paths.Database, world.job.ID, now.Add(-73*time.Hour))
	worker := defaultJobWorker(world.store, io.Discard, world.home)
	worker.WorkflowFactory = func(string) workflow.Engine { return engine }
	if err := reclaimAgedTerminalDelegationWorktrees(ctx, worker, "", "", nil, newTickCandidates(world.store), now, 72*time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(world.path); !os.IsNotExist(err) {
		obligation, _ := world.store.GetCleanupObligation(ctx, resource)
		events, _ := world.store.ListJobEvents(ctx, world.job.ID)
		t.Fatalf("terminal replacement generation was not reclaimed: %v obligation=%+v events=%+v", err, obligation, events)
	}
	completed := world.cleanup(t, "inspect", world.job.ID)
	replacement := completed["receipt"].(map[string]any)
	if completed["lifecycle_generation"] != float64(retried.LifecycleGeneration) ||
		replacement["lifecycle_generation"] != float64(retried.LifecycleGeneration) ||
		replacement["receipt_id"] == originalReceipt["receipt_id"] || replacement["removed_at"] == "" {
		t.Fatalf("replacement cleanup did not receive its own durable receipt: %+v", completed)
	}
}

type pausedCleanupGit struct {
	gitutil.Client
	entered chan struct{}
	release chan struct{}
}

func (client pausedCleanupGit) RemoveWorktreeForce(ctx context.Context, path string) error {
	close(client.entered)
	select {
	case <-client.release:
		return client.Client.RemoveWorktreeForce(ctx, path)
	case <-ctx.Done():
		return ctx.Err()
	}
}

type cleanupRetryObservation struct {
	State      string `json:"state"`
	Path       string `json:"path"`
	Generation int64  `json:"generation"`
	Error      string `json:"error,omitempty"`
}

func TestCleanupRetryProcessHelper(t *testing.T) {
	path := os.Getenv("GITMOOT_CLEANUP_RETRY_DATABASE")
	if path == "" {
		t.Skip("subprocess helper")
	}
	store, err := db.OpenAlreadyMigrated(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	fmt.Println("READY")
	if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	job, err := workflow.RetryJob(ctx, store, "cleanup-owned-review")
	observation := cleanupRetryObservation{State: job.State, Generation: job.LifecycleGeneration}
	if err != nil {
		observation.Error = err.Error()
	} else {
		payload, parseErr := workflow.ParseJobPayload(job.Payload)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		observation.Path = payload.WorktreePath
	}
	encoded, err := json.Marshal(observation)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("RESULT %s\n", encoded)
}

func TestCleanupRetryWaitsAcrossProcessBeforeReadingWorkspace(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("uses /proc fd observation to synchronize the real cross-process race")
	}
	world := newCleanupCLIWorld(t, "failed", "review")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCleanupRetryProcessHelper$")
	command.Env = append(os.Environ(), "GITMOOT_CLEANUP_RETRY_DATABASE="+world.paths.Database)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() || scanner.Text() != "READY" {
		t.Fatalf("helper did not open isolated database: %s", scanner.Text())
	}
	done := make(chan cleanupRetryObservation, 1)
	go func() {
		observation := cleanupRetryObservation{Error: "helper returned no observation"}
		for scanner.Scan() {
			if value, ok := strings.CutPrefix(scanner.Text(), "RESULT "); ok {
				observation = cleanupRetryObservation{}
				if err := json.Unmarshal([]byte(value), &observation); err != nil {
					observation.Error = err.Error()
				}
			}
		}
		if err := command.Wait(); err != nil {
			observation.Error = err.Error() + ": " + stderr.String()
		}
		done <- observation
	}()
	manager := pausedCleanupGit{Client: gitutil.NewHostClient(world.checkout), entered: make(chan struct{}), release: make(chan struct{})}
	engine := daemonWorkflowEngine(world.store, nil, world.checkout, world.paths.Home)
	// These fixtures own all workspace users; unrelated host process permissions are not part of this scenario.
	engine.WorktreeLiveness = func(string) (bool, bool) { return false, true }
	engine.DelegationWorktrees = manager
	now := time.Now().UTC().Add(96 * time.Hour)
	engine.Now = func() time.Time { return now }
	cleaned := make(chan error, 1)
	go func() {
		removed, err := engine.ReclaimAgedTerminalDelegationWorktreeOutcome(ctx, world.job.ID, now.Add(-72*time.Hour))
		if err == nil && !removed {
			err = fmt.Errorf("eligible aged producer was not removed")
		}
		cleaned <- err
	}()
	select {
	case <-manager.entered:
	case err := <-cleaned:
		t.Fatalf("cleanup did not reach removal: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	released := false
	defer func() {
		if !released {
			close(manager.release)
		}
	}()
	if _, err := fmt.Fprintln(stdin, "retry"); err != nil {
		t.Fatal(err)
	}
	_ = stdin.Close()
	var early *cleanupRetryObservation
	waiting := false
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for !waiting {
		select {
		case result := <-done:
			early = &result
			waiting = true
		case <-ticker.C:
			entries, err := os.ReadDir(filepath.Join("/proc", strconv.Itoa(command.Process.Pid), "fd"))
			if err != nil {
				continue
			}
			for _, entry := range entries {
				target, _ := os.Readlink(filepath.Join("/proc", strconv.Itoa(command.Process.Pid), "fd", entry.Name()))
				if target == world.paths.Database+".cleanup-lifecycle.lock" {
					waiting = true
					break
				}
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	close(manager.release)
	released = true
	if err := <-cleaned; err != nil {
		t.Fatal(err)
	}
	var retried cleanupRetryObservation
	if early != nil {
		retried = *early
	} else {
		retried = <-done
	}
	if retried.Error != "" || retried.State != "queued" || retried.Path != "" || retried.Generation != world.job.LifecycleGeneration+1 {
		t.Fatalf("retry read the pre-deletion workspace or failed: %+v", retried)
	}
	if _, err := os.Stat(world.path); !os.IsNotExist(err) {
		t.Fatalf("old workspace stat: %v", err)
	}
}

type interruptedCleanupGit struct {
	gitutil.Client
	removeFirst bool
}

func (client interruptedCleanupGit) RemoveWorktreeForce(ctx context.Context, path string) error {
	if client.removeFirst {
		if err := client.Client.RemoveWorktreeForce(ctx, path); err != nil {
			return err
		}
	}
	return errors.New("injected interruption at worktree removal")
}

func TestCleanupCLIInterruptedEpisodePreservesNewLifecycle(t *testing.T) {
	for _, removedFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("removed=%t", removedFirst), func(t *testing.T) {
			world := newCleanupCLIWorld(t, "failed", "review")
			ctx := context.Background()
			engine := daemonWorkflowEngine(world.store, nil, world.checkout, world.paths.Home)
			// These fixtures own all workspace users; unrelated host process permissions are not part of this scenario.
			engine.WorktreeLiveness = func(string) (bool, bool) { return false, true }
			engine.DelegationWorktrees = interruptedCleanupGit{Client: gitutil.NewHostClient(world.checkout), removeFirst: removedFirst}
			now := time.Now().UTC().Add(96 * time.Hour)
			engine.Now = func() time.Time { return now }
			if _, err := engine.ReclaimAgedTerminalDelegationWorktreeOutcome(ctx, world.job.ID, now.Add(-72*time.Hour)); err == nil {
				t.Fatal("expected the injected removal interruption")
			}
			inspection := world.cleanup(t, "inspect", world.job.ID)
			receipt, ok := inspection["receipt"].(map[string]any)
			if !ok {
				t.Fatalf("missing durable prepared receipt: %+v", inspection)
			}
			resource, token := receipt["resource_id"].(string), receipt["eligibility_token"].(string)
			retried, err := workflow.RetryJob(ctx, world.store, world.job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if removedFirst {
				world.git(t, "worktree", "add", "--detach", world.path, "HEAD")
			}
			payload, err := workflow.ParseJobPayload(retried.Payload)
			if err != nil {
				t.Fatal(err)
			}
			payload.WorktreePath = world.path
			encoded, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			if err := world.store.UpdateJobPayload(ctx, world.job.ID, string(encoded)); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(world.path, "retry.txt"), []byte("new lifecycle owns these bytes\n"), 0600); err != nil {
				t.Fatal(err)
			}
			before, err := world.store.EnsureCleanupObligation(ctx, world.job.ID, world.path, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			replayed := world.cleanupStatus(t, 3, "apply", "--resource", resource, "--token", token, world.job.ID)
			if replayed["state"] != "superseded" {
				t.Fatalf("old episode did not settle explicitly: %+v", replayed)
			}
			settled := replayed["receipt"].(map[string]any)
			if settled["receipt_id"] != receipt["receipt_id"] || settled["evidence"] != receipt["evidence"] ||
				settled["owner_payload"] != receipt["owner_payload"] || settled["removed_at"] != "" ||
				settled["superseded_by_generation"] != float64(retried.LifecycleGeneration) {
				t.Fatalf("supersession rewrote evidence or claimed removal: %+v", settled)
			}
			again := world.cleanupStatus(t, 3, "apply", "--resource", resource, "--token", token, world.job.ID)
			if !reflect.DeepEqual(again["receipt"], settled) {
				t.Fatal("historical supersession changed on replay")
			}
			after, err := world.store.GetCleanupObligation(ctx, resource)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("replacement ledger changed: before=%+v after=%+v err=%v", before, after, err)
			}
			content, err := os.ReadFile(filepath.Join(world.path, "retry.txt"))
			if err != nil || string(content) != "new lifecycle owns these bytes\n" {
				t.Fatalf("replacement resource changed: %q %v", content, err)
			}
		})
	}
}

func TestCleanupReconcilesRemovedResourceAfterRetentionDisabled(t *testing.T) {
	world := newCleanupCLIWorld(t, "failed", "review")
	ctx := context.Background()
	engine := daemonWorkflowEngine(world.store, nil, world.checkout, world.paths.Home)
	// These fixtures own all workspace users; unrelated host process permissions are not part of this scenario.
	engine.WorktreeLiveness = func(string) (bool, bool) { return false, true }
	engine.DelegationWorktrees = interruptedCleanupGit{Client: gitutil.NewHostClient(world.checkout), removeFirst: true}
	now := time.Now().UTC().Add(96 * time.Hour)
	engine.Now = func() time.Time { return now }
	if _, err := engine.ReclaimAgedTerminalDelegationWorktreeOutcome(ctx, world.job.ID, now.Add(-72*time.Hour)); err == nil {
		t.Fatal("expected the injected interruption after physical removal")
	}
	if _, err := os.Stat(world.path); !os.IsNotExist(err) {
		t.Fatalf("interrupted resource still exists: %v", err)
	}
	if err := os.WriteFile(world.paths.ConfigFile, []byte("[workflow]\ndelegation_worktree_ttl = \"0\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	engine.DelegationWorktrees = gitutil.NewHostClient(world.checkout)
	raw, err := sql.Open("sqlite", world.paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	if _, err := raw.ExecContext(ctx, `
CREATE TRIGGER fail_reconcile_lock_release
BEFORE DELETE ON resource_locks
WHEN OLD.owner_job_id LIKE 'cleanup-reconcile:%'
BEGIN
  SELECT RAISE(ABORT, 'injected checkout release failure');
END;`); err != nil {
		t.Fatal(err)
	}
	removed, err := engine.ReclaimAgedTerminalDelegationWorktreeOutcome(ctx, world.job.ID, now.Add(-72*time.Hour))
	if err == nil || !strings.Contains(err.Error(), "injected checkout release failure") {
		t.Fatalf("accounting recovery hid checkout-lock release failure: removed=%t err=%v", removed, err)
	}
	resource := db.CleanupObligationResourceID(world.job.ID, world.path)
	obligation, err := world.store.GetCleanupObligation(ctx, resource)
	if err != nil || obligation.State != db.CleanupObligationRemoved {
		t.Fatalf("unfinished ledger: %+v %v", obligation, err)
	}
	var lock db.ResourceLock
	if err := raw.QueryRowContext(ctx, `SELECT resource_key, owner_job_id, owner_token
FROM resource_locks WHERE owner_job_id = ?`, "cleanup-reconcile:"+world.job.ID).
		Scan(&lock.ResourceKey, &lock.OwnerJobID, &lock.OwnerToken); err != nil {
		t.Fatalf("failed release did not retain the checkout lock: %v", err)
	}
	if _, err := raw.ExecContext(ctx, `DROP TRIGGER fail_reconcile_lock_release`); err != nil {
		t.Fatal(err)
	}
	if released, err := world.store.ReleaseResourceLock(ctx, lock.ResourceKey, lock.OwnerJobID, lock.OwnerToken); err != nil || !released {
		t.Fatalf("release after fault cleared: released=%t err=%v", released, err)
	}
	if removed, err := engine.ReclaimAgedTerminalDelegationWorktreeOutcome(ctx, world.job.ID, now.Add(-72*time.Hour)); err != nil || removed {
		t.Fatalf("completed episode counted twice: removed=%t err=%v", removed, err)
	}
}

func TestCleanupRevalidatesConsumerEnqueueAfterLifecycleFence(t *testing.T) {
	world := newCleanupCLIWorld(t, "succeeded", "review")
	ctx := context.Background()
	engine := daemonWorkflowEngine(world.store, nil, world.checkout, world.paths.Home)
	engine.WorktreeLiveness = func(string) (bool, bool) { return false, true }
	inspection, err := engine.InspectProducerCleanup(ctx, world.job.ID, workflow.CleanupPolicy{BudgetGate: workflow.CleanupBudgetGateEnforced})
	if err != nil || !inspection.Eligible {
		t.Fatalf("initial admission: %+v %v", inspection, err)
	}
	if err := world.store.UpsertAgent(ctx, db.Agent{Name: "consumer", Runtime: "shell", RepoScope: "owner/repo"}); err != nil {
		t.Fatal(err)
	}
	func() {
		writerCtx, release, err := world.store.AcquireJobLifecycle(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		contended, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel()
		if _, err := engine.ApplyProducerCleanup(contended, world.job.ID, inspection.ResourceID, inspection.EligibilityToken, workflow.CleanupPolicy{}); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("cleanup crossed an active enqueue lifecycle: %v", err)
		}
		mailbox := workflow.NewMailbox(world.store, nil)
		if _, err := mailbox.Enqueue(writerCtx, workflow.JobRequest{
			ID: "late-consumer", Agent: "consumer", Action: "ask", Repo: "owner/repo",
			ParentJobID: world.job.ID, WorktreePath: world.path,
		}); err != nil {
			t.Fatal(err)
		}
	}()
	result, err := engine.ApplyProducerCleanup(ctx, world.job.ID, inspection.ResourceID, inspection.EligibilityToken, workflow.CleanupPolicy{})
	if err != nil || result.State != "refused" {
		t.Fatalf("stale admission ignored newly queued consumer: %+v %v", result, err)
	}
	content, err := os.ReadFile(filepath.Join(world.path, "result.txt"))
	if err != nil || string(content) != "durable producer input\n" {
		t.Fatalf("consumer lost producer input: %q %v", content, err)
	}
	consumer, err := world.store.GetJob(ctx, "late-consumer")
	if err != nil || consumer.State != "queued" {
		t.Fatalf("cleanup changed consumer state: %+v %v", consumer, err)
	}
}

func appendCleanupFixtureBudget(t *testing.T, configFile string, limit int64) {
	t.Helper()
	f, err := os.OpenFile(configFile, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := fmt.Fprintf(f, "\n[cleanup_budget]\nlimit_bytes = %d\nowner = \"fixture-owner\"\napproval = \"fixture-only\"\nmax_entries = 100000\nmax_depth = 64\ntimeout_ms = 30000\n", limit); err != nil {
		t.Fatal(err)
	}
}

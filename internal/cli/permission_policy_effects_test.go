package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/permissionpolicy"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

type permissionPolicyEffectGitFake struct {
	status    string
	statusErr error
	behindErr error
	remote    map[string]struct{}
	remoteErr error
	calls     []string
}

func (f *permissionPolicyEffectGitFake) StatusPorcelain(context.Context) (string, error) {
	f.calls = append(f.calls, "status")
	return f.status, f.statusErr
}

func (f *permissionPolicyEffectGitFake) BehindCount(_ context.Context, upstream string) (int, error) {
	f.calls = append(f.calls, "behind:"+upstream)
	return 0, f.behindErr
}

func (f *permissionPolicyEffectGitFake) RemoteBranches(_ context.Context, branches []string) (map[string]struct{}, error) {
	f.calls = append(f.calls, fmt.Sprintf("remote:%v", branches))
	return f.remote, f.remoteErr
}

func permissionPolicyEffectWarning(t *testing.T, store *db.Store, jobID string) permissionpolicy.Warning {
	t.Helper()
	events, err := store.ListJobEvents(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind != permissionpolicy.WarningEventKind {
			continue
		}
		var warning permissionpolicy.Warning
		if err := json.Unmarshal([]byte(event.Message), &warning); err != nil {
			t.Fatal(err)
		}
		return warning
	}
	t.Fatalf("job %s has no permission-policy warning", jobID)
	return permissionpolicy.Warning{}
}

func TestPermissionPolicyEffectCaptureFailureDoesNotFailCleanJob(t *testing.T) {
	git := &permissionPolicyEffectGitFake{
		behindErr: errors.New("local upstream unavailable"),
		remoteErr: errors.New("remote unavailable"),
	}
	store, job, _ := runPermissionPolicyJobWithEffectGit(t, runtime.PermissionPolicyNotApplied, func(string) permissionpolicy.EffectGit { return git })
	stored, err := store.GetJob(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != string(workflow.JobSucceeded) {
		t.Fatalf("job state = %q, want succeeded despite observation failure", stored.State)
	}
	for _, event := range mustListJobEvents(t, store, job.ID) {
		if event.Kind == "advance_retry" {
			t.Fatalf("capture failure altered advancement with retry event: %+v", event)
		}
	}
	effects := permissionPolicyEffectWarning(t, store, job.ID).Effects
	if effects == nil || effects.CheckoutDirty == nil || *effects.CheckoutDirty {
		t.Fatalf("effects = %#v, want checkout_dirty=false", effects)
	}
}

func TestPermissionPolicyEffectCaptureFailureLogsResolvedLocation(t *testing.T) {
	t.Run("job lookup uses caller checkout and identifies non-repository", func(t *testing.T) {
		checkout := t.TempDir()
		var output bytes.Buffer
		worker := defaultJobWorker(daemonWorkerStore(t), &output)
		if err := worker.capturePermissionPolicyEffects(context.Background(), "missing-policy-job", checkout); err == nil {
			t.Fatal("capture returned nil error for missing job")
		}
		assertPermissionPolicyCaptureLocation(t, output.String(), checkout, "true", "false")
	})

	t.Run("payload parse uses caller checkout and identifies absent path", func(t *testing.T) {
		ctx := context.Background()
		store := daemonWorkerStore(t)
		enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "malformed-policy-job", Agent: "policy-agent", Action: "ask", Repo: "owner/repo"})
		if err := store.UpdateJobPayload(ctx, "malformed-policy-job", "{"); err != nil {
			t.Fatal(err)
		}
		checkout := filepath.Join(t.TempDir(), "missing")
		var output bytes.Buffer
		worker := defaultJobWorker(store, &output)
		if err := worker.capturePermissionPolicyEffects(ctx, "malformed-policy-job", checkout); err == nil {
			t.Fatal("capture returned nil error for malformed payload")
		}
		assertPermissionPolicyCaptureLocation(t, output.String(), checkout, "false", "unknown")
	})

	t.Run("effect capture uses payload fallback and identifies work tree", func(t *testing.T) {
		ctx := context.Background()
		store := daemonWorkerStore(t)
		checkout := createDaemonWorkerGitCheckout(t, "main")
		enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
			ID: "fallback-policy-job", Agent: "policy-agent", Action: "ask",
			Repo: "owner/repo", Branch: "main", WorktreePath: checkout,
		})
		job, err := store.GetJob(ctx, "fallback-policy-job")
		if err != nil {
			t.Fatal(err)
		}
		agent := runtime.Agent{
			Name: "policy-agent", Runtime: runtime.ShellRuntime, RuntimeRef: "printf done",
			AutonomyPolicy: runtime.AutonomyPolicyAuto,
		}
		claimed, err := permissionpolicy.RecordWarning(ctx, store, job, agent, &permissionPolicyTestAdapter{property: runtime.PermissionPolicyNotApplied}, time.Now())
		if err != nil || !claimed {
			t.Fatalf("RecordWarning = claimed %t, err %v", claimed, err)
		}

		var output bytes.Buffer
		usedCheckout := ""
		worker := defaultJobWorker(store, &output)
		worker.PermissionPolicyEffectGit = func(got string) permissionpolicy.EffectGit {
			usedCheckout = got
			return &permissionPolicyEffectGitFake{
				behindErr: errors.New("local upstream unavailable"),
				remoteErr: errors.New("remote unavailable"),
			}
		}
		if err := worker.capturePermissionPolicyEffects(ctx, job.ID, ""); err == nil {
			t.Fatal("capture returned nil error for remote failure")
		}
		if usedCheckout != checkout {
			t.Fatalf("effect git checkout = %q, want payload fallback %q", usedCheckout, checkout)
		}
		assertPermissionPolicyCaptureLocation(t, output.String(), checkout, "true", "true")
	})

	t.Run("effect capture logs caller checkout when payload worktree differs", func(t *testing.T) {
		ctx := context.Background()
		store := daemonWorkerStore(t)
		checkout := t.TempDir()
		payloadCheckout := filepath.Join(t.TempDir(), "different-payload-checkout")
		enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
			ID: "caller-checkout-policy-job", Agent: "policy-agent", Action: "ask",
			Repo: "owner/repo", Branch: "main", WorktreePath: payloadCheckout,
		})
		job, err := store.GetJob(ctx, "caller-checkout-policy-job")
		if err != nil {
			t.Fatal(err)
		}
		agent := runtime.Agent{
			Name: "policy-agent", Runtime: runtime.ShellRuntime, RuntimeRef: "printf done",
			AutonomyPolicy: runtime.AutonomyPolicyAuto,
		}
		claimed, err := permissionpolicy.RecordWarning(ctx, store, job, agent, &permissionPolicyTestAdapter{property: runtime.PermissionPolicyNotApplied}, time.Now())
		if err != nil || !claimed {
			t.Fatalf("RecordWarning = claimed %t, err %v", claimed, err)
		}

		var output bytes.Buffer
		worker := defaultJobWorker(store, &output)
		worker.PermissionPolicyEffectGit = func(string) permissionpolicy.EffectGit {
			return &permissionPolicyEffectGitFake{
				behindErr: errors.New("local upstream unavailable"),
				remoteErr: errors.New("remote unavailable"),
			}
		}
		if err := worker.capturePermissionPolicyEffects(ctx, job.ID, checkout); err == nil {
			t.Fatal("capture returned nil error for remote failure")
		}
		assertPermissionPolicyCaptureLocation(t, output.String(), checkout, "true", "false")
	})

	t.Run("runner resolution logs caller checkout", func(t *testing.T) {
		ctx := context.Background()
		store := daemonWorkerStore(t)
		checkout := t.TempDir()
		enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
			ID: "runner-resolution-policy-job", Agent: "policy-agent", Action: "ask",
			Repo: "owner/repo", Branch: "main", WorktreePath: filepath.Join(t.TempDir(), "payload-checkout"),
		})
		job, err := store.GetJob(ctx, "runner-resolution-policy-job")
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
			t.Fatal(err)
		}
		payload["exec_backend"] = "unimplemented-runner"
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.UpdateJobPayload(ctx, job.ID, string(encoded)); err != nil {
			t.Fatal(err)
		}
		job, err = store.GetJob(ctx, job.ID)
		if err != nil {
			t.Fatal(err)
		}

		var output bytes.Buffer
		worker := defaultJobWorker(store, &output)
		err = worker.capturePermissionPolicyEffects(ctx, job.ID, checkout)
		if err == nil || !strings.Contains(err.Error(), "resolve permission-policy effect runner") {
			t.Fatalf("capture error = %v, want runner resolution failure", err)
		}
		assertPermissionPolicyCaptureLocation(t, output.String(), checkout, "true", "false")
	})
}

func assertPermissionPolicyCaptureLocation(t *testing.T, output, checkout, exists, workTree string) {
	t.Helper()
	want := fmt.Sprintf("path=%q exists=%s work_tree=%s", workflow.RedactCommentText(checkout), exists, workTree)
	if !strings.Contains(output, want) {
		t.Fatalf("capture failure output = %q, want location %q", output, want)
	}
}

func TestPermissionPolicyEffectCaptureRecordsDirtyCheckout(t *testing.T) {
	git := &permissionPolicyEffectGitFake{status: " M internal/file.go"}
	store, job, _ := runPermissionPolicyJobWithEffectGit(t, runtime.PermissionPolicyNotApplied, func(string) permissionpolicy.EffectGit { return git })
	effects := permissionPolicyEffectWarning(t, store, job.ID).Effects
	if effects == nil || effects.CheckoutDirty == nil || !*effects.CheckoutDirty {
		t.Fatalf("effects = %#v, want checkout_dirty=true", effects)
	}
}

func TestPermissionPolicyAppliedPathSkipsEffectGit(t *testing.T) {
	factoryCalls := 0
	git := &permissionPolicyEffectGitFake{}
	store, job, _ := runPermissionPolicyJobWithEffectGit(t, runtime.PermissionPolicyApplied, func(string) permissionpolicy.EffectGit {
		factoryCalls++
		return git
	})
	if factoryCalls != 0 || len(git.calls) != 0 {
		t.Fatalf("applied path effect git calls: factory=%d git=%v, want none", factoryCalls, git.calls)
	}
	for _, event := range mustListJobEvents(t, store, job.ID) {
		if event.Kind == permissionpolicy.WarningEventKind {
			t.Fatalf("applied path recorded permission-policy observation: %+v", event)
		}
	}
}

func TestPermissionPolicyEffectCapturePreservesUnknownPushState(t *testing.T) {
	git := &permissionPolicyEffectGitFake{
		behindErr: errors.New("no local upstream"),
		remoteErr: errors.New("network unavailable"),
	}
	store, job, _ := runPermissionPolicyJobWithEffectGit(t, runtime.PermissionPolicyNotApplied, func(string) permissionpolicy.EffectGit { return git })
	effects := permissionPolicyEffectWarning(t, store, job.ID).Effects
	if effects == nil || effects.BranchPushed != nil || effects.BranchPushedInstrument != permissionpolicy.PushInstrumentUnavailable {
		t.Fatalf("effects = %#v, want branch_pushed=null instrument=%q", effects, permissionpolicy.PushInstrumentUnavailable)
	}
}

func TestPermissionPolicyEffectCapturePreservesBranchlessPushUnknown(t *testing.T) {
	git := &permissionPolicyEffectGitFake{}
	store, job, _ := runPermissionPolicyJobWithPayload(t, runtime.PermissionPolicyNotApplied, func(string) permissionpolicy.EffectGit { return git }, "", 0)
	effects := permissionPolicyEffectWarning(t, store, job.ID).Effects
	if effects == nil || effects.BranchPushed != nil || effects.BranchPushedInstrument != permissionpolicy.PushInstrumentPayload {
		t.Fatalf("effects = %#v, want branch_pushed=null instrument=%q", effects, permissionpolicy.PushInstrumentPayload)
	}
	if got := fmt.Sprint(git.calls); got != "[status]" {
		t.Fatalf("git calls = %s, want status only without an invented branch probe", got)
	}
}

func TestPermissionPolicyEffectCaptureNamesPullRequestPayloadFact(t *testing.T) {
	for _, tc := range []struct {
		name        string
		pullRequest int
		want        bool
	}{{"present", 42, true}, {"absent", 0, false}} {
		t.Run(tc.name, func(t *testing.T) {
			store, job, _ := runPermissionPolicyJobWithPayload(t, runtime.PermissionPolicyNotApplied, func(string) permissionpolicy.EffectGit {
				return &permissionPolicyEffectGitFake{}
			}, "main", tc.pullRequest)
			for _, event := range mustListJobEvents(t, store, job.ID) {
				if event.Kind != permissionpolicy.WarningEventKind {
					continue
				}
				var warning map[string]any
				if err := json.Unmarshal([]byte(event.Message), &warning); err != nil {
					t.Fatal(err)
				}
				effects, ok := warning["effects"].(map[string]any)
				if !ok {
					t.Fatalf("warning effects = %#v, want object", warning["effects"])
				}
				got, exists := effects["payload_had_pull_request"]
				if !exists || got != tc.want {
					t.Fatalf("effects = %#v, want present payload_had_pull_request=%t", effects, tc.want)
				}
				if _, exists := effects["pr_opened"]; exists {
					t.Fatalf("effects = %#v, pr_opened overstates the payload observation", effects)
				}
				return
			}
			t.Fatal("permission-policy warning not found")
		})
	}
}

func TestPermissionPolicyEffectsRemainAttachedToQueryableObservation(t *testing.T) {
	git := &permissionPolicyEffectGitFake{remote: map[string]struct{}{"main": {}}}
	store, job, _ := runPermissionPolicyJobWithEffectGit(t, runtime.PermissionPolicyNotApplied, func(string) permissionpolicy.EffectGit { return git })
	claim, ok, err := store.LatestPermissionPolicyWarningClaim(context.Background(), "policy-agent", runtime.ShellRuntime, runtime.AutonomyPolicyAuto)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || claim.JobID != job.ID {
		t.Fatalf("claim = %#v, present=%t, want job %s", claim, ok, job.ID)
	}
	effects := permissionPolicyEffectWarning(t, store, claim.JobID).Effects
	if effects == nil || effects.CheckoutDirty == nil || effects.BranchPushed == nil || !*effects.BranchPushed {
		t.Fatalf("claimed observation effects = %#v, want complete attached capture", effects)
	}
}

func TestPermissionPolicyUnresolvedEffectCaptureUsesAvailableCheckout(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	git := &permissionPolicyEffectGitFake{status: "?? observed.txt"}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "missing-agent-effect-job", Agent: "deleted-agent", Action: "ask",
		Repo: "owner/repo", Branch: "main", WorktreePath: t.TempDir(),
	})
	job, err := store.GetJob(ctx, "missing-agent-effect-job")
	if err != nil {
		t.Fatal(err)
	}
	worker := defaultJobWorker(store, io.Discard)
	worker.PermissionPolicyEffectGit = func(string) permissionpolicy.EffectGit { return git }
	if err := worker.run(ctx, job); err != nil {
		t.Fatal(err)
	}
	effects := permissionPolicyEffectWarning(t, store, job.ID).Effects
	if effects == nil || effects.CheckoutDirty == nil || !*effects.CheckoutDirty {
		t.Fatalf("unresolved effects = %#v, want available checkout observation", effects)
	}
}

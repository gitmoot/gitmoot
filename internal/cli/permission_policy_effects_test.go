package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"testing"

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

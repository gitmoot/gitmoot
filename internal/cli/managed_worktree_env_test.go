package cli

import (
	"context"
	"reflect"
	"testing"

	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

type managedWorktreeEnvCaptureAdapter struct {
	jobs []runtime.Job
}

func (a *managedWorktreeEnvCaptureAdapter) Deliver(_ context.Context, _ runtime.Agent, job runtime.Job) (runtime.Result, error) {
	a.jobs = append(a.jobs, job)
	return runtime.Result{}, nil
}

func TestManagedWorktreeRuntimeEnvContainsBuildVCSFlag(t *testing.T) {
	t.Setenv("GOFLAGS", "-mod=readonly")
	capture := &managedWorktreeEnvCaptureAdapter{}
	adapter := wrapManagedWorktreeRuntimeEnv(
		workflow.JobPayload{WorktreePath: t.TempDir()},
		capture,
	)

	_, err := adapter.Deliver(context.Background(), runtime.Agent{}, runtime.Job{
		AgentEnv: []string{"GOFLAGS=-trimpath"},
		ShellEnv: []string{"SHELL_MARKER=1"},
	})
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if len(capture.jobs) != 1 {
		t.Fatalf("deliveries = %d, want 1", len(capture.jobs))
	}
	if got, want := capture.jobs[0].AgentEnv, []string{"GOFLAGS=-trimpath -buildvcs=false"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("AgentEnv = %#v, want %#v", got, want)
	}
	if got, want := capture.jobs[0].ShellEnv, []string{"SHELL_MARKER=1", "GOFLAGS=-mod=readonly -buildvcs=false"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ShellEnv = %#v, want %#v", got, want)
	}
}

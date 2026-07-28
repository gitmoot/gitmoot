package cli

import (
	"context"
	"os"
	"strings"

	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

const managedWorktreeBuildVCSFlag = "-buildvcs=false"

type managedWorktreeEnvAdapter struct {
	inner            workflow.DeliveryAdapter
	inheritedGoFlags string
}

func wrapManagedWorktreeRuntimeEnv(payload workflow.JobPayload, adapter workflow.DeliveryAdapter) workflow.DeliveryAdapter {
	if adapter == nil || strings.TrimSpace(payload.WorktreePath) == "" {
		return adapter
	}
	return managedWorktreeEnvAdapter{inner: adapter, inheritedGoFlags: os.Getenv("GOFLAGS")}
}

func (a managedWorktreeEnvAdapter) Deliver(ctx context.Context, agent runtime.Agent, job runtime.Job) (runtime.Result, error) {
	job.AgentEnv = appendManagedWorktreeGoFlags(job.AgentEnv, a.inheritedGoFlags)
	job.ShellEnv = appendManagedWorktreeGoFlags(job.ShellEnv, a.inheritedGoFlags)
	return a.inner.Deliver(ctx, agent, job)
}

func appendManagedWorktreeGoFlags(env []string, inherited string) []string {
	effective := inherited
	last := -1
	for i, entry := range env {
		name, value, ok := strings.Cut(entry, "=")
		if ok && name == "GOFLAGS" {
			effective = value
			last = i
		}
	}
	if !containsGoFlag(effective, managedWorktreeBuildVCSFlag) {
		effective = strings.TrimSpace(effective + " " + managedWorktreeBuildVCSFlag)
	}

	out := append([]string(nil), env...)
	entry := "GOFLAGS=" + effective
	if last >= 0 {
		out[last] = entry
		return out
	}
	return append(out, entry)
}

func containsGoFlag(value, want string) bool {
	for _, flag := range strings.Fields(value) {
		if flag == want {
			return true
		}
	}
	return false
}

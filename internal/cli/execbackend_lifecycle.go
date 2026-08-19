package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/execbackend"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/subprocess"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

const executionBackendDestroyTimeout = 30 * time.Second

var reapedExecutionBackendRoots sync.Map

func (w jobWorker) defaultExecutionBackend(backend execbackend.Backend) (execbackend.ExecutionBackend, error) {
	return execbackend.Consume(backend, func() (execbackend.ExecutionBackend, error) {
		home := strings.TrimSpace(w.workflowHome())
		if home == "" {
			return nil, errors.New("resolve local execution-backend home")
		}
		local, err := execbackend.NewLocalBackend(filepath.Join(home, "execbackends", string(execbackend.Local)))
		if err != nil {
			return nil, err
		}
		// Reap once per resolved root in this process. A restarted daemon has a
		// fresh map and therefore reconciles the prior process's instances before
		// provisioning its first new job.
		rootKey := filepath.Join(home, "execbackends", string(execbackend.Local))
		if _, loaded := reapedExecutionBackendRoots.LoadOrStore(rootKey, struct{}{}); !loaded {
			if _, err := local.Reap(context.Background()); err != nil {
				reapedExecutionBackendRoots.Delete(rootKey)
				return nil, fmt.Errorf("reap local execution backends: %w", err)
			}
		}
		return local, nil
	})
}

func (w jobWorker) provisionExecutionBackend(ctx context.Context, backend execbackend.Backend, job db.Job, checkout string) (execbackend.ExecutionBackend, *execbackend.Instance, error) {
	if w.ExecutionBackendFactory == nil {
		return nil, nil, nil
	}
	lifecycle, err := w.ExecutionBackendFactory(backend)
	if err != nil {
		return nil, nil, fmt.Errorf("construct %s execution backend: %w", backend, err)
	}
	instance, err := lifecycle.Provision(ctx, execbackend.JobScope{JobID: job.ID, LifecycleGeneration: job.LifecycleGeneration})
	if err != nil {
		return nil, nil, fmt.Errorf("provision %s execution backend for job %s: %w", backend, job.ID, err)
	}
	if err := lifecycle.SyncIn(ctx, instance, execbackend.Materials{SourceWorktree: checkout}); err != nil {
		return lifecycle, instance, fmt.Errorf("sync job %s into %s execution backend: %w", job.ID, backend, err)
	}
	return lifecycle, instance, nil
}

func executionChangeSetCollector(lifecycle execbackend.ExecutionBackend, instance *execbackend.Instance, liveBackend execbackend.Backend, liveJobID string) func(context.Context, execbackend.Backend, string) (*execbackend.ChangeSet, error) {
	if lifecycle == nil || instance == nil {
		return nil
	}
	return func(ctx context.Context, requestedBackend execbackend.Backend, requestedJobID string) (*execbackend.ChangeSet, error) {
		if requestedBackend != liveBackend {
			return nil, fmt.Errorf("collect changeset backend %q does not match live instance backend %q", requestedBackend, liveBackend)
		}
		if requestedJobID != liveJobID {
			return nil, fmt.Errorf("collect changeset job %q does not own live instance for job %q", requestedJobID, liveJobID)
		}
		changes, err := lifecycle.Collect(ctx, instance)
		if err != nil {
			return nil, err
		}
		return &changes, nil
	}
}

func (w jobWorker) destroyExecutionBackend(jobID string, lifecycle execbackend.ExecutionBackend, instance *execbackend.Instance) {
	if lifecycle == nil || instance == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), executionBackendDestroyTimeout)
	defer cancel()
	cancelled := false
	if w.Store != nil {
		if latest, err := w.Store.GetJob(ctx, jobID); err == nil {
			cancelled = latest.State == string(workflow.JobCancelled)
		}
	}
	var err error
	if cancelled {
		err = lifecycle.Cancel(ctx, instance)
	} else {
		err = lifecycle.Destroy(ctx, instance)
	}
	if err == nil {
		return
	}
	writeLine(w.Stdout, "job %s execution backend teardown failed: %v", jobID, err)
	if w.Store != nil {
		_ = w.Store.AddJobEvent(context.Background(), db.JobEvent{JobID: jobID, Kind: "execbackend_destroy_failed", Message: err.Error()})
	}
}

func (w *jobWorker) executionDeliveryAdapter(agent runtime.Agent, checkout string, relayToken string, outputs ...io.Writer) (workflow.DeliveryAdapter, error) {
	runner := w.executionRunner
	if runner == nil {
		return nil, errors.New("execution-backend runtime runner is required")
	}
	if relayToken != "" {
		if w.RelayServer == nil {
			return nil, errors.New("execution-backend relay token has no relay server")
		}
		runner = subprocess.EnvInjectingRunner{Inner: runner, Env: []string{
			chatRelayEnvSocket + "=" + w.RelayServer.SocketPath(),
			chatRelayEnvToken + "=" + relayToken,
		}}
	}
	// Keep the relay wrapper as part of the run-scoped base so a later cockpit
	// tee rebuild preserves both backend execution and seat authentication.
	w.executionRunner = runner
	if len(outputs) > 0 && outputs[0] != nil {
		stream, ok := runner.(subprocess.StreamRunner)
		if !ok {
			return nil, errors.New("execution-backend runtime runner does not support streaming")
		}
		runner = subprocess.TeeRunner{Inner: stream, Out: runtimeOutputWriter(outputs...)}
	}
	return buildRuntimeAdapter(w.ConfigHome, agent, checkout, runner)
}

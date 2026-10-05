//go:build e2e

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/execbackend"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

var errSandboxdProbeStop = errors.New("probe stops at the first command in the sandbox")

// sandboxdProvisionProbe is the real sandboxd execution backend up to and
// including Provision and Destroy. It skips the workspace sync and file
// installs and refuses the first command, so a test observes admission
// without a data plane.
type sandboxdProvisionProbe struct {
	execbackend.ExecutionBackend
}

func (sandboxdProvisionProbe) InstallInstanceFile(_ context.Context, _ *execbackend.Instance, path string, _ io.Reader, _ os.FileMode) (string, error) {
	return path, nil
}

func (sandboxdProvisionProbe) SyncIn(context.Context, *execbackend.Instance, execbackend.Materials) error {
	return nil
}

func (sandboxdProvisionProbe) Exec(context.Context, *execbackend.Instance, execbackend.Command) (execbackend.Stream, error) {
	return nil, errSandboxdProbeStop
}

// newSandboxdConflictReview is a remote review explicitly requested on
// sandboxd (not checks-routed), provisioned through the production backend
// against a fake gateway that reports two free slots but refuses the create
// with 409, as sandboxd does when another client took the slot first.
func newSandboxdConflictReview(t *testing.T) (*remoteReviewAdmissionFixture, *fakeSandboxd) {
	t.Helper()
	f := newRemoteReviewAdmissionFixture(t, runtime.ShellRuntime)
	gateway := newFakeSandboxd(t)
	gateway.reportCapacity(2, map[string]int{"review-arm64": 2})
	gateway.set(func(g *fakeSandboxd) { g.createStatus = 409 })

	file, err := os.OpenFile(f.paths.ConfigFile, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(gateway.configSection(t, "sandboxd", 0)); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(f.job.Payload), &payload); err != nil {
		t.Fatal(err)
	}
	payload["exec_provider"] = "sandboxd"
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.UpdateJobPayload(f.ctx, f.job.ID, string(encoded)); err != nil {
		t.Fatal(err)
	}
	if f.job, err = f.store.GetJob(f.ctx, f.job.ID); err != nil {
		t.Fatal(err)
	}
	worker := f.worker
	f.worker.ExecutionBackendFactory = func(backend execbackend.Backend, cfg config.RemoteExecConfig) (execbackend.ExecutionBackend, error) {
		f.factoryCalls++
		if cfg.Provider != "sandboxd" || cfg.E2BBaseURL != sandboxdPlaceholderOrigin {
			t.Errorf("review provisioned against provider %q at %q; want sandboxd at the configured gateway", cfg.Provider, cfg.E2BBaseURL)
		}
		// config.toml only accepts HTTPS gateways; the fake listens on HTTP.
		cfg.E2BBaseURL, cfg.E2BEnvdBaseURL = gateway.server.URL, gateway.server.URL
		inner, err := worker.defaultExecutionBackend(backend, cfg)
		if err != nil {
			return nil, err
		}
		return sandboxdProvisionProbe{inner}, nil
	}
	return f, gateway
}

func sandboxdJobEvents(t *testing.T, f *remoteReviewAdmissionFixture) []db.JobEvent {
	t.Helper()
	events, err := f.store.ListJobEvents(f.ctx, f.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

// requireQueuedOnSandboxdConflict runs the review once against the 409 and
// checks it waits visibly instead of failing.
func requireQueuedOnSandboxdConflict(t *testing.T, f *remoteReviewAdmissionFixture, gateway *fakeSandboxd) db.Job {
	t.Helper()
	f.run(t)
	waiting, err := f.store.GetJob(f.ctx, f.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if waiting.State != string(workflow.JobQueued) {
		t.Fatalf("review refused by sandboxd's 409 is %s; want queued (waiting for capacity), not failed. events: %+v", waiting.State, sandboxdJobEvents(t, f))
	}
	var held map[string]any
	if err := json.Unmarshal([]byte(waiting.Payload), &held); err != nil {
		t.Fatal(err)
	}
	if held["blocker_retry_at"] == nil || held["remote_cap_wait_since"] == nil {
		t.Fatalf("waiting payload has no hold: %v", held)
	}
	if !queuedJobBlockerHeld(waiting, time.Now()) {
		t.Fatal("waiting review is not held from the next scheduler tick")
	}
	visible := false
	for _, event := range sandboxdJobEvents(t, f) {
		if event.Kind == "remote_review_cap_waiting" && strings.Contains(event.Message, "sandboxd capacity") && strings.Contains(event.Message, "409") {
			visible = true
		}
	}
	if !visible {
		t.Fatalf("no remote_review_cap_waiting event naming sandboxd capacity and the 409: %+v", sandboxdJobEvents(t, f))
	}
	if _, posts := gateway.counts(); posts != 1 {
		t.Fatalf("sandboxd saw %d creates; want the one it refused", posts)
	}
	rows, err := f.store.ListExecBackendAttemptsForJob(f.ctx, f.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].State != "failed" {
		t.Fatalf("ledger rows after the 409 = %+v; want one released (failed) row, holding no slot", rows)
	}
	return waiting
}

// sandboxd answers 409 when its capacity report raced another client. The
// review it refused allocated nothing, so it waits in the queue with a visible
// reason and is admitted on a later tick once the gateway has room, reading
// capacity afresh. The wait is bounded by the job timeout.
func TestSandboxdCreateConflictQueuesReviewUntilCapacityFrees(t *testing.T) {
	t.Run("admitted once capacity frees", func(t *testing.T) {
		f, gateway := newSandboxdConflictReview(t)
		f.job = requireQueuedOnSandboxdConflict(t, f, gateway)

		gateway.set(func(g *fakeSandboxd) { g.createStatus = 0 })
		f.run(t)
		gets, posts := gateway.counts()
		if posts != 2 {
			t.Fatalf("sandboxd saw %d creates; want the waiting review created on its retry", posts)
		}
		if gets != 2 {
			t.Fatalf("capacity reads = %d; want a fresh read after the 409 dropped the cached report", gets)
		}
		delivered := false
		for _, event := range sandboxdJobEvents(t, f) {
			if strings.HasPrefix(event.Kind, "remote_review_admission_avoided_") {
				t.Fatalf("retry after the wait was refused: %+v", event)
			}
			// The probe refuses the first command, so this failure proves the
			// review was provisioned and reached its sandbox.
			if event.Kind == "failed" && strings.Contains(event.Message, errSandboxdProbeStop.Error()) {
				delivered = true
			}
		}
		if !delivered {
			t.Fatalf("admitted review never reached its sandbox: %+v", sandboxdJobEvents(t, f))
		}
		admitted, err := f.store.GetJob(f.ctx, f.job.ID)
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(admitted.Payload), &payload); err != nil {
			t.Fatal(err)
		}
		if payload["remote_cap_wait_since"] != nil || payload["blocker_retry_at"] != nil {
			t.Fatalf("admitted review still carries its capacity hold: %v", payload)
		}
	})

	t.Run("wait is bounded", func(t *testing.T) {
		f, gateway := newSandboxdConflictReview(t)
		waiting := requireQueuedOnSandboxdConflict(t, f, gateway)
		var aged map[string]any
		if err := json.Unmarshal([]byte(waiting.Payload), &aged); err != nil {
			t.Fatal(err)
		}
		// Start the wait two days ago: far past any job timeout.
		aged["remote_cap_wait_since"] = time.Now().UTC().Add(-48 * time.Hour).Format(time.RFC3339Nano)
		encoded, err := json.Marshal(aged)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.store.UpdateJobPayload(f.ctx, f.job.ID, string(encoded)); err != nil {
			t.Fatal(err)
		}
		if f.job, err = f.store.GetJob(f.ctx, f.job.ID); err != nil {
			t.Fatal(err)
		}
		f.run(t)
		failed, err := f.store.GetJob(f.ctx, f.job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if failed.State != string(workflow.JobFailed) {
			t.Fatalf("review past its wait bound is %s; want failed", failed.State)
		}
		reason := false
		for _, event := range sandboxdJobEvents(t, f) {
			if event.Kind == "failed" && strings.Contains(event.Message, "sandboxd capacity") && strings.Contains(event.Message, "still full") {
				reason = true
			}
		}
		if !reason {
			t.Fatalf("expired wait has no failure naming sandboxd capacity: %+v", sandboxdJobEvents(t, f))
		}
	})
}

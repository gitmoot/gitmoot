//go:build e2e

package cli

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/execbackend"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// markChecksRouted writes the #2316 payload keys as raw JSON, so this test
// compiles on main and fails there for the behaviour, not the build.
func markChecksRouted(t *testing.T, f *remoteReviewAdmissionFixture, template string) {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal([]byte(f.job.Payload), &payload); err != nil {
		t.Fatal(err)
	}
	payload["review_checks_routed"] = true
	payload["exec_template"] = template
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.UpdateJobPayload(f.ctx, f.job.ID, string(encoded)); err != nil {
		t.Fatal(err)
	}
	job, err := f.store.GetJob(f.ctx, f.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.job = job
}

func TestChecksRoutedReviewWaitsForFullCapThenIsAdmitted(t *testing.T) {
	f := newRemoteReviewAdmissionFixture(t, runtime.ShellRuntime)
	markChecksRouted(t, f, "swift-tmpl")
	var templates []string
	f.worker.ExecutionBackendFactory = func(_ execbackend.Backend, cfg config.RemoteExecConfig) (execbackend.ExecutionBackend, error) {
		f.factoryCalls++
		templates = append(templates, cfg.E2BTemplate)
		return f.backend, nil
	}
	f.backend.provisionErr = &db.ExecBackendCapRefusal{Clause: "concurrency", LiveCount: 2, MaxConcurrent: 2}
	f.run(t)

	waiting, err := f.store.GetJob(f.ctx, f.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if waiting.State != string(workflow.JobQueued) {
		t.Fatalf("cap-full checks-routed review state = %s; want queued (waiting), not failed", waiting.State)
	}
	if len(templates) != 1 || templates[0] != "swift-tmpl" {
		t.Fatalf("provisioned with templates %v; want the repository's checks_template", templates)
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
	events, err := f.store.ListJobEvents(f.ctx, f.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	sawWaiting := false
	for _, event := range events {
		if event.Kind == "remote_review_cap_waiting" && strings.Contains(event.Message, "capacity") {
			sawWaiting = true
		}
	}
	if !sawWaiting {
		t.Fatalf("no visible waiting event: %+v", events)
	}

	// A slot frees: the next tick (after the hold) admits it to provisioning
	// again instead of refusing it as a second cloud attempt.
	freed := errors.New("slot freed; probe stops after Provision")
	f.backend.provisionErr = freed
	f.job = waiting
	f.run(t)
	if f.backend.provisionCalls != 2 {
		t.Fatalf("provision calls = %d; want the waiting review admitted again", f.backend.provisionCalls)
	}
	events, err = f.store.ListJobEvents(f.ctx, f.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if strings.HasPrefix(event.Kind, "remote_review_admission_avoided_") {
			t.Fatalf("admitted retry was refused: %+v", event)
		}
	}
}

func TestChecksRoutedReviewCapWaitIsBounded(t *testing.T) {
	f := newRemoteReviewAdmissionFixture(t, runtime.ShellRuntime)
	markChecksRouted(t, f, "")
	f.backend.provisionErr = &db.ExecBackendCapRefusal{Clause: "concurrency", LiveCount: 2, MaxConcurrent: 2}
	f.run(t)
	waiting, err := f.store.GetJob(f.ctx, f.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if waiting.State != string(workflow.JobQueued) {
		t.Fatalf("first refusal state = %s; want queued", waiting.State)
	}
	previous := remoteReviewCapWaitNow
	remoteReviewCapWaitNow = func() time.Time { return time.Now().UTC().Add(48 * time.Hour) }
	t.Cleanup(func() { remoteReviewCapWaitNow = previous })
	f.job = waiting
	f.run(t)
	failed, err := f.store.GetJob(f.ctx, f.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failed.State != string(workflow.JobFailed) {
		t.Fatalf("expired wait state = %s; want failed", failed.State)
	}
	events, err := f.store.ListJobEvents(f.ctx, f.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.Kind == "failed" && strings.Contains(event.Message, "still full") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expired wait has no clear failure reason: %+v", events)
	}
}

//go:build e2e

package cli

import (
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

// appendReviewChecksConfig appends config to the fixture home's config file.
func appendReviewChecksConfig(t *testing.T, paths config.Paths, body string) {
	t.Helper()
	file, err := os.OpenFile(paths.ConfigFile, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := io.WriteString(file, body)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		t.Fatal(err)
	}
}

// producerReviewFixture is a remote-review fixture whose review was enqueued
// the way every producer other than the dispatch verbs enqueues one (native PR
// fan-out, comment-triggered, heartbeat, pipeline): a plain Mailbox request
// with no exec_backend, exec_provider or exec_template. It records the backend
// and provider view the worker provisioned with.
type producerReviewFixture struct {
	*remoteReviewAdmissionFixture
	backends []execbackend.Backend
	views    []config.RemoteExecConfig
}

func newProducerReviewFixture(t *testing.T, runtimeName, checksConfig string, request workflow.JobRequest) *producerReviewFixture {
	t.Helper()
	base := newRemoteReviewAdmissionFixture(t, runtimeName)
	appendReviewChecksConfig(t, base.paths, checksConfig)
	request.Agent = "remote-review-agent"
	request.Action = "review"
	request.Repo = "owner/repo"
	request.Instructions = "review the exact head"
	if request.PullRequest != 0 {
		request.HeadSHA = base.head
		request.Branch = "remote-review-admission"
		request.ReviewPurpose = db.DefaultReviewPurpose
	}
	job, err := workflow.NewMailbox(base.store, workflow.UnavailableDeliveryWorktreeResolver("producer enqueue")).Enqueue(base.ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &producerReviewFixture{remoteReviewAdmissionFixture: base}
	fixture.job = job
	fixture.worker.ExecutionBackendFactory = func(backend execbackend.Backend, cfg config.RemoteExecConfig) (execbackend.ExecutionBackend, error) {
		fixture.backends = append(fixture.backends, backend)
		fixture.views = append(fixture.views, cfg)
		return fixture.backend, nil
	}
	return fixture
}

// jobAndEvents returns the stored job, its payload as JSON fields (read by
// name so this test compiles against payloads without the #2316 fields) and
// its events.
func (f *producerReviewFixture) jobAndEvents(t *testing.T) (db.Job, map[string]any, []db.JobEvent) {
	t.Helper()
	job, err := f.store.GetJob(f.ctx, f.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
		t.Fatal(err)
	}
	events, err := f.store.ListJobEvents(f.ctx, f.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	return job, payload, events
}

func hasJobEvent(events []db.JobEvent, kind string, contains ...string) bool {
	for _, event := range events {
		if event.Kind != kind {
			continue
		}
		matched := true
		for _, want := range contains {
			matched = matched && strings.Contains(event.Message, want)
		}
		if matched {
			return true
		}
	}
	return false
}

const producerChecksE2B = "\n[repos.\"owner/repo\".review]\nchecks_backend = \"remote\"\nchecks_template = \"swift-tmpl\"\n"

// A review enqueued with no backend by a producer other than the dispatch
// verbs, of a repository with checks_backend = "remote", provisions remotely
// with checks_template. Before #2316's fix it was forced local.
func TestReviewChecksRoutingAppliesToReviewEnqueuedWithoutBackend(t *testing.T) {
	f := newProducerReviewFixture(t, runtime.ShellRuntime, producerChecksE2B, workflow.JobRequest{
		ID: "producer-review", PullRequest: 2238,
	})
	f.run(t)

	if len(f.backends) != 1 || f.backends[0] != execbackend.Remote {
		t.Fatalf("provisioned backends %v; want exactly one remote provision", f.backends)
	}
	view := f.views[0]
	if view.Provider != config.RemoteExecProviderE2B || view.E2BTemplate != "swift-tmpl" || view.E2BOMPTemplate != "swift-tmpl" {
		t.Fatalf("provider view provider=%q e2b_template=%q e2b_omp_template=%q; want e2b with swift-tmpl for every runtime",
			view.Provider, view.E2BTemplate, view.E2BOMPTemplate)
	}
	if f.backend.provisionCalls != 1 {
		t.Fatalf("Provision calls = %d; want 1", f.backend.provisionCalls)
	}
	_, payload, events := f.jobAndEvents(t)
	if payload["exec_backend"] != "remote" || payload["exec_template"] != "swift-tmpl" || payload["review_checks_routed"] != true {
		t.Fatalf("stored payload exec_backend=%v exec_template=%v review_checks_routed=%v; want remote, swift-tmpl, true",
			payload["exec_backend"], payload["exec_template"], payload["review_checks_routed"])
	}
	if provider, present := payload["exec_provider"]; present && provider != "" {
		t.Fatalf("e2b provider must be stored as absent, got %v", provider)
	}
	if !hasJobEvent(events, "review_backend_route_selected", "source=repo_checks", "template=swift-tmpl") {
		t.Fatalf("missing repo_checks route event: %+v", events)
	}
}

// checks_provider = "mac" is adopted by a review that names no provider.
func TestReviewChecksRoutingProducerReviewAdoptsMacProvider(t *testing.T) {
	macKey := t.TempDir() + "/mac.key"
	if err := os.WriteFile(macKey, []byte("mac-test-key-0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	checks := "\n[remote_exec.mac]\napi_key_file = \"" + macKey + "\"\ntemplate = \"review-arm64\"\nbase_url = \"https://mac.example:8443\"\nenvd_base_url = \"https://mac.example:8443\"\nmax_concurrent = 1\ncost_max_reserved_usd = 5.0\ncost_per_attempt_usd = 0.5\n" +
		"\n[repos.\"owner/repo\".review]\nchecks_backend = \"remote\"\nchecks_provider = \"mac\"\nchecks_template = \"swift-arm64\"\n"
	f := newProducerReviewFixture(t, runtime.ShellRuntime, checks, workflow.JobRequest{
		ID: "producer-review-mac", PullRequest: 2238,
	})
	f.run(t)

	if len(f.views) != 1 || f.backends[0] != execbackend.Remote {
		t.Fatalf("provisioned backends %v; want exactly one remote provision", f.backends)
	}
	if view := f.views[0]; view.Provider != config.RemoteExecProviderMac || view.E2BTemplate != "swift-arm64" {
		t.Fatalf("provider view provider=%q e2b_template=%q; want mac with swift-arm64", view.Provider, view.E2BTemplate)
	}
	if _, payload, _ := f.jobAndEvents(t); payload["exec_provider"] != "mac" || payload["exec_template"] != "swift-arm64" {
		t.Fatalf("stored payload exec_provider=%v exec_template=%v; want mac, swift-arm64", payload["exec_provider"], payload["exec_template"])
	}
}

// A review that pins itself local is refused with the setting named, never run
// locally and never provisioned.
func TestReviewChecksRoutingRefusesProducerReviewPinnedLocal(t *testing.T) {
	local := string(execbackend.Local)
	f := newProducerReviewFixture(t, runtime.ShellRuntime, producerChecksE2B, workflow.JobRequest{
		ID: "producer-review-local", PullRequest: 2238, ExecBackend: &local,
	})
	f.run(t)

	if len(f.backends) != 0 {
		t.Fatalf("a review pinned local on a checks-routed repo reached the backend factory: %v", f.backends)
	}
	job, _, events := f.jobAndEvents(t)
	if job.State != string(workflow.JobFailed) {
		t.Fatalf("job state %q; want failed", job.State)
	}
	if !hasJobEvent(events, string(workflow.JobFailed), "exec_backend local refused", "checks_backend") {
		t.Fatalf("failure does not explain the checks_backend setting: %+v", events)
	}
}

// Without checks_template an omp review provisions e2b_omp_template; the
// fixture's provider sets only e2b_template, so the worker refuses the omp
// review naming the gap instead of failing at provisioning.
func TestReviewChecksRoutingRefusesProducerReviewWithoutRuntimeTemplate(t *testing.T) {
	f := newProducerReviewFixture(t, runtime.OmpRuntime,
		"\n[repos.\"owner/repo\".review]\nchecks_backend = \"remote\"\n",
		workflow.JobRequest{ID: "producer-review-template", PullRequest: 2238})
	f.run(t)

	if len(f.backends) != 0 {
		t.Fatalf("an omp review with no omp template reached the backend factory: %v", f.backends)
	}
	job, _, events := f.jobAndEvents(t)
	if job.State != string(workflow.JobFailed) {
		t.Fatalf("job state %q; want failed", job.State)
	}
	if !hasJobEvent(events, string(workflow.JobFailed), "owner/repo", `"omp"`, "no e2b_omp_template", "checks_template is unset") {
		t.Fatalf("failure does not name the runtime-dependent template gap: %+v", events)
	}
}

// The heartbeat producer, end to end: a review heartbeat of a configured
// repository enqueues through its production enqueuer and the worker routes it
// remote. A heartbeat review has no pull request, so remote admission refuses
// it loudly; it never runs locally.
func TestReviewChecksRoutingAppliesToHeartbeatReview(t *testing.T) {
	base := newRemoteReviewAdmissionFixture(t, runtime.ShellRuntime)
	appendReviewChecksConfig(t, base.paths, producerChecksE2B)
	var backends []execbackend.Backend
	base.worker.ExecutionBackendFactory = func(backend execbackend.Backend, _ config.RemoteExecConfig) (execbackend.ExecutionBackend, error) {
		backends = append(backends, backend)
		return base.backend, nil
	}
	now := time.Now().UTC()
	heartbeat := config.Heartbeat{
		Agent: "remote-review-agent", Name: "nightly", Enabled: true, Repo: "owner/repo",
		Interval: "1h", Jitter: "0s", Action: "review", Prompt: "review the repository", MaxConcurrent: 1,
	}
	if err := runOneHeartbeat(base.ctx, base.store, newHeartbeatEnqueuer(base.store, base.home), nil, heartbeat, base.home, now); err != nil {
		t.Fatal(err)
	}
	job, err := base.store.GetJob(base.ctx, heartbeatJobID(heartbeat.Agent, heartbeat.Name, now))
	if err != nil {
		t.Fatal(err)
	}
	if err := base.worker.run(base.ctx, job); err != nil {
		t.Fatal(err)
	}
	job, err = base.store.GetJob(base.ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["exec_backend"] != "remote" || payload["exec_template"] != "swift-tmpl" || payload["review_checks_routed"] != true {
		t.Fatalf("heartbeat review payload exec_backend=%v exec_template=%v review_checks_routed=%v; want remote, swift-tmpl, true",
			payload["exec_backend"], payload["exec_template"], payload["review_checks_routed"])
	}
	if job.State == string(workflow.JobSucceeded) {
		t.Fatalf("heartbeat review of a checks-routed repo succeeded without a remote provision (backends %v): it ran locally", backends)
	}
	for _, backend := range backends {
		if backend != execbackend.Remote {
			t.Fatalf("heartbeat review reached the %s backend", backend)
		}
	}
}

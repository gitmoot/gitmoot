package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/runtime"
)

// reviewChecksRoutingHome is a review-router home whose [remote_exec] configures
// the default E2B provider and sandboxd beside it, and whose repo (if
// non-empty) sets checks routing (#2316) to E2B with checks_template. The PR
// has NO CI checks, so the remote_routing_enabled gates would keep every
// review local.
func reviewChecksRoutingHome(t *testing.T, checksRepo string) (string, *db.Store, string) {
	t.Helper()
	section := ""
	if checksRepo != "" {
		section = "\n[repos.\"" + checksRepo + "\".review]\nchecks_backend = \"remote\"\nchecks_provider = \"e2b\"\nchecks_template = \"swift-tmpl\"\n"
	}
	return reviewChecksRoutingHomeWith(t, "e2b_template = \"base-tmpl\"\ne2b_omp_template = \"omp-tmpl\"\n", section)
}

// reviewChecksRoutingHomeWith is reviewChecksRoutingHome with the E2B
// provider's template keys and the appended config given verbatim.
func reviewChecksRoutingHomeWith(t *testing.T, e2bTemplates, extraConfig string) (string, *db.Store, string) {
	t.Helper()
	home, store, head := reviewRouterHome(t)
	paths := config.PathsForHome(home)
	keyFile := filepath.Join(home, "e2b.key")
	sandboxdKeyFile := filepath.Join(home, "sandboxd.key")
	for _, key := range []string{keyFile, sandboxdKeyFile} {
		if err := os.WriteFile(key, []byte("test-key-0123456789"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	extra := "\n[remote_exec]\ne2b_api_key_file = \"" + keyFile + "\"\n" + e2bTemplates +
		"\n[remote_exec.sandboxd]\napi_key_file = \"" + sandboxdKeyFile + "\"\ntemplate = \"review-arm64\"\nbase_url = \"https://sandboxd.example:8443\"\nenvd_base_url = \"https://sandboxd.example:8443\"\nmax_concurrent = 1\n" +
		extraConfig
	file, err := os.OpenFile(paths.ConfigFile, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(extra); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	client := &reviewRoutingFixtureClient{head: head, checks: []github.PullRequestCheck{}}
	previous := newAgentDispatchGitHubClient
	newAgentDispatchGitHubClient = func(string) github.Client { return client }
	t.Cleanup(func() { newAgentDispatchGitHubClient = previous })
	return home, store, head
}

func reviewChecksPayload(t *testing.T, store *db.Store, jobID string) map[string]any {
	t.Helper()
	job, err := store.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

func TestReviewChecksRoutingSendsConfiguredRepoRemoteWithTemplateWithoutCI(t *testing.T) {
	home, store, head := reviewChecksRoutingHome(t, "owner/repo")
	output, failure := runReviewRequestJSON(t,
		"--repo", "owner/repo", "--pr", "12", "--head", head,
		"--branch", "feature/review", "--role", "joltra", "--home", home,
		"--runtime", runtime.OmpRuntime, "--json",
	)
	if failure != "" {
		t.Fatal(failure)
	}
	payload := reviewChecksPayload(t, store, output.JobID)
	if payload["exec_backend"] != "remote" || payload["exec_template"] != "swift-tmpl" || payload["review_checks_routed"] != true {
		t.Fatalf("configured repo payload exec_backend=%v exec_template=%v review_checks_routed=%v; want remote, swift-tmpl, true",
			payload["exec_backend"], payload["exec_template"], payload["review_checks_routed"])
	}
	if provider, present := payload["exec_provider"]; present && provider != "" {
		t.Fatalf("e2b provider must be stored as absent, got %v", provider)
	}
	events, err := store.ListJobEvents(context.Background(), output.JobID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.Kind == "review_backend_route_selected" && strings.Contains(event.Message, "source=repo_checks") && strings.Contains(event.Message, "template=swift-tmpl") {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing repo_checks route event: %+v", events)
	}
}

func TestReviewChecksRoutingLeavesUnconfiguredRepoLocal(t *testing.T) {
	home, store, head := reviewChecksRoutingHome(t, "other/repo")
	output, failure := runReviewRequestJSON(t,
		"--repo", "owner/repo", "--pr", "12", "--head", head,
		"--branch", "feature/review", "--role", "joltra", "--home", home,
		"--runtime", runtime.OmpRuntime, "--json",
	)
	if failure != "" {
		t.Fatal(failure)
	}
	payload := reviewChecksPayload(t, store, output.JobID)
	for _, key := range []string{"exec_backend", "exec_template", "review_checks_routed", "exec_provider"} {
		if _, present := payload[key]; present {
			t.Fatalf("unconfigured repo payload carries %s=%v", key, payload[key])
		}
	}
}

func TestReviewChecksRoutingRefusesNonRemoteRuntime(t *testing.T) {
	home, _, head := reviewChecksRoutingHome(t, "owner/repo")
	_, failure := runReviewRequestJSON(t,
		"--repo", "owner/repo", "--pr", "12", "--head", head,
		"--branch", "feature/review", "--role", "joltra", "--home", home,
		"--runtime", runtime.ClaudeRuntime, "--json",
	)
	if failure == "" {
		t.Fatal("claude review of a checks-routed repo was dispatched; want refusal")
	}
	for _, want := range []string{"claude", "owner/repo", "cannot run remotely"} {
		if !strings.Contains(failure, want) {
			t.Fatalf("refusal %q does not name %q", failure, want)
		}
	}
}

func TestReviewChecksRoutingRefusesExplicitLocalBackend(t *testing.T) {
	home, _, head := reviewChecksRoutingHome(t, "owner/repo")
	_, failure := runReviewRequestJSON(t,
		"--repo", "owner/repo", "--pr", "12", "--head", head,
		"--branch", "feature/review", "--role", "joltra", "--home", home,
		"--runtime", runtime.OmpRuntime, "--exec-backend", "local", "--json",
	)
	if failure == "" {
		t.Fatal("--exec-backend local on a checks-routed repo was dispatched; want refusal")
	}
	if !strings.Contains(failure, "--exec-backend local refused") || !strings.Contains(failure, "checks_backend") {
		t.Fatalf("refusal %q does not explain the checks_backend setting", failure)
	}
}

// A review with no --exec-provider adopts checks_provider; before the fix the
// absent flag compared as "" != "sandboxd" and every such review was refused,
// attributed to an "--exec-provider e2b" nobody passed.
func TestReviewChecksRoutingSandboxdAdoptsProviderWithoutExecProvider(t *testing.T) {
	home, store, head := reviewChecksRoutingHomeWith(t, "e2b_template = \"base-tmpl\"\n",
		"\n[repos.\"owner/repo\".review]\nchecks_backend = \"remote\"\nchecks_provider = \"sandboxd\"\nchecks_template = \"swift-arm64\"\n")
	output, failure := runReviewRequestJSON(t,
		"--repo", "owner/repo", "--pr", "12", "--head", head,
		"--branch", "feature/review", "--role", "joltra", "--home", home,
		"--runtime", runtime.OmpRuntime, "--json",
	)
	if failure != "" {
		t.Fatal(failure)
	}
	payload := reviewChecksPayload(t, store, output.JobID)
	if payload["exec_backend"] != "remote" || payload["exec_provider"] != "sandboxd" || payload["exec_template"] != "swift-arm64" || payload["review_checks_routed"] != true {
		t.Fatalf("payload exec_backend=%v exec_provider=%v exec_template=%v review_checks_routed=%v; want remote, sandboxd, swift-arm64, true",
			payload["exec_backend"], payload["exec_provider"], payload["exec_template"], payload["review_checks_routed"])
	}
}

func TestReviewChecksRoutingRefusesDifferentExplicitProvider(t *testing.T) {
	home, _, head := reviewChecksRoutingHome(t, "owner/repo")
	_, failure := runReviewRequestJSON(t,
		"--repo", "owner/repo", "--pr", "12", "--head", head,
		"--branch", "feature/review", "--role", "joltra", "--home", home,
		"--runtime", runtime.OmpRuntime, "--exec-provider", "sandboxd", "--json",
	)
	if failure == "" {
		t.Fatal("--exec-provider sandboxd on an e2b checks-routed repo was dispatched; want refusal")
	}
	if !strings.Contains(failure, "--exec-provider sandboxd refused") || !strings.Contains(failure, `checks_provider \"e2b\"`) {
		t.Fatalf("refusal %q does not name the passed provider and checks_provider", failure)
	}
}

// Without checks_template the template follows the runtime: a provider with
// only e2b_omp_template has nothing for a shell review, and the dispatch says
// so instead of failing at provisioning.
func TestReviewChecksRoutingRefusesRuntimeWithoutTemplate(t *testing.T) {
	home, _, head := reviewChecksRoutingHomeWith(t, "e2b_omp_template = \"omp-tmpl\"\n",
		"\n[repos.\"owner/repo\".review]\nchecks_backend = \"remote\"\n")
	_, failure := runReviewRequestJSON(t,
		"--repo", "owner/repo", "--pr", "12", "--head", head,
		"--branch", "feature/review", "--role", "joltra", "--home", home,
		"--runtime", runtime.ShellRuntime, "--session", "true", "--json",
	)
	if failure == "" {
		t.Fatal("shell review on a provider with only e2b_omp_template was dispatched; want refusal")
	}
	for _, want := range []string{"owner/repo", `\"shell\"`, "only e2b_omp_template", "e2b_template or checks_template"} {
		if !strings.Contains(failure, want) {
			t.Fatalf("refusal %q does not name %q", failure, want)
		}
	}
}

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/execbackend"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// remoteProviderTestHome writes a home whose default remote provider is E2B
// and, when withMac is set, declares the opt-in Mac provider beside it.
func remoteProviderTestHome(t *testing.T, withMac bool) string {
	t.Helper()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	keys := map[string]string{}
	for _, name := range []string{"e2b", "mac"} {
		keys[name] = filepath.Join(home, name+"-api-key")
		if err := os.WriteFile(keys[name], []byte("private-"+name+"-key"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	body := fmt.Sprintf("\n[remote_exec]\nbackend = \"local\"\ne2b_api_key_file = %q\ne2b_template = \"base\"\ncost_max_reserved_usd = 9\ncost_per_attempt_usd = 4.5\ncost_max_concurrent = 2\n", keys["e2b"])
	if withMac {
		body += fmt.Sprintf("\n[remote_exec.mac]\napi_key_file = %q\ntemplate = \"review-arm64\"\nbase_url = \"https://mac.example:8443\"\nenvd_base_url = \"https://mac.example:8443\"\nmax_concurrent = 1\n", keys["mac"])
	}
	f, err := os.OpenFile(paths.ConfigFile, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(body); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return home
}

// providerSeenByProvision resolves job as the daemon does and records the
// configuration its execution backend would be constructed against.
func providerSeenByProvision(t *testing.T, worker jobWorker, job db.Job) config.RemoteExecConfig {
	t.Helper()
	payload, err := daemonJobPayload(job)
	if err != nil {
		t.Fatal(err)
	}
	backend, cfg, err := worker.resolveExecutionBackendForJob(job, payload)
	if err != nil || backend != execbackend.Remote {
		t.Fatalf("resolve %s: backend=%q err=%v", job.ID, backend, err)
	}
	var seen config.RemoteExecConfig
	stop := errors.New("stop before the provider is called")
	worker.ExecutionBackendFactory = func(_ execbackend.Backend, cfg config.RemoteExecConfig) (execbackend.ExecutionBackend, error) {
		seen = cfg
		return nil, stop
	}
	if _, _, _, _, err := worker.provisionExecutionBackend(context.Background(), backend, cfg, runtime.ShellRuntime, job, time.Minute, t.TempDir()); !errors.Is(err, stop) {
		t.Fatalf("provision %s: %v", job.ID, err)
	}
	return seen
}

// A job that opted into the Mac provider is provisioned against the Mac's
// control origin, key and template, while a remote job without the opt-in on
// the same home still provisions on E2B. A retry keeps the Mac opt-in.
func TestMacOptInProvisionsOnTheMacAndSurvivesRetry(t *testing.T) {
	ctx := context.Background()
	home := remoteProviderTestHome(t, true)
	store := openExecBackendLedgerTestStore(t)
	worker := jobWorker{Store: store, ConfigHome: home, ConfigHomeExplicit: true}
	jobs := map[string]string{
		"e2b-job": `{"repo":"o/r","exec_backend":"remote"}`,
		"mac-job": `{"repo":"o/r","exec_backend":"remote","exec_provider":"mac"}`,
	}
	for id, payload := range jobs {
		if err := store.CreateJobWithEvent(ctx, db.Job{ID: id, Agent: "a", Type: "implement", State: string(workflow.JobFailed), Payload: payload},
			db.JobEvent{Kind: string(workflow.JobFailed), Message: "seed"}); err != nil {
			t.Fatal(err)
		}
	}
	e2bJob, err := store.GetJob(ctx, "e2b-job")
	if err != nil {
		t.Fatal(err)
	}
	if cfg := providerSeenByProvision(t, worker, e2bJob); cfg.Provider != "e2b" || cfg.E2BBaseURL != "" || cfg.E2BTemplate != "base" || !strings.HasSuffix(cfg.E2BAPIKeyFile, "e2b-api-key") {
		t.Fatalf("default job provisioned with provider=%q base=%q template=%q key=%q, want E2B", cfg.Provider, cfg.E2BBaseURL, cfg.E2BTemplate, cfg.E2BAPIKeyFile)
	}
	retried, err := workflow.RetryJob(ctx, store, "mac-job")
	if err != nil {
		t.Fatal(err)
	}
	if retried.LifecycleGeneration == 0 {
		t.Fatalf("retry did not start a new generation: %+v", retried)
	}
	if cfg := providerSeenByProvision(t, worker, retried); cfg.Provider != "mac" || cfg.E2BBaseURL != "https://mac.example:8443" ||
		cfg.E2BEnvdBaseURL != "https://mac.example:8443" || cfg.E2BTemplate != "review-arm64" || !strings.HasSuffix(cfg.E2BAPIKeyFile, "mac-api-key") {
		t.Fatalf("retried Mac job provisioned with provider=%q base=%q envd=%q template=%q key=%q, want the Mac", cfg.Provider, cfg.E2BBaseURL, cfg.E2BEnvdBaseURL, cfg.E2BTemplate, cfg.E2BAPIKeyFile)
	}
}

// A request for a provider this home does not configure, or does not know,
// is refused before anything is enqueued instead of running on E2B.
func TestExecProviderRequestRefusesUnconfiguredAndUnknownProviders(t *testing.T) {
	if err := validateRequestExecProvider(remoteProviderTestHome(t, false), "mac"); err == nil || !strings.Contains(err.Error(), "[remote_exec.mac]") {
		t.Fatalf("Mac request on a home without [remote_exec.mac] = %v, want a refusal naming the section", err)
	}
	if err := validateRequestExecProvider(remoteProviderTestHome(t, true), "mac"); err != nil {
		t.Fatalf("configured Mac provider refused: %v", err)
	}
	if _, _, err := requestExecProvider("mac-studio", nil); err == nil || !strings.Contains(err.Error(), "unknown --exec-provider") {
		t.Fatalf("unknown provider accepted: %v", err)
	}
	local := "local"
	if _, _, err := requestExecProvider("mac", &local); err == nil {
		t.Fatal("--exec-provider mac accepted with --exec-backend local")
	}
}

package cli

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/credgw"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/execbackend"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/sandbox"
	"github.com/gitmoot/gitmoot/internal/subprocess"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// noExecLaunchDirFakeOmp is a static omp stand-in for a no-execution seat. It
// must be a binary, not a script: the no-execution sandbox lets the runtime
// itself start and nothing else, so a shebang interpreter would be refused.
// It reports the directory it started in, whether that directory is empty,
// and the project dir it was given, in its review summary.
const noExecLaunchDirFakeOmp = `package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println("omp/18.18.6")
		return
	}
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "error:" + err.Error()
	}
	entries, err := os.ReadDir(".")
	count := fmt.Sprint(len(entries))
	if err != nil {
		count = "error"
	}
	project := ""
	for _, arg := range os.Args[1:] {
		if value, ok := strings.CutPrefix(arg, "--cwd="); ok {
			project = value
		}
	}
	summary := fmt.Sprintf("launch=%s entries=%s project=%s", cwd, count, project)
	result, _ := json.Marshal(map[string]any{"gitmoot_result": map[string]any{
		"decision": "approved", "summary": summary, "findings": []any{}, "changes_made": []any{},
		"tests_run": []any{}, "needs": []any{}, "delegations": []any{},
	}})
	message, _ := json.Marshal(map[string]any{"type": "message_end", "message": map[string]any{
		"role": "assistant", "content": []any{map[string]any{"type": "text", "text": string(result)}},
		"usage": map[string]any{"input": 1, "output": 1, "totalTokens": 2}, "stopReason": "stop",
	}})
	fmt.Println(` + "`" + `{"type":"session","version":3,"id":"01920000-0000-7000-8000-000000002563","timestamp":"2026-11-10T00:00:00.000Z","cwd":"/repo"}` + "`" + `)
	fmt.Println(string(message))
	fmt.Println(` + "`" + `{"type":"agent_end","messages":[],"isTerminal":true}` + "`" + `)
}
`

func buildNoExecLaunchDirFakeOmp(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "go.mod"), []byte("module fakeomp\n\ngo 1.26\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "main.go"), []byte(noExecLaunchDirFakeOmp), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	build := exec.Command("go", "build", "-buildvcs=false", "-o", filepath.Join(bin, "omp"), ".")
	build.Dir = src
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOFLAGS=")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fake omp: %v\n%s", err, output)
	}
	return bin
}

type noExecLaunchDirCI struct{}

func (noExecLaunchDirCI) ListCheckRunsForRef(context.Context, github.Repository, string) ([]github.PullRequestCheck, error) {
	return nil, nil
}

func (noExecLaunchDirCI) ActionsJobLog(context.Context, github.Repository, int64) (string, error) {
	return "", nil
}

// TestNoExecutionReviewStartsOnLocalExecutionBackendInstance is #2563: every
// no-execution review on the live daemon failed because the local execution
// backend refused the seat's launch dir as escaping the instance workspace.
// This runs the worker's real no-execution setup on the real local backend
// with a fake omp, and requires the seat to start in the empty, job-private
// launch dir outside the workspace while omp is pointed at the workspace.
func TestNoExecutionReviewStartsOnLocalExecutionBackendInstance(t *testing.T) {
	requireReadOnlySeatLaunchable(t)
	if err := sandbox.NoExecutionSupported(); err != nil {
		t.Skipf("no-execution sandbox unsupported here: %v", err)
	}
	ctx := context.Background()
	t.Setenv("PATH", buildNoExecLaunchDirFakeOmp(t)+string(os.PathListSeparator)+os.Getenv("PATH"))
	tokenFile := filepath.Join(t.TempDir(), "omp-broker-token")
	if err := os.WriteFile(tokenFile, []byte("seat-broker-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OMP_AUTH_BROKER_URL", "http://127.0.0.1:9")
	t.Setenv("OMP_AUTH_BROKER_TOKEN", "")
	t.Setenv("OMP_AUTH_BROKER_TOKEN_FILE", tokenFile)

	store, home := blockerE2EHome(t)
	paths, err := pathsFromFlag(home)
	if err != nil {
		t.Fatal(err)
	}
	existing, _ := os.ReadFile(paths.ConfigFile)
	if err := os.WriteFile(paths.ConfigFile, append(existing, []byte("\n[repos.\"owner/repo\".review]\nexecution = \"none\"\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	checkout := readonlyWorktreeGitCheckout(t, "owner/repo")
	// The project's .env is exactly what the neutral launch dir keeps omp from
	// loading; it travels into the instance workspace with the checkout.
	if err := os.WriteFile(filepath.Join(checkout, ".env"), []byte("PROJECT_SECRET=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgentWithPolicy(t, store, "omp-reviewer", runtime.OmpRuntime, "", []string{"review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "noexec-local-backend", Agent: "omp-reviewer", Action: "review", Repo: "owner/repo",
		WorktreePath: checkout, ReadOnlySeat: true, Model: "anthropic/claude-sonnet-4-5",
	})

	worker := executionBackendJobWorker(store, io.Discard, home)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	worker.RuntimePreflight = func(context.Context, runtime.Agent, runtime.RuntimeContractRequest) runtime.RuntimeContractResult {
		return runtime.RuntimeContractResult{Runtime: runtime.OmpRuntime, Binary: "omp", Version: "omp/18.18.6", State: runtime.RuntimeContractSupported}
	}
	worker.StaticReviewCIGitHubFactory = func(string) staticReviewCIGitHub { return noExecLaunchDirCI{} }
	job, err := store.GetJob(ctx, "noexec-local-backend")
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.run(ctx, job); err != nil {
		t.Fatalf("worker.run: %v", err)
	}

	job, err = store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	events, err := store.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != string(workflow.JobSucceeded) || payload.Result == nil {
		t.Fatalf("job state = %q, want succeeded; events=%+v", job.State, events)
	}
	fields := map[string]string{}
	for _, field := range strings.Fields(payload.Result.Summary) {
		if name, value, ok := strings.Cut(field, "="); ok {
			fields[name] = value
		}
	}
	launch, project := fields["launch"], fields["project"]
	if project == "" || !strings.Contains(project, string(filepath.Separator)+"execbackends"+string(filepath.Separator)) {
		t.Fatalf("omp project dir = %q, want the local execution instance workspace; summary=%q", project, payload.Result.Summary)
	}
	if launch == "" || filepath.Base(launch) != "launch" || fields["entries"] != "0" {
		t.Fatalf("omp started in %q with %s entries, want the empty seat launch dir; summary=%q", launch, fields["entries"], payload.Result.Summary)
	}
	if localPathWithinTest(project, launch) || localPathWithinTest(launch, project) || localPathWithinTest(checkout, launch) || localPathWithinTest(launch, checkout) {
		t.Fatalf("launch dir %q is not disjoint from workspace %q and checkout %q", launch, project, checkout)
	}
}

// TestWithExecutionInstanceLaunchDirReachesTheInstanceRunner: the launch dir
// must land on the instance runner under every wrapper a delivery runner can
// carry, without mutating a runner the worker still holds.
func TestWithExecutionInstanceLaunchDirReachesTheInstanceRunner(t *testing.T) {
	const launchDir = "/seat/launch"
	shared := &execbackend.InstanceRunner{Instance: &execbackend.Instance{ID: "i"}}
	shapes := map[string]subprocess.Runner{
		"value":          execbackend.InstanceRunner{Instance: &execbackend.Instance{ID: "i"}},
		"pointer":        shared,
		"tee":            subprocess.TeeRunner{Inner: shared},
		"tee pointer":    &subprocess.TeeRunner{Inner: shared},
		"env":            subprocess.EnvInjectingRunner{Inner: shared},
		"env pointer":    &subprocess.EnvInjectingRunner{Inner: shared},
		"wrapping":       subprocess.WrappingRunner{Inner: shared},
		"credential gw":  &credgw.Runner{Inner: subprocess.EnvInjectingRunner{Inner: shared}},
		"tee env nested": subprocess.TeeRunner{Inner: subprocess.EnvInjectingRunner{Inner: shared}},
	}
	for name, runner := range shapes {
		got := instanceRunnerLaunchDir(withExecutionInstanceLaunchDir(runner, launchDir))
		if got != launchDir {
			t.Errorf("%s: instance runner launch dir = %q, want %q", name, got, launchDir)
		}
	}
	if shared.LaunchDir != "" {
		t.Fatalf("shared instance runner was mutated: LaunchDir = %q", shared.LaunchDir)
	}
	host := subprocess.GroupRunner{}
	if got := withExecutionInstanceLaunchDir(host, launchDir); got != host {
		t.Fatalf("host runner changed to %#v", got)
	}
}

func instanceRunnerLaunchDir(runner subprocess.Runner) string {
	switch r := runner.(type) {
	case execbackend.InstanceRunner:
		return r.LaunchDir
	case *execbackend.InstanceRunner:
		return r.LaunchDir
	case subprocess.TeeRunner:
		return instanceRunnerLaunchDir(r.Inner)
	case *subprocess.TeeRunner:
		return instanceRunnerLaunchDir(r.Inner)
	case subprocess.EnvInjectingRunner:
		return instanceRunnerLaunchDir(r.Inner)
	case *subprocess.EnvInjectingRunner:
		return instanceRunnerLaunchDir(r.Inner)
	case subprocess.WrappingRunner:
		return instanceRunnerLaunchDir(r.Inner)
	case *credgw.Runner:
		return instanceRunnerLaunchDir(r.Inner)
	default:
		return ""
	}
}

func localPathWithinTest(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

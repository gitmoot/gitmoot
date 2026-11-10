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
//
// It tries to create a file in the directory it started in, then reports that
// directory, how many entries it holds, whether the write landed, the project
// dir it was given and which delivery of the job this is. The first delivery
// answers without a gitmoot_result so the mailbox runs a repair turn; the
// delivery count lives in the seat's TMPDIR, which persists for the job.
const noExecLaunchDirFakeOmp = `package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println("omp/18.18.6")
		return
	}
	counter := filepath.Join(os.TempDir(), "fake-omp-deliveries")
	raw, _ := os.ReadFile(counter)
	delivery, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	delivery++
	counterState := "ok"
	if err := os.WriteFile(counter, []byte(strconv.Itoa(delivery)), 0o600); err != nil {
		counterState = "error"
	}
	cwdWrite := "denied"
	if err := os.WriteFile("omp-wrote-here", []byte("x"), 0o600); err == nil {
		cwdWrite = "ok"
	}
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "error"
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
	text := "no structured result on the first delivery"
	if delivery > 1 {
		summary := fmt.Sprintf("launch=%s entries=%s cwdwrite=%s project=%s delivery=%d counter=%s", cwd, count, cwdWrite, project, delivery, counterState)
		result, _ := json.Marshal(map[string]any{"gitmoot_result": map[string]any{
			"decision": "approved", "summary": summary, "findings": []any{}, "changes_made": []any{},
			"tests_run": []any{}, "needs": []any{}, "delegations": []any{},
		}})
		text = string(result)
	}
	message, _ := json.Marshal(map[string]any{"type": "message_end", "message": map[string]any{
		"role": "assistant", "content": []any{map[string]any{"type": "text", "text": text}},
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
// with a fake omp, and requires the seat to start in an empty launch dir that
// is outside the workspace, the checkout and every seat-writable path, while
// omp is pointed at the workspace. The seat cannot write its launch dir, a
// repair turn (a second delivery in the same job) starts there too, and the
// dir is gone when the job ends.
func TestNoExecutionReviewStartsOnLocalExecutionBackendInstance(t *testing.T) {
	if abi, err := sandbox.ABI(); err != nil || abi < sandbox.MinimumABI {
		t.Skipf("no-execution seat needs Landlock ABI v%d; this kernel reports v%d (%v)", sandbox.MinimumABI, abi, err)
	}
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
	summary := payload.Result.Summary
	if fields["delivery"] != "2" || fields["counter"] != "ok" {
		t.Fatalf("result came from delivery %q (counter %q), want the repair turn, delivery 2; summary=%q", fields["delivery"], fields["counter"], summary)
	}
	launch, project := fields["launch"], fields["project"]
	if project == "" || !strings.Contains(project, string(filepath.Separator)+"execbackends"+string(filepath.Separator)) {
		t.Fatalf("omp project dir = %q, want the local execution instance workspace; summary=%q", project, summary)
	}
	if launch == "" || fields["entries"] != "0" {
		t.Fatalf("omp started in %q with %s entries, want an empty launch dir; summary=%q", launch, fields["entries"], summary)
	}
	if fields["cwdwrite"] != "denied" {
		t.Fatalf("the seat could write its launch dir (cwdwrite=%q); summary=%q", fields["cwdwrite"], summary)
	}
	if got, want := filepath.Dir(launch), noExecutionLaunchParent(paths); got != want {
		t.Fatalf("launch dir %q is not in the daemon-only parent %q", launch, want)
	}
	for _, other := range []string{project, checkout, filepath.Join(paths.Home, "cache")} {
		if localPathWithinTest(other, launch) || localPathWithinTest(launch, other) {
			t.Fatalf("launch dir %q overlaps %q", launch, other)
		}
	}
	if _, err := os.Lstat(launch); !os.IsNotExist(err) {
		t.Fatalf("launch dir %q survived the job: stat err = %v", launch, err)
	}
}

func localPathWithinTest(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// TestCreateNoExecutionLaunchDirRefusesSeatWritableParent: the launch dir's
// parent must be outside every path the seat may write, or the seat could
// fill or swap the dir before the runtime starts in it (#2563 review).
func TestCreateNoExecutionLaunchDirRefusesSeatWritableParent(t *testing.T) {
	home := t.TempDir()
	paths, err := pathsFromFlag(home)
	if err != nil {
		t.Fatal(err)
	}
	parent := noExecutionLaunchParent(paths)
	for name, writes := range map[string][]string{
		"parent itself":     {parent},
		"gitmoot home":      {paths.Home},
		"run dir":           {filepath.Dir(parent)},
		"inside the parent": {filepath.Join(parent, "seat-x")},
	} {
		t.Run(name, func(t *testing.T) {
			dir, err := createNoExecutionLaunchDir(home, writes)
			if err == nil || !strings.Contains(err.Error(), "overlaps seat-writable path") {
				t.Fatalf("createNoExecutionLaunchDir(%v) = %q, %v; want an overlap refusal", writes, dir, err)
			}
		})
	}

	// An ordinary seat grant (its cache root) is disjoint: the dir is made,
	// empty and private, in a private daemon-owned parent, and a second call
	// (a second job) gets its own dir.
	if err := os.MkdirAll(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	writes := []string{filepath.Join(paths.Home, "cache", "tools", "read-only", "abc")}
	first, err := createNoExecutionLaunchDir(home, writes)
	if err != nil {
		t.Fatal(err)
	}
	second, err := createNoExecutionLaunchDir(home, writes)
	if err != nil {
		t.Fatal(err)
	}
	if first == second || filepath.Dir(first) != parent || filepath.Dir(second) != parent {
		t.Fatalf("launch dirs %q and %q, want two distinct dirs in %q", first, second, parent)
	}
	for _, dir := range []string{parent, first} {
		info, err := os.Lstat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode() != os.ModeDir|0o700 {
			t.Fatalf("%s mode = %s, want drwx------", dir, info.Mode())
		}
	}
	entries, err := os.ReadDir(first)
	if err != nil || len(entries) != 0 {
		t.Fatalf("launch dir entries = %v, %v; want empty", entries, err)
	}
}

// TestGraftRuntimeBaseRunnerSetsLaunchDirOnTheInstanceRunner: the launch dir
// reaches the execution-backend instance runner through the same graft that
// gives it the curated environment, under every wrapper a delivery runner can
// carry.
func TestGraftRuntimeBaseRunnerSetsLaunchDirOnTheInstanceRunner(t *testing.T) {
	const launchDir = "/seat/launch"
	instance := &execbackend.Instance{ID: "i"}
	shapes := map[string]func() subprocess.Runner{
		"value":   func() subprocess.Runner { return execbackend.InstanceRunner{Instance: instance} },
		"pointer": func() subprocess.Runner { return &execbackend.InstanceRunner{Instance: instance} },
		"tee": func() subprocess.Runner {
			return subprocess.TeeRunner{Inner: execbackend.InstanceRunner{Instance: instance}}
		},
		"tee pointer": func() subprocess.Runner {
			return &subprocess.TeeRunner{Inner: execbackend.InstanceRunner{Instance: instance}}
		},
		"env": func() subprocess.Runner {
			return subprocess.EnvInjectingRunner{Inner: execbackend.InstanceRunner{Instance: instance}}
		},
		"env pointer": func() subprocess.Runner {
			return &subprocess.EnvInjectingRunner{Inner: execbackend.InstanceRunner{Instance: instance}}
		},
		"wrapping": func() subprocess.Runner {
			return subprocess.WrappingRunner{Inner: execbackend.InstanceRunner{Instance: instance}}
		},
		"credential gw": func() subprocess.Runner { return &credgw.Runner{Inner: execbackend.InstanceRunner{Instance: instance}} },
		"tee env nested": func() subprocess.Runner {
			return subprocess.TeeRunner{Inner: subprocess.EnvInjectingRunner{Inner: execbackend.InstanceRunner{Instance: instance}}}
		},
	}
	curated := subprocess.CuratedGroupRunner{BaseEnv: []string{"CURATED=yes"}}
	for name, shape := range shapes {
		grafted := graftRuntimeBaseRunner(shape(), curated, launchDir)
		got, env := instanceRunnerLaunchDir(grafted)
		if got != launchDir || len(env) != 1 || env[0] != "CURATED=yes" {
			t.Errorf("%s: instance runner launch dir = %q base env = %v, want %q and the curated env", name, got, env, launchDir)
		}
		if got, _ := instanceRunnerLaunchDir(graftRuntimeBaseRunner(shape(), curated, "")); got != "" {
			t.Errorf("%s: an ordinary seat named launch dir %q", name, got)
		}
	}
}

func instanceRunnerLaunchDir(runner subprocess.Runner) (string, []string) {
	switch r := runner.(type) {
	case execbackend.InstanceRunner:
		return r.LaunchDir, r.BaseEnv
	case *execbackend.InstanceRunner:
		return r.LaunchDir, r.BaseEnv
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
		return "", nil
	}
}

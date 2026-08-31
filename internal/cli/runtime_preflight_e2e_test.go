package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/db/dbtest"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/subprocess"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestRuntimePreflightStubBinaryE2E(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	stub := filepath.Join(binDir, "kimi")
	writeRuntimePreflightStub(t, stub, "supported")
	checker := runtime.NewRuntimeContractChecker(subprocess.GroupRunner{}, runtime.BuiltinRuntimeRegistry())

	t.Run("arm1_supported_dispatches", func(t *testing.T) {
		job, events := runRuntimePreflightStubJob(t, filepath.Join(root, "arm1-home"), checker, "arm1")
		if job.State != string(workflow.JobSucceeded) {
			t.Fatalf("job state = %q, want succeeded; events=%+v payload=%s", job.State, events, job.Payload)
		}
		if daemonWorkerHasEvent(events, "runtime_contract_unknown") {
			t.Fatalf("supported dispatch emitted unknown: %+v", events)
		}
	})

	t.Run("arm4_changed_identity_reprobes_and_blocks", func(t *testing.T) {
		writeRuntimePreflightStub(t, stub, "unsupported-and-longer")
		job, events := runRuntimePreflightStubJob(t, filepath.Join(root, "arm4-home"), checker, "arm4")
		if job.State != string(workflow.JobBlocked) {
			t.Fatalf("job state = %q, want blocked", job.State)
		}
		assertRuntimePreflightEventText(t, events, "--print", "kimi-code stub-0.29.2", "remedy")
	})

	t.Run("arm2_unsupported_blocks", func(t *testing.T) {
		otherDir := filepath.Join(root, "unsupported-bin")
		if err := os.MkdirAll(otherDir, 0o755); err != nil {
			t.Fatal(err)
		}
		otherStub := filepath.Join(otherDir, "kimi")
		writeRuntimePreflightStub(t, otherStub, "unsupported")
		t.Setenv("PATH", otherDir+string(os.PathListSeparator)+os.Getenv("PATH"))
		job, events := runRuntimePreflightStubJob(t, filepath.Join(root, "arm2-home"), checker, "arm2")
		if job.State != string(workflow.JobBlocked) {
			t.Fatalf("job state = %q, want blocked", job.State)
		}
		assertRuntimePreflightEventText(t, events, "--print", "kimi-code stub-0.29.2", "remedy")
	})

	t.Run("arm3_unparseable_dispatches_and_records", func(t *testing.T) {
		otherDir := filepath.Join(root, "unknown-bin")
		if err := os.MkdirAll(otherDir, 0o755); err != nil {
			t.Fatal(err)
		}
		otherStub := filepath.Join(otherDir, "kimi")
		writeRuntimePreflightStub(t, otherStub, "unparseable")
		t.Setenv("PATH", otherDir+string(os.PathListSeparator)+os.Getenv("PATH"))
		job, events := runRuntimePreflightStubJob(t, filepath.Join(root, "arm3-home"), checker, "arm3")
		if job.State != string(workflow.JobSucceeded) {
			t.Fatalf("job state = %q, want succeeded; events=%+v payload=%s", job.State, events, job.Payload)
		}
		if !daemonWorkerHasEvent(events, "runtime_contract_unknown") {
			t.Fatalf("events = %+v, want runtime_contract_unknown", events)
		}
	})
}

func TestRuntimePreflightDefaultWorkerDoesNotAssumeConfiguredExecutionIdentity(t *testing.T) {
	rawHome := t.TempDir()
	paths, err := pathsFromFlag(rawHome)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.Home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte("[remote_exec]\nbackend = \"local\"\nlocal_uid = 996\nlocal_gid = 986\nlocal_root = \"/var/tmp/gitmoot-local\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	checkout := createDaemonWorkerGitCheckout(t, "host-only-preflight")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgentWithPolicy(t, store, "root-claude", runtime.ClaudeRuntime, "fresh:host-only", []string{"implement"}, "owner/repo", runtime.AutonomyPolicyDangerFullAccess)
	const jobID = "host-only-preflight"
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: jobID, Agent: "root-claude", Action: "implement", Repo: "owner/repo", Branch: "main"})

	preflightCalls := 0
	worker := defaultJobWorker(store, io.Discard, rawHome)
	worker.RuntimePreflight = func(_ context.Context, agent runtime.Agent, request runtime.RuntimeContractRequest) runtime.RuntimeContractResult {
		preflightCalls++
		if request.EffectiveUIDKnown {
			t.Fatalf("host-only worker preflight received execution uid %d", request.EffectiveUID)
		}
		return runtime.RuntimeContractResult{
			Runtime: agent.Runtime, State: runtime.RuntimeContractUnsupported, Instrument: "effective-uid",
			Requirements: []runtime.RuntimeRequirementResult{{
				Kind: runtime.RuntimeRequirementNonRootEUID, Name: "effective uid must be non-root",
				Remedy: "run as non-root", State: runtime.RuntimeContractUnsupported, Instrument: "effective-uid",
			}},
		}
	}
	job, err := store.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.run(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	completed, err := store.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	if preflightCalls != 1 || completed.State != string(workflow.JobBlocked) {
		t.Fatalf("host-only preflight calls = %d, job state = %q; want one call and blocked", preflightCalls, completed.State)
	}
}

func writeRuntimePreflightStub(t *testing.T, path, mode string) {
	t.Helper()
	help := "Usage: kimi [options]\\n  --print\\n  -p, --prompt\\n  --output-format\\n"
	if strings.Contains(mode, "unsupported") {
		help = "Usage: kimi [options]\\n  --prompt\\n  --output-format\\n"
	} else if mode == "unparseable" {
		help = "???\\n"
	}
	script := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"  --help) printf '" + help + "' ;;\n" +
		"  --version) printf 'kimi-code stub-0.29.2\\n' ;;\n" +
		"  *) printf '%s\\n' '{\"role\":\"assistant\",\"content\":\"{\\\"gitmoot_result\\\":{\\\"decision\\\":\\\"approved\\\",\\\"summary\\\":\\\"stub completed\\\",\\\"findings\\\":[],\\\"changes_made\\\":[],\\\"tests_run\\\":[],\\\"needs\\\":[],\\\"delegations\\\":[]}}\"}' ;;\n" +
		"esac\n# " + mode + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().Add(time.Duration(len(script)) * time.Second)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
}

func runRuntimePreflightStubJob(t *testing.T, rawHome string, checker *runtime.RuntimeContractChecker, jobID string) (db.Job, []db.JobEvent) {
	t.Helper()
	ctx := context.Background()
	paths, err := pathsFromFlag(rawHome)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.Home, 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	checkout := filepath.Join(rawHome, "checkout")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "legacy", runtime.KimiCLIRuntime, "fresh:e2e", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: jobID, Agent: "legacy", Action: "ask", Repo: "owner/repo", Branch: "main"})
	worker := defaultJobWorker(store, io.Discard, rawHome)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	worker.RuntimePreflight = checker.CheckRequest
	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("run queued job: %v", err)
	}
	job, err := store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	events, err := store.ListJobEvents(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	return job, events
}

func assertRuntimePreflightEventText(t *testing.T, events []db.JobEvent, wants ...string) {
	t.Helper()
	joined := ""
	for _, event := range events {
		joined += event.Message + "\n"
	}
	for _, want := range wants {
		if !strings.Contains(joined, want) {
			t.Fatalf("events do not contain %q: %+v", want, events)
		}
	}
}

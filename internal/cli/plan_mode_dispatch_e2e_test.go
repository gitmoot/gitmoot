package cli

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/db/dbtest"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestPlanModeDaemonDispatchE2E(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writePlanModeOmpStub(t, filepath.Join(binDir, "omp"))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	t.Run("explicit execution model reaches argv and evidence", func(t *testing.T) {
		argvFile := filepath.Join(root, "explicit-argv")
		t.Setenv("GITMOOT_OMP_ARGV_FILE", argvFile)
		job, payload, events := runPlanModeDaemonJob(t, filepath.Join(root, "explicit-home"), "plan-explicit", runtime.OmpRuntime,
			"fresh:plan-e2e", workflow.JobRequest{
				Plan:     true,
				PlanInto: "executor/model",
				Model:    "planner/model",
			})
		if job.State != string(workflow.JobSucceeded) {
			t.Fatalf("job state = %q, want succeeded; events=%+v payload=%s", job.State, events, job.Payload)
		}
		argv := readPlanModeStubArgv(t, argvFile)
		assertPlanModeArgvPair(t, argv, "--plan-yolo", "--plan-yolo-into", "executor/model")
		assertPlanModeArgvPair(t, argv, "--model", "planner/model")
		if payload.PlanMode != "plan-into:executor/model" {
			t.Fatalf("persisted plan_mode = %q, want %q; argv=%v", payload.PlanMode, "plan-into:executor/model", argv)
		}
	})

	t.Run("runtime default remains explicit evidence", func(t *testing.T) {
		argvFile := filepath.Join(root, "default-argv")
		t.Setenv("GITMOOT_OMP_ARGV_FILE", argvFile)
		job, payload, events := runPlanModeDaemonJob(t, filepath.Join(root, "default-home"), "plan-default", runtime.OmpRuntime,
			"fresh:plan-e2e", workflow.JobRequest{Plan: true})
		if job.State != string(workflow.JobSucceeded) {
			t.Fatalf("job state = %q, want succeeded; events=%+v payload=%s", job.State, events, job.Payload)
		}
		argv := readPlanModeStubArgv(t, argvFile)
		if !slices.Contains(argv, "--plan-yolo") {
			t.Fatalf("stub argv = %v, want --plan-yolo", argv)
		}
		if slices.Contains(argv, "--plan-yolo-into") {
			t.Fatalf("stub argv = %v, targetless plan must leave execution model to omp", argv)
		}
		if payload.PlanMode != "plan-into:<runtime-default>" {
			t.Fatalf("persisted plan_mode = %q, want %q; argv=%v", payload.PlanMode, "plan-into:<runtime-default>", argv)
		}
	})

	t.Run("unsupported explicit runtime override is rejected before enqueue", func(t *testing.T) {
		ctx := context.Background()
		rawHome := filepath.Join(root, "override-home")
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
		seedDaemonWorkerAgentWithPolicy(t, store, "planner", runtime.OmpRuntime, "fresh:plan-e2e", []string{"ask"}, "owner/repo", runtime.AutonomyPolicyWorkspaceWrite)

		const jobID = "plan-shell-override"
		_, err = (workflow.Mailbox{Store: store}).Enqueue(ctx, workflow.JobRequest{
			ID:                 jobID,
			Agent:              "planner",
			Action:             "ask",
			Repo:               "owner/repo",
			Branch:             "main",
			Plan:               true,
			RuntimeOverride:    runtime.ShellRuntime,
			RuntimeOverrideRef: "true",
		})
		if err == nil {
			t.Fatal("enqueue accepted a plan job with an unsupported shell runtime override")
		}
		for _, want := range []string{`runtime "shell" cannot honour plan mode`, "only the omp runtime implements it"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("enqueue error %q does not contain %q", err, want)
			}
		}
		if _, getErr := store.GetJob(ctx, jobID); !errors.Is(getErr, sql.ErrNoRows) {
			t.Fatalf("rejected override created a job row: GetJob error = %v, want sql.ErrNoRows", getErr)
		}
	})

	t.Run("unsupported runtime fails loudly without dispatch", func(t *testing.T) {
		marker := filepath.Join(root, "shell-ran")
		script := "touch " + marker + "; printf '%s\\n' '{\"gitmoot_result\":{\"decision\":\"approved\",\"summary\":\"silently downgraded\",\"findings\":[],\"changes_made\":[],\"tests_run\":[],\"needs\":[],\"delegations\":[]}}'"
		job, payload, events := runPlanModeDaemonJob(t, filepath.Join(root, "unsupported-home"), "plan-shell", runtime.ShellRuntime,
			script, workflow.JobRequest{Plan: true})
		if job.State != string(workflow.JobFailed) {
			t.Fatalf("job state = %q, want failed; events=%+v payload=%s", job.State, events, job.Payload)
		}
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("unsupported runtime command ran despite plan gate: stat err=%v", err)
		}
		failure := planModeE2EEventText(events)
		for _, want := range []string{`runtime "shell" cannot honour plan mode`, "only the omp runtime implements it"} {
			if !strings.Contains(failure, want) {
				t.Fatalf("terminal events do not contain %q: %+v", want, events)
			}
		}
		if payload.PlanMode != "" {
			t.Fatalf("refused job persisted plan_mode = %q, want empty because no runtime ran", payload.PlanMode)
		}
	})
}

func writePlanModeOmpStub(t *testing.T, path string) {
	t.Helper()
	const script = `#!/bin/sh
case "$1" in
  --help)
    cat <<'EOF'
Usage: omp [options]
  -p
  --mode
  --approval-mode
  --no-session
  --plan-yolo
  --plan-yolo-into
EOF
    ;;
  --version)
    printf 'omp stub-17.2.4\n'
    ;;
  *)
    printf '%s\n' "$@" > "$GITMOOT_OMP_ARGV_FILE"
    cat <<'EOF'
{"type":"session","version":3,"id":"01920000-0000-7000-8000-000000000151","timestamp":"2026-08-15T00:00:00.000Z","cwd":"/repo"}
{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"{\"gitmoot_result\":{\"decision\":\"approved\",\"summary\":\"stub completed\",\"findings\":[],\"changes_made\":[],\"tests_run\":[],\"needs\":[],\"delegations\":[]}}"}],"usage":{"input":1,"output":1,"totalTokens":2},"stopReason":"stop"}}
{"type":"agent_end","messages":[],"isTerminal":true}
EOF
    ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func runPlanModeDaemonJob(t *testing.T, rawHome, jobID, runtimeName, runtimeRef string, request workflow.JobRequest) (db.Job, workflow.JobPayload, []db.JobEvent) {
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
	seedDaemonWorkerAgentWithPolicy(t, store, "planner", runtimeName, runtimeRef, []string{"ask"}, "owner/repo", runtime.AutonomyPolicyWorkspaceWrite)
	request.ID = jobID
	request.Agent = "planner"
	request.Action = "ask"
	request.Repo = "owner/repo"
	request.Branch = "main"
	enqueueDaemonWorkerJob(t, store, request)

	worker := defaultJobWorker(store, io.Discard, rawHome)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("run queued job: %v", err)
	}
	job, err := store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		t.Fatal(err)
	}
	events, err := store.ListJobEvents(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	return job, payload, events
}

func readPlanModeStubArgv(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read stub argv: %v", err)
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
}

func assertPlanModeArgvPair(t *testing.T, argv []string, want ...string) {
	t.Helper()
	for start := 0; start+len(want) <= len(argv); start++ {
		if slices.Equal(argv[start:start+len(want)], want) {
			return
		}
	}
	t.Fatalf("stub argv = %v, want contiguous %v", argv, want)
}

func planModeE2EEventText(events []db.JobEvent) string {
	messages := make([]string, 0, len(events))
	for _, event := range events {
		messages = append(messages, event.Message)
	}
	return strings.Join(messages, "\n")
}

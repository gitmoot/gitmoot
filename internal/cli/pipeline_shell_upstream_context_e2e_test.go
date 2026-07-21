package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/pipeline"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

const maxPipelineShellUpstreamSummaryBytes = 16 * 1024

type pipelineShellUpstreamContext struct {
	SchemaVersion int                                   `json:"schema_version"`
	Complete      bool                                  `json:"complete"`
	Stages        map[string]pipelineShellUpstreamStage `json:"stages"`
}

type pipelineShellUpstreamStage struct {
	ID               string `json:"id"`
	State            string `json:"state"`
	Summary          string `json:"summary"`
	SummaryTruncated bool   `json:"summary_truncated"`
}

func TestPipelineShellStageUpstreamContextE2E(t *testing.T) {
	const extractSummary = "arxiv record one\narxiv record two with `ticks` and \"quotes\""
	const downstreamSummary = "round-trip: arxiv record one\narxiv record two with `ticks` and \"quotes\""
	runID, store := runPipelineShellUpstreamContextE2E(
		t,
		"shell-context-flow",
		extractSummary,
		[]string{
			`"schema_version":1`,
			`"complete":true`,
			`"state":"succeeded"`,
			`"summary_truncated":false`,
			jsonString(t, extractSummary),
		},
		downstreamSummary,
	)

	consume := stageRow(t, store, runID, "consume")
	if consume.State != pipeline.StageSucceeded || consume.Summary != downstreamSummary {
		t.Fatalf("consume state/summary = %s/%q, want succeeded/%q", consume.State, consume.Summary, downstreamSummary)
	}
	job, err := store.GetJob(context.Background(), consume.JobID)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := workflow.ParseJobPayload(job.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(payload.ShellUpstreamContext, jsonString(t, extractSummary)) {
		t.Fatalf("persisted shell context lost multiline summary: %s", payload.ShellUpstreamContext)
	}
}

func TestPipelineShellStageUpstreamContextTruncationE2E(t *testing.T) {
	oversize := strings.Repeat("界", maxPipelineShellUpstreamSummaryBytes)
	runID, store := runPipelineShellUpstreamContextE2E(
		t,
		"shell-context-truncated",
		oversize,
		[]string{`"schema_version":1`, `"complete":false`, `"state":"succeeded"`, `"summary_truncated":true`},
		"saw explicit truncation",
	)

	consume := stageRow(t, store, runID, "consume")
	job, err := store.GetJob(context.Background(), consume.JobID)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := workflow.ParseJobPayload(job.Payload)
	if err != nil {
		t.Fatal(err)
	}
	var decoded pipelineShellUpstreamContext
	if err := json.Unmarshal([]byte(payload.ShellUpstreamContext), &decoded); err != nil {
		t.Fatal(err)
	}
	source := decoded.Stages["source"]
	if decoded.Complete || !source.SummaryTruncated || len(source.Summary) > maxPipelineShellUpstreamSummaryBytes {
		t.Fatalf("truncation context = complete:%v truncated:%v summary-bytes:%d", decoded.Complete, source.SummaryTruncated, len(source.Summary))
	}
}

func runPipelineShellUpstreamContextE2E(t *testing.T, name, sourceSummary string, requiredFragments []string, downstreamSummary string) (string, *db.Store) {
	t.Helper()
	ctx := context.Background()
	home, _, store := heartbeatLoopE2EHome(t)
	checkout := createDaemonWorkerGitCheckout(t, "main")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgentWithPolicy(t, store, "source-agent", runtime.ShellRuntime,
		pipelineShellResultCommand(t, "approved", sourceSummary),
		[]string{"ask"}, "owner/repo", runtime.AutonomyPolicyReadOnly)

	checks := []string{
		`[ -r "$GITMOOT_PIPELINE_UPSTREAM_CONTEXT_FILE" ] || exit 41`,
		`[ "$GITMOOT_PIPELINE_NAME" = ` + posixQuote(name) + ` ] || exit 42`,
		`[ -n "$GITMOOT_PIPELINE_RUN_ID" ] || exit 43`,
		`[ "$GITMOOT_PIPELINE_STAGE_ID" = consume ] || exit 44`,
		`context_value=`,
		`while IFS= read -r line || [ -n "$line" ]; do context_value=$context_value$line; done < "$GITMOOT_PIPELINE_UPSTREAM_CONTEXT_FILE"`,
	}
	for i, fragment := range requiredFragments {
		checks = append(checks, `case "$context_value" in *`+posixQuote(fragment)+`*) ;; *) exit `+string(rune('5'+i))+` ;; esac`)
	}
	checks = append(checks, pipelineShellResultCommand(t, "approved", downstreamSummary))
	consumeCmd := strings.Join(checks, "; ")

	specYAML := "name: " + name + "\nrepo: owner/repo\nstages:\n" +
		"  - id: source\n    agent: source-agent\n    prompt: Produce the arxiv summary.\n" +
		"  - id: consume\n    cmd: |\n      " + consumeCmd + "\n    needs: [source]\n"
	specFile := writeSpec(t, specYAML)
	var out, errBuf bytes.Buffer
	if code := Run([]string{"pipeline", "add", specFile, "--home", home}, &out, &errBuf); code != 0 {
		t.Fatalf("pipeline add exit=%d stderr=%s", code, errBuf.String())
	}
	out.Reset()
	errBuf.Reset()
	if code := Run([]string{"pipeline", "run", name, "--home", home}, &out, &errBuf); code != 0 {
		t.Fatalf("pipeline run exit=%d stderr=%s", code, errBuf.String())
	}
	runID := strings.TrimSpace(out.String())

	enqueue := newPipelineStageEnqueuer(store, home)
	worker := defaultJobWorker(store, io.Discard, home)
	now := time.Date(2026, 7, 12, 9, 0, 0, 0, time.UTC)
	for i := 0; i < 10; i++ {
		if err := runEnabledRepoWorkerTicks(ctx, store, worker, 1, io.Discard, now); err != nil {
			t.Fatalf("worker tick %d: %v", i, err)
		}
		if err := runPipelineScanOnce(ctx, store, enqueue, now); err != nil {
			t.Fatalf("pipeline scan %d: %v", i, err)
		}
		run, _, err := store.GetPipelineRun(ctx, runID)
		if err != nil {
			t.Fatal(err)
		}
		if run.State != pipeline.RunRunning {
			if run.State != pipeline.RunSucceeded {
				t.Fatalf("run state = %s halt=%s reason=%s", run.State, run.HaltStage, run.HaltReason)
			}
			return runID, store
		}
	}
	t.Fatalf("pipeline %s did not settle", name)
	return "", nil
}

func pipelineShellResultCommand(t *testing.T, decision, summary string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"gitmoot_result": map[string]any{
			"decision": decision, "summary": summary,
			"findings": []any{}, "changes_made": []any{}, "tests_run": []any{},
			"needs": []any{}, "delegations": []any{},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return `printf '%s' ` + posixQuote(string(raw))
}

func jsonString(t *testing.T, value string) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/pipeline"
	"github.com/gitmoot/gitmoot/internal/proof"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

const pipelineServiceRunsDir = "pipeline-service-runs"

var pipelineServiceRunIDPattern = regexp.MustCompile(`^psr-[0-9a-f]{32}$`)

type pipelineStoredOutcomeVerification struct {
	Version             int    `json:"version"`
	Kind                string `json:"kind"`
	RunID               string `json:"run_id"`
	Pipeline            string `json:"pipeline"`
	State               string `json:"state"`
	StagesChecked       int    `json:"stages_checked"`
	JobsChecked         int    `json:"jobs_checked"`
	ResultHashesChecked int    `json:"result_hashes_checked"`
	ManifestVerified    bool   `json:"manifest_verified"`
	OutcomeAsOf         string `json:"outcome_as_of"`
	VerifiedAt          string `json:"verified_at"`
}

type verifiedPipelineRunProof struct {
	Manifest     proof.Manifest
	Verification pipelineStoredOutcomeVerification
	Run          db.PipelineRun
	Stages       []db.PipelineRunStage
}

func pipelineServiceRunPaths(paths config.Paths, runID string) (root, base, sourceSpec, archive string, err error) {
	runID = strings.TrimSpace(runID)
	if !pipelineServiceRunIDPattern.MatchString(runID) {
		return "", "", "", "", fmt.Errorf("invalid service run id %q", runID)
	}
	root = filepath.Join(paths.Home, pipelineServiceRunsDir, runID)
	base = filepath.Join(root, "frozen-bundle")
	sourceSpec = filepath.Join(root, "run-spec.yaml")
	archive = filepath.Join(root, "bundle.zip")
	return root, base, sourceSpec, archive, nil
}

// verifyPipelineRunFromStore checks only persisted Gitmoot state. It never runs
// a stage command, reads a transcript, or queries CI/GitHub.
func verifyPipelineRunFromStore(ctx context.Context, store *db.Store, paths config.Paths, runID string) (verifiedPipelineRunProof, error) {
	serviceRun, ok, err := store.GetServiceRun(ctx, runID)
	if err != nil {
		return verifiedPipelineRunProof{}, err
	}
	if !ok {
		return verifiedPipelineRunProof{}, fmt.Errorf("unknown service pipeline run %q", runID)
	}
	run, ok, err := store.GetPipelineRun(ctx, runID)
	if err != nil {
		return verifiedPipelineRunProof{}, err
	}
	if !ok || run.Trigger != "service" || run.Pipeline != serviceRun.PipelineName {
		return verifiedPipelineRunProof{}, fmt.Errorf("service run %q has no consistent pipeline run", runID)
	}
	if run.State != pipeline.RunSucceeded || run.FinishedAt.IsZero() {
		return verifiedPipelineRunProof{}, fmt.Errorf("pipeline run %q is %s, not succeeded", runID, emptyProofValue(run.State))
	}
	_, _, sourceSpecPath, _, err := pipelineServiceRunPaths(paths, runID)
	if err != nil {
		return verifiedPipelineRunProof{}, err
	}
	rawSpec, err := os.ReadFile(sourceSpecPath)
	if err != nil {
		return verifiedPipelineRunProof{}, fmt.Errorf("read frozen run spec: %w", err)
	}
	if pipeline.Hash(rawSpec) != strings.TrimSpace(run.SpecHash) {
		return verifiedPipelineRunProof{}, errors.New("frozen run spec hash does not match pipeline run")
	}
	spec, err := pipeline.Load(rawSpec)
	if err != nil {
		return verifiedPipelineRunProof{}, fmt.Errorf("parse frozen run spec: %w", err)
	}
	if spec.Name != run.Pipeline {
		return verifiedPipelineRunProof{}, errors.New("frozen run spec pipeline name does not match run")
	}
	stages, err := store.ListPipelineRunStages(ctx, runID)
	if err != nil {
		return verifiedPipelineRunProof{}, err
	}
	byStage := make(map[string]db.PipelineRunStage, len(stages))
	for _, stage := range stages {
		if _, duplicate := byStage[stage.StageID]; duplicate {
			return verifiedPipelineRunProof{}, fmt.Errorf("duplicate stored stage %q", stage.StageID)
		}
		byStage[stage.StageID] = stage
	}
	if len(byStage) != len(spec.Stages) {
		return verifiedPipelineRunProof{}, fmt.Errorf("stage count mismatch: stored=%d frozen=%d", len(byStage), len(spec.Stages))
	}

	jobs, results, stageRoots, err := gatherPipelineProofJobs(ctx, store, stages)
	if err != nil {
		return verifiedPipelineRunProof{}, err
	}
	jobsByID := make(map[string]db.Job, len(jobs))
	resultHashesChecked := 0
	for _, job := range jobs {
		jobsByID[job.ID] = job
		if job.State != string(workflow.JobSucceeded) {
			return verifiedPipelineRunProof{}, fmt.Errorf("job %q has non-success terminal state %q", job.ID, job.State)
		}
		if results[job.ID] == nil {
			return verifiedPipelineRunProof{}, fmt.Errorf("job %q has no structured result", job.ID)
		}
		if strings.TrimSpace(job.ResultHash) != "" {
			resultHashesChecked++
			if !proof.StoredResultHashMatches(job.Payload, job.ResultHash) {
				return verifiedPipelineRunProof{}, fmt.Errorf("job %q result_hash does not match stored result", job.ID)
			}
		}
	}
	for _, stageSpec := range spec.Stages {
		row, ok := byStage[stageSpec.ID]
		if !ok {
			return verifiedPipelineRunProof{}, fmt.Errorf("frozen stage %q has no stored row", stageSpec.ID)
		}
		if row.State != pipeline.StageSucceeded && row.State != pipeline.StageSkipped {
			return verifiedPipelineRunProof{}, fmt.Errorf("stage %q is %q, not succeeded/skipped", stageSpec.ID, row.State)
		}
		if row.State == pipeline.StageSkipped {
			continue
		}
		expectedJobID := pipelineStageJobID(runID, stageSpec.ID, row.Attempt)
		if row.JobID != expectedJobID {
			return verifiedPipelineRunProof{}, fmt.Errorf("stage %q job id %q does not match expected %q", stageSpec.ID, row.JobID, expectedJobID)
		}
		job, ok := jobsByID[row.JobID]
		if !ok || job.State != string(workflow.JobSucceeded) || results[row.JobID] == nil {
			return verifiedPipelineRunProof{}, fmt.Errorf("stage %q lacks its succeeded terminal job and result", stageSpec.ID)
		}
		payload, err := workflow.ParseJobPayload(job.Payload)
		if err != nil {
			return verifiedPipelineRunProof{}, fmt.Errorf("stage %q payload is unparseable: %w", stageSpec.ID, err)
		}
		expectedInputEnv := pipelineServiceInputEnvironment(run.PayloadJSON)
		if !equalServiceStringSlices(payload.PipelineInputEnv, expectedInputEnv) {
			return verifiedPipelineRunProof{}, fmt.Errorf("stage %q typed input environment does not match admitted payload", stageSpec.ID)
		}
		if strings.Contains(payload.Instructions, run.PayloadJSON) {
			return verifiedPipelineRunProof{}, fmt.Errorf("stage %q instructions contain the admitted input payload", stageSpec.ID)
		}
		if !serviceContainsString(spec.EffectiveSuccessDecisions(stageSpec), results[row.JobID].Decision) {
			return verifiedPipelineRunProof{}, fmt.Errorf("stage %q result decision %q is not a success decision", stageSpec.ID, results[row.JobID].Decision)
		}
	}

	asOf := run.FinishedAt.UTC().Format(time.RFC3339Nano)
	root, normalized := normalizePipelineProofJobs(run, jobs, stageRoots, asOf)
	structured := proof.Project(root, normalized, results, nil, nil)
	if err := proof.VerifyManifest(structured); err != nil {
		return verifiedPipelineRunProof{}, fmt.Errorf("verify stored pipeline job DAG: %w", err)
	}
	publicJobs, publicResults := sanitizePipelineProofJobs(normalized, results)
	publicManifest := proof.Project(root, publicJobs, publicResults, nil, nil)
	publicManifest, err = proof.WithVerifiedRootClaim(publicManifest,
		"stored_pipeline_outcome", "pipeline_run", run.ID, asOf,
		map[string]string{"pipeline": run.Pipeline, "run_state": run.State, "verification_kind": "stored_pipeline_outcome"})
	if err != nil {
		return verifiedPipelineRunProof{}, fmt.Errorf("verify public pipeline manifest: %w", err)
	}
	verification := pipelineStoredOutcomeVerification{
		Version: 1, Kind: "stored_pipeline_outcome", RunID: run.ID, Pipeline: run.Pipeline,
		State: run.State, StagesChecked: len(stages), JobsChecked: len(jobs),
		ResultHashesChecked: resultHashesChecked, ManifestVerified: true, OutcomeAsOf: asOf,
	}
	return verifiedPipelineRunProof{Manifest: publicManifest, Verification: verification, Run: run, Stages: stages}, nil
}

func gatherPipelineProofJobs(ctx context.Context, store *db.Store, stages []db.PipelineRunStage) ([]db.Job, map[string]*workflow.AgentResult, map[string]string, error) {
	byID := map[string]db.Job{}
	stageRoots := map[string]string{}
	add := func(job db.Job) {
		prior, exists := byID[job.ID]
		if !exists || (prior.ResultHash == "" && job.ResultHash != "") {
			byID[job.ID] = job
		}
	}
	for _, stage := range stages {
		if strings.TrimSpace(stage.JobID) == "" {
			continue
		}
		stageRoots[stage.JobID] = stage.StageID
		job, err := store.GetJobForProof(ctx, stage.JobID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, nil, nil, fmt.Errorf("stage %q job %q not found", stage.StageID, stage.JobID)
			}
			return nil, nil, nil, err
		}
		add(job)
		subtree, err := store.ListJobsByRoot(ctx, stage.JobID)
		if err != nil {
			return nil, nil, nil, err
		}
		for _, child := range subtree {
			add(child)
		}
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	jobs := make([]db.Job, 0, len(ids))
	results := make(map[string]*workflow.AgentResult, len(ids))
	for _, id := range ids {
		job := byID[id]
		payload, err := workflow.ParseJobPayload(job.Payload)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("parse pipeline job %q payload: %w", id, err)
		}
		if job.Repo == "" {
			job.Repo = payload.Repo
		}
		if job.PullRequest == 0 {
			job.PullRequest = payload.PullRequest
		}
		if payload.RuntimeOverride != "" {
			job.Runtime = payload.RuntimeOverride
			job.RuntimeRef = payload.RuntimeOverrideRef
		}
		jobs = append(jobs, job)
		results[id] = payload.Result
	}
	return jobs, results, stageRoots, nil
}

func normalizePipelineProofJobs(run db.PipelineRun, jobs []db.Job, stageRoots map[string]string, asOf string) (db.Job, []db.Job) {
	root := db.Job{ID: run.ID, Type: "pipeline", State: run.State, RootID: run.ID, CreatedAt: run.StartedAt.UTC().Format(time.RFC3339Nano), UpdatedAt: asOf, Payload: `{}`}
	normalized := make([]db.Job, 0, len(jobs)+1)
	normalized = append(normalized, root)
	for _, job := range jobs {
		job.RootID = run.ID
		if stageID, ok := stageRoots[job.ID]; ok {
			job.ParentJobID = run.ID
			job.DelegationID = "stage:" + stageID
			job.DelegationDepth = 1
			job.DelegatedBy = "pipeline"
		} else {
			job.DelegationDepth++
		}
		normalized = append(normalized, job)
	}
	return root, normalized
}

func sanitizePipelineProofJobs(jobs []db.Job, results map[string]*workflow.AgentResult) ([]db.Job, map[string]*workflow.AgentResult) {
	publicJobs := make([]db.Job, 0, len(jobs))
	publicResults := make(map[string]*workflow.AgentResult, len(results))
	for _, job := range jobs {
		if job.Type == "pipeline" {
			publicJobs = append(publicJobs, job)
			continue
		}
		result := results[job.ID]
		var safe *workflow.AgentResult
		if result != nil {
			safe = &workflow.AgentResult{Decision: result.Decision, Findings: make([]json.RawMessage, len(result.Findings))}
			for _, delegation := range result.Delegations {
				safe.Delegations = append(safe.Delegations, workflow.Delegation{
					ID: delegation.ID, Deps: append([]string(nil), delegation.Deps...),
					SynthesisRule: delegation.SynthesisRule, Quorum: delegation.Quorum,
				})
			}
		}
		encoded, _ := json.Marshal(workflow.JobPayload{Result: safe})
		job.Payload = string(encoded)
		job.Repo = ""
		job.PullRequest = 0
		job.WorkflowID = ""
		job.RuntimeRef = ""
		job.ResultHash = ""
		publicJobs = append(publicJobs, job)
		publicResults[job.ID] = safe
	}
	return publicJobs, publicResults
}

func serviceContainsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func equalServiceStringSlices(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func emptyProofValue(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return strings.TrimSpace(value)
}

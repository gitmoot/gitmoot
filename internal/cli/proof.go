package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/proof"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func runProof(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("proof", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	jsonOutput := fs.Bool("json", false, "print the canonical manifest JSON")
	verify := fs.Bool("verify", false, "verify a succeeded pipeline run against offline store state")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 1 || strings.TrimSpace(fs.Arg(0)) == "" {
		fmt.Fprintln(stderr, "proof requires one non-empty <root-id> (flags must precede it)")
		return 2
	}
	rootID := strings.TrimSpace(fs.Arg(0))
	paths, err := pathsFromFlag(*home)
	if err != nil {
		fmt.Fprintf(stderr, "proof: resolve home: %v\n", err)
		return 1
	}
	store, err := db.OpenReadOnly(paths.Database)
	if err != nil {
		fmt.Fprintf(stderr, "proof: open read-only store: %v\n", err)
		return 1
	}
	defer store.Close()

	var manifest proof.Manifest
	if *verify {
		verified, verifyErr := verifyPipelineRunFromStore(context.Background(), store, paths, rootID)
		err = verifyErr
		manifest = verified.Manifest
	} else {
		manifest, err = loadProofManifest(context.Background(), store, rootID)
	}
	if err != nil {
		fmt.Fprintf(stderr, "proof: %v\n", err)
		return 1
	}
	if *jsonOutput {
		raw, err := proof.Marshal(manifest)
		if err != nil {
			fmt.Fprintf(stderr, "proof: encode manifest: %v\n", err)
			return 1
		}
		if _, err := stdout.Write(append(raw, '\n')); err != nil {
			fmt.Fprintf(stderr, "proof: write manifest: %v\n", err)
			return 1
		}
		return 0
	}
	if err := proof.RenderTree(stdout, manifest); err != nil {
		fmt.Fprintf(stderr, "proof: render manifest: %v\n", err)
		return 1
	}
	return 0
}

func loadProofManifest(ctx context.Context, store *db.Store, rootID string) (proof.Manifest, error) {
	jobs, err := store.ListJobsByRoot(ctx, rootID)
	if err != nil {
		return proof.Manifest{}, fmt.Errorf("list root %q: %w", rootID, err)
	}
	if len(jobs) == 0 {
		return proof.Manifest{}, fmt.Errorf("unknown root-id %q", rootID)
	}
	results := make(map[string]*workflow.AgentResult, len(jobs))
	payloads := make(map[string]workflow.JobPayload, len(jobs))
	events := make(map[string][]db.JobEvent, len(jobs))
	agents := map[string]db.Agent{}
	rootIndex := -1
	for i := range jobs {
		job := &jobs[i]
		if job.ID == rootID {
			rootIndex = i
		}
		var payload workflow.JobPayload
		// A corrupt row is evidence of a gap, not a reason to suppress the rest of
		// the proof. Project records payload_unparseable on that session.
		_ = json.Unmarshal([]byte(job.Payload), &payload)
		payloads[job.ID] = payload
		if payload.Result != nil {
			results[job.ID] = payload.Result
		}
		if job.Repo == "" {
			job.Repo = payload.Repo
		}
		if job.PullRequest == 0 {
			job.PullRequest = payload.PullRequest
		}
		if job.WorkflowID == "" {
			job.WorkflowID = payload.WorkflowID
		}
		if payload.RuntimeOverride != "" {
			job.Runtime = payload.RuntimeOverride
			job.RuntimeRef = payload.RuntimeOverrideRef
		} else if strings.TrimSpace(job.Agent) != "" {
			agent, ok := agents[job.Agent]
			if !ok {
				agent, err = store.GetAgent(ctx, job.Agent)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					return proof.Manifest{}, fmt.Errorf("load agent %q: %w", job.Agent, err)
				}
				agents[job.Agent] = agent
			}
			job.Runtime = agent.Runtime
			job.RuntimeRef = agent.RuntimeRef
		}
		jobEvents, err := store.ListJobEvents(ctx, job.ID)
		if err != nil {
			return proof.Manifest{}, fmt.Errorf("list events for job %q: %w", job.ID, err)
		}
		events[job.ID] = jobEvents
	}
	if rootIndex < 0 {
		return proof.Manifest{}, fmt.Errorf("root-id %q has descendants but no root anchor job", rootID)
	}

	receipts, err := loadProofPRReceipts(ctx, store, jobs, payloads)
	if err != nil {
		return proof.Manifest{}, err
	}
	manifest := proof.Project(jobs[rootIndex], jobs, results, receipts, events)
	if err := proof.VerifyManifest(manifest); err != nil {
		return proof.Manifest{}, fmt.Errorf("verify projected manifest: %w", err)
	}
	return manifest, nil
}

func loadProofPRReceipts(ctx context.Context, store *db.Store, jobs []db.Job, payloads map[string]workflow.JobPayload) ([]proof.PRReceipt, error) {
	receipts := map[string]proof.PRReceipt{}
	notesByWorkflow := map[string][]db.WorkflowNote{}
	for _, job := range jobs {
		payload := payloads[job.ID]
		repo := strings.TrimSpace(job.Repo)
		if repo == "" {
			repo = strings.TrimSpace(payload.Repo)
		}
		number := job.PullRequest
		if number == 0 {
			number = payload.PullRequest
		}
		if number <= 0 {
			continue
		}
		key := fmt.Sprintf("%s#%d", repo, number)
		if _, exists := receipts[key]; exists {
			continue
		}
		receipt := proof.PRReceipt{Repo: repo, Number: number, HeadSHA: payload.HeadSHA}
		storedMergeCommitSHA := ""
		if repo != "" {
			storedPR, err := store.GetPullRequest(ctx, repo, int64(number))
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, fmt.Errorf("load stored PR %s: %w", key, err)
			}
			if err == nil {
				receipt.HeadSHA = firstProofValue(storedPR.HeadSHA, receipt.HeadSHA)
				storedMergeCommitSHA = storedPR.MergeCommitSHA
				receipt.State = storedPR.State
			}
		}
		workflowID := strings.TrimSpace(job.WorkflowID)
		if workflowID == "" {
			workflowID = strings.TrimSpace(payload.WorkflowID)
		}
		if workflowID != "" {
			notes, loaded := notesByWorkflow[workflowID]
			if !loaded {
				var loadErr error
				notes, loadErr = store.ListWorkflowNotes(ctx, workflowID, 0)
				if loadErr != nil {
					return nil, fmt.Errorf("load workflow receipts %q: %w", workflowID, loadErr)
				}
				notesByWorkflow[workflowID] = notes
			}
			for _, note := range notes {
				if note.Author != db.WorkflowAutoNoteAuthor {
					continue
				}
				if strings.HasPrefix(note.Body, fmt.Sprintf("[auto:pr:%d:opened]", number)) {
					receipt.OpenedAt = note.CreatedAt
				}
				if strings.HasPrefix(note.Body, fmt.Sprintf("[auto:pr:%d:merged]", number)) {
					receipt.MergedAt = note.CreatedAt
					receipt.MergeCommitSHA = storedMergeCommitSHA
				}
			}
		}
		receipts[key] = receipt
	}
	keys := make([]string, 0, len(receipts))
	for key := range receipts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]proof.PRReceipt, 0, len(keys))
	for _, key := range keys {
		out = append(out, receipts[key])
	}
	return out, nil
}

func firstProofValue(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

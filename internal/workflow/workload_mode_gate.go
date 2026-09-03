package workflow

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
)

const (
	operatingModeNotePrefix        = "[operating-mode "
	modeReconciliationNotePrefix   = "[workload-mode-reconciliation "
	workloadModeMarker             = "**Current mode:"
	workloadModeReconciliationScan = 200
)

type modeSensitivePullRequest struct {
	required  bool
	mode      string
	ambiguous bool
}

// workloadModeEnforcedOwner is the repository OWNER whose mode-marker changes
// this gate enforces. AGENTS.md writes its workload-mode rules against the
// gitmoot/* scope, but the gate compared FullName to the literal
// "gitmoot/gitmoot", so a mode-marker change in any other gitmoot repository
// merged with no reconciliation at all (#1783 review, P3). The note grammar is
// already repo-parameterized, so widening to the owner matches the policy the
// gate implements without inventing a config knob.
const workloadModeEnforcedOwner = "gitmoot"

func inspectModeSensitivePullRequest(ctx context.Context, gh MergeGateGitHub, repo github.Repository, number int64) (modeSensitivePullRequest, error) {
	if strings.TrimSpace(repo.Owner) != workloadModeEnforcedOwner {
		return modeSensitivePullRequest{}, nil
	}
	files, err := gh.ListPullRequestFiles(ctx, repo, number)
	if err != nil {
		return modeSensitivePullRequest{}, fmt.Errorf("list pull request files for workload-mode reconciliation: %w", err)
	}
	for _, file := range files {
		if strings.TrimSpace(file.Filename) != "AGENTS.md" {
			continue
		}
		patch := strings.TrimSpace(file.Patch)
		if patch == "" {
			return modeSensitivePullRequest{required: true, ambiguous: true}, nil
		}
		var addedMode string
		changed := false
		ambiguous := false
		for _, line := range strings.Split(patch, "\n") {
			if len(line) < 2 || (line[0] != '+' && line[0] != '-') {
				continue
			}
			text := strings.TrimSpace(line[1:])
			if !strings.HasPrefix(text, workloadModeMarker) {
				continue
			}
			changed = true
			if line[0] != '+' {
				continue
			}
			mode, ok := parseWorkloadModeMarker(text)
			if !ok || (addedMode != "" && addedMode != mode) {
				ambiguous = true
				continue
			}
			addedMode = mode
		}
		if !changed {
			return modeSensitivePullRequest{}, nil
		}
		if addedMode == "" {
			ambiguous = true
		}
		return modeSensitivePullRequest{required: true, mode: addedMode, ambiguous: ambiguous}, nil
	}
	return modeSensitivePullRequest{}, nil
}

func parseWorkloadModeMarker(line string) (string, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, workloadModeMarker) || !strings.HasSuffix(line, ".**") {
		return "", false
	}
	mode := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, workloadModeMarker), ".**"))
	return mode, validWorkloadMode(mode)
}

func validWorkloadMode(mode string) bool {
	switch strings.ToUpper(strings.TrimSpace(mode)) {
	case "THROUGHPUT", "STEADY", "DRAIN":
		return true
	default:
		return false
	}
}

type operatingModeDecision struct {
	id   int64
	mode string
}

func ensureWorkloadModeReconciled(ctx context.Context, store *db.Store, gh MergeGateGitHub, repo github.Repository, number int64, headSHA string) (required, reconciled bool, reason string, err error) {
	observed, err := inspectModeSensitivePullRequest(ctx, gh, repo, number)
	if err != nil {
		return false, false, "", err
	}
	if !observed.required {
		return false, true, "", nil
	}
	if store == nil {
		return true, false, "", fmt.Errorf("workload-mode reconciliation requires a store")
	}
	decision, err := latestOperatingModeDecision(ctx, store, repo.FullName())
	if err != nil {
		return true, false, "", err
	}
	expectedMode := strings.ToUpper(strings.TrimSpace(observed.mode))
	if expectedMode == "" && decision.id > 0 {
		expectedMode = decision.mode
	}
	decisionID := "none"
	if decision.id > 0 {
		decisionID = strconv.FormatInt(decision.id, 10)
	}
	notes, err := store.ListRepoWorkflowNotesByBodyPrefix(ctx, modeReconciliationNotePrefix, repo.FullName(), workloadModeReconciliationScan)
	if err != nil {
		return true, false, "", fmt.Errorf("list workload-mode reconciliation notes: %w", err)
	}
	// Two rejections used to be silent, and the #1783 review found both.
	//
	// P2a: the documented `decision_note=none` was unmatchable. decisionID is
	// "none" only while the repo has ZERO operating-mode notes, so the moment the
	// first typed note exists, every row written per the documented PR-sourced
	// instruction was skipped and the PR held forever. `none` is now accepted as
	// what it claims to be: a PR-SOURCED decision, where the PR itself decides
	// and no earlier note has to agree. The row must still be newer than the
	// newest note, so a superseded PR-sourced row cannot ratify anything.
	//
	// P2b: a row citing a note id proved only that someone READ that note, never
	// that the PR agreed with it. With the newest note deciding STEADY, a PR
	// flipping the marker to THROUGHPUT merged cleanly by citing the STEADY note.
	// A row that names a decision must now match that decision's mode.
	//
	// Every rejection reason is recorded and appended to the hold, because the
	// native path holds through g.pending, which retries silently: a coordinator
	// who HAD written a row saw an indefinitely pending PR and no cause.
	nearMiss := ""
	for _, note := range notes {
		fields, ok := parseModeNoteFields(note.Body, modeReconciliationNotePrefix)
		if !ok || !workflowNoteMatchesRepo(note, fields, repo.FullName()) {
			continue
		}
		if fields["pr"] != strconv.FormatInt(number, 10) || fields["head"] != strings.TrimSpace(headSHA) {
			continue
		}
		mode := strings.ToUpper(strings.TrimSpace(fields["mode"]))
		if !validWorkloadMode(mode) {
			nearMiss = fmt.Sprintf("; row %d declares mode=%q, which is not a workload mode", note.ID, fields["mode"])
			continue
		}
		if expectedMode != "" && mode != expectedMode {
			nearMiss = fmt.Sprintf("; row %d reconciles mode=%s but the PR changes the marker to %s", note.ID, mode, expectedMode)
			continue
		}
		if decision.id > 0 && note.ID <= decision.id {
			nearMiss = fmt.Sprintf("; row %d predates operating-mode note %d, so it reconciles a superseded decision", note.ID, decision.id)
			continue
		}
		cited := strings.TrimSpace(fields["decision_note"])
		if cited == "none" {
			// PR-sourced: this PR is the decision, so no earlier note must agree.
			return true, true, "", nil
		}
		if cited != decisionID {
			nearMiss = fmt.Sprintf("; row %d cites decision_note=%s but the newest operating-mode note is %s", note.ID, cited, decisionID)
			continue
		}
		if decision.id > 0 && mode != decision.mode {
			nearMiss = fmt.Sprintf("; row %d cites operating-mode note %d, which decided %s, so it cannot ratify %s", note.ID, decision.id, decision.mode, mode)
			continue
		}
		return true, true, "", nil
	}
	detail := fmt.Sprintf("workload-mode change requires reconciliation at head %s against operating-mode note %s", shortSHA(headSHA), decisionID)
	if observed.ambiguous {
		detail += "; AGENTS.md mode-marker patch is missing or ambiguous"
	}
	detail += nearMiss
	return true, false, detail, nil
}

func latestOperatingModeDecision(ctx context.Context, store *db.Store, repo string) (operatingModeDecision, error) {
	notes, err := store.ListRepoWorkflowNotesByBodyPrefix(ctx, operatingModeNotePrefix, repo, workloadModeReconciliationScan)
	if err != nil {
		return operatingModeDecision{}, fmt.Errorf("list operating-mode notes: %w", err)
	}
	for _, note := range notes {
		fields, ok := parseModeNoteFields(note.Body, operatingModeNotePrefix)
		if !ok || !workflowNoteMatchesRepo(note, fields, repo) {
			continue
		}
		mode := strings.ToUpper(strings.TrimSpace(fields["mode"]))
		if validWorkloadMode(mode) {
			return operatingModeDecision{id: note.ID, mode: mode}, nil
		}
	}
	return operatingModeDecision{}, nil
}

func parseModeNoteFields(body, prefix string) (map[string]string, bool) {
	if !strings.HasPrefix(body, prefix) {
		return nil, false
	}
	end := strings.IndexByte(body, ']')
	if end < len(prefix) {
		return nil, false
	}
	fields := make(map[string]string)
	for _, field := range strings.Fields(body[len(prefix):end]) {
		key, value, ok := strings.Cut(field, "=")
		if !ok || strings.TrimSpace(key) == "" || strings.TrimSpace(value) == "" {
			return nil, false
		}
		fields[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return fields, true
}

func workflowNoteMatchesRepo(note db.WorkflowNote, fields map[string]string, repo string) bool {
	repo = strings.TrimSpace(repo)
	if noteRepo := strings.TrimSpace(note.Repo); noteRepo != "" && noteRepo != repo {
		return false
	}
	return strings.TrimSpace(fields["repo"]) == repo
}

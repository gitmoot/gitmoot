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

func inspectModeSensitivePullRequest(ctx context.Context, gh MergeGateGitHub, repo github.Repository, number int64) (modeSensitivePullRequest, error) {
	if repo.FullName() != "gitmoot/gitmoot" {
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
	notes, err := store.ListWorkflowNotesByBodyPrefix(ctx, modeReconciliationNotePrefix, workloadModeReconciliationScan)
	if err != nil {
		return true, false, "", fmt.Errorf("list workload-mode reconciliation notes: %w", err)
	}
	for _, note := range notes {
		fields, ok := parseModeNoteFields(note.Body, modeReconciliationNotePrefix)
		if !ok || !workflowNoteMatchesRepo(note, fields, repo.FullName()) {
			continue
		}
		if fields["pr"] != strconv.FormatInt(number, 10) || fields["head"] != strings.TrimSpace(headSHA) || fields["decision_note"] != decisionID {
			continue
		}
		mode := strings.ToUpper(strings.TrimSpace(fields["mode"]))
		if !validWorkloadMode(mode) || (expectedMode != "" && mode != expectedMode) {
			continue
		}
		if decision.id > 0 && note.ID <= decision.id {
			continue
		}
		return true, true, "", nil
	}
	detail := fmt.Sprintf("workload-mode change requires reconciliation at head %s against operating-mode note %s", shortSHA(headSHA), decisionID)
	if observed.ambiguous {
		detail += "; AGENTS.md mode-marker patch is missing or ambiguous"
	}
	return true, false, detail, nil
}

func latestOperatingModeDecision(ctx context.Context, store *db.Store, repo string) (operatingModeDecision, error) {
	notes, err := store.ListWorkflowNotesByBodyPrefix(ctx, operatingModeNotePrefix, workloadModeReconciliationScan)
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

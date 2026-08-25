package workflow

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/gitmoot/gitmoot/internal/evidence"
	"github.com/gitmoot/gitmoot/internal/execbackend"
	"github.com/gitmoot/gitmoot/internal/subprocess"
)

const (
	ResultObservationSourceWorktreeDiff                = "worktree_git_diff"
	ResultObservationSourceWorktreeLessDelegationChild = "excluded_worktree_less_delegation_child"
)

var resultClaimLineSuffix = regexp.MustCompile(`:\d+(?:(?::|-)\d+)?$`)

// ChangeObservation binds one reported changes_made entry to the files the
// engine observed in the job worktree. GradeObserved means every file named by
// the claim was present in the diff; it does not verify the claim's prose.
type ChangeObservation struct {
	Claim        string         `json:"claim"`
	ClaimedFiles []string       `json:"claimed_files"`
	Observation  []string       `json:"observation"`
	Grade        evidence.Grade `json:"grade"`
	Divergent    bool           `json:"divergent"`
}

// HasCapturedPathBinding reports whether every per-change observation exists in
// touchedFiles and every claimed file resolves to one of those observations.
// Unqualified filenames may resolve through one unique basename; qualified
// paths must match exactly.
func (c ChangeObservation) HasCapturedPathBinding(touchedFiles []string) bool {
	if c.Divergent || len(c.ClaimedFiles) == 0 || len(c.Observation) == 0 || len(touchedFiles) == 0 {
		return false
	}
	claimed := sortedUniquePaths(c.ClaimedFiles)
	observed := sortedUniquePaths(c.Observation)
	touchedList := sortedUniquePaths(touchedFiles)
	if len(claimed) == 0 || len(observed) == 0 || len(touchedList) == 0 {
		return false
	}
	touched := make(map[string]struct{}, len(touchedList))
	for _, file := range touchedList {
		touched[file] = struct{}{}
	}
	observedSet := make(map[string]struct{}, len(observed))
	for _, file := range observed {
		if !hasPath(touched, file) {
			return false
		}
		observedSet[file] = struct{}{}
	}
	fromClaim := claimFilePaths(c.Claim, observed)
	if len(fromClaim) != len(claimed) {
		return false
	}
	for i := range claimed {
		if fromClaim[i] != claimed[i] {
			return false
		}
	}
	covered := make(map[string]struct{}, len(observed))
	for _, file := range claimed {
		if hasPath(observedSet, file) {
			covered[file] = struct{}{}
			continue
		}
		if strings.Contains(file, "/") {
			return false
		}
		var match string
		for _, candidate := range observed {
			if path.Base(candidate) != file {
				continue
			}
			if match != "" {
				return false
			}
			match = candidate
		}
		if match == "" {
			return false
		}
		covered[match] = struct{}{}
	}
	return len(covered) == len(observed)
}

// IsExactPathObserved reports whether an observed-grade entry binds every
// normalized claimed path to that exact path in the captured worktree diff. It
// rejects basename substitutions and paths absent from touchedFiles, including
// malformed observations persisted by older engines. Missing or indeterminate
// capture membership fails toward reported, never observed.
func (c ChangeObservation) IsExactPathObserved(touchedFiles []string) bool {
	if c.Grade != evidence.GradeObserved || !c.HasCapturedPathBinding(touchedFiles) {
		return false
	}
	claimed := sortedUniquePaths(c.ClaimedFiles)
	observed := sortedUniquePaths(c.Observation)
	if len(claimed) != len(observed) {
		return false
	}
	for i := range claimed {
		if claimed[i] != observed[i] {
			return false
		}
	}
	return true
}

// ResultObservation is the persisted, read-only comparison between an
// implement result's changes_made claims and the git diff at result-persistence
// time. ClaimedOnlyFiles records over-claims; UnclaimedFiles records diff paths
// no claim mentions. Capture errors are evidence gaps, not persistence errors.
type ResultObservation struct {
	Source           string              `json:"source"`
	TouchedFiles     []string            `json:"touched_files"`
	Changes          []ChangeObservation `json:"changes"`
	ClaimedOnlyFiles []string            `json:"claimed_only_files"`
	UnclaimedFiles   []string            `json:"unclaimed_files"`
	UnboundClaims    []string            `json:"unbound_claims"`
	Divergent        bool                `json:"divergent"`
	Error            string              `json:"error,omitempty"`
}

// excludedResultObservation records that a caller deliberately excluded a job
// shape from worktree observation. Keeping the exclusion typed and persisted
// makes it distinguishable from a completed observation with no divergence.
func excludedResultObservation(source string, result AgentResult) *ResultObservation {
	return &ResultObservation{
		Source:           strings.TrimSpace(source),
		TouchedFiles:     []string{},
		Changes:          reportedChangeObservations(result.ChangesMade),
		ClaimedOnlyFiles: []string{},
		UnclaimedFiles:   []string{},
		UnboundClaims:    []string{},
		Divergent:        false,
	}
}

// observeResultChanges captures the worktree diff and binds it to the result.
// Callers must resolve or explicitly exclude the delivery worktree before this
// function is reached; an empty path is therefore recorded as an observation
// error rather than disappearing as nil.
func observeResultChanges(ctx context.Context, worktree string, result AgentResult, backend execbackend.Backend) *ResultObservation {
	worktree = strings.TrimSpace(worktree)
	if worktree == "" {
		return &ResultObservation{
			Source:           ResultObservationSourceWorktreeDiff,
			TouchedFiles:     []string{},
			Changes:          reportedChangeObservations(result.ChangesMade),
			ClaimedOnlyFiles: []string{},
			UnclaimedFiles:   []string{},
			UnboundClaims:    []string{},
			Divergent:        false,
			Error:            "observe worktree diff: resolved delivery worktree is empty",
		}
	}
	files, err := execbackend.Consume(backend, func() ([]string, error) {
		return changedWorktreeFiles(ctx, worktree)
	})
	if err != nil {
		return &ResultObservation{
			Source:           ResultObservationSourceWorktreeDiff,
			TouchedFiles:     []string{},
			Changes:          reportedChangeObservations(result.ChangesMade),
			ClaimedOnlyFiles: []string{},
			UnclaimedFiles:   []string{},
			UnboundClaims:    []string{},
			Divergent:        false,
			Error:            err.Error(),
		}
	}
	return compareResultChanges(result.ChangesMade, files)
}

func changedWorktreeFiles(ctx context.Context, worktree string) ([]string, error) {
	runner := subprocess.ExecRunner{}
	tracked, err := runner.Run(ctx, worktree, "git", "diff", "--name-only", "--no-renames", "--no-ext-diff", "--ignore-submodules=none", "-z", "HEAD", "--")
	if err != nil {
		return nil, fmt.Errorf("observe tracked worktree diff: %w", err)
	}
	untracked, err := runner.Run(ctx, worktree, "git", "ls-files", "--others", "--exclude-standard", "-z", "--")
	if err != nil {
		return nil, fmt.Errorf("observe untracked worktree files: %w", err)
	}
	files := append(splitNULPaths(tracked.Stdout), splitNULPaths(untracked.Stdout)...)
	return sortedUniquePaths(files), nil
}

func splitNULPaths(output string) []string {
	var paths []string
	for _, item := range strings.Split(output, "\x00") {
		if item = strings.TrimSpace(item); item != "" {
			paths = append(paths, item)
		}
	}
	return paths
}

func sortedUniquePaths(files []string) []string {
	seen := make(map[string]struct{}, len(files))
	for _, file := range files {
		file = normalizeClaimPath(file)
		if file != "" {
			seen[file] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for file := range seen {
		out = append(out, file)
	}
	sort.Strings(out)
	return out
}

func reportedChangeObservations(claims []string) []ChangeObservation {
	out := make([]ChangeObservation, 0, len(claims))
	for _, claim := range claims {
		out = append(out, ChangeObservation{
			Claim:        claim,
			ClaimedFiles: []string{},
			Observation:  []string{},
			Grade:        evidence.GradeReported,
			Divergent:    false,
		})
	}
	return out
}

func compareResultChanges(claims, touchedFiles []string) *ResultObservation {
	touchedFiles = sortedUniquePaths(touchedFiles)
	touched := make(map[string]struct{}, len(touchedFiles))
	byBase := make(map[string][]string, len(touchedFiles))
	for _, file := range touchedFiles {
		touched[file] = struct{}{}
		base := path.Base(file)
		byBase[base] = append(byBase[base], file)
	}

	covered := make(map[string]struct{}, len(touchedFiles))
	claimedOnly := make(map[string]struct{})
	unboundClaims := make([]string, 0)
	changes := make([]ChangeObservation, 0, len(claims))
	for _, claim := range claims {
		claimedFiles := claimFilePaths(claim, touchedFiles)
		observed := make([]string, 0, len(claimedFiles))
		divergent := len(claimedFiles) == 0
		exactPaths := len(claimedFiles) > 0
		if divergent {
			unboundClaims = append(unboundClaims, claim)
		}
		for _, claimed := range claimedFiles {
			switch {
			case hasPath(touched, claimed):
				observed = append(observed, claimed)
				covered[claimed] = struct{}{}
			case !strings.Contains(claimed, "/") && len(byBase[claimed]) == 1:
				// A unique basename can assist an unqualified filename claim, but
				// it is not evidence that the agent named the observed repo path.
				// Keep the binding reported rather than upgrading it to observed.
				match := byBase[claimed][0]
				observed = append(observed, match)
				covered[match] = struct{}{}
				exactPaths = false
			default:
				divergent = true
				claimedOnly[claimed] = struct{}{}
			}
		}
		grade := evidence.GradeReported
		if !divergent && exactPaths {
			grade = evidence.GradeObserved
		}
		changes = append(changes, ChangeObservation{
			Claim:        claim,
			ClaimedFiles: claimedFiles,
			Observation:  sortedUniquePaths(observed),
			Grade:        grade,
			Divergent:    divergent,
		})
	}

	unclaimed := make([]string, 0)
	for _, file := range touchedFiles {
		if _, ok := covered[file]; !ok {
			unclaimed = append(unclaimed, file)
		}
	}
	claimedOnlyFiles := setKeys(claimedOnly)
	return &ResultObservation{
		Source:           ResultObservationSourceWorktreeDiff,
		TouchedFiles:     touchedFiles,
		Changes:          changes,
		ClaimedOnlyFiles: claimedOnlyFiles,
		UnclaimedFiles:   unclaimed,
		UnboundClaims:    unboundClaims,
		Divergent:        len(claimedOnlyFiles) > 0 || len(unclaimed) > 0 || len(unboundClaims) > 0 || anyDivergentChange(changes),
	}
}

func claimFilePaths(claim string, _ []string) []string {
	binding := strings.TrimSpace(claim)
	if separator := strings.IndexFunc(binding, func(r rune) bool {
		return unicode.IsSpace(r) || r == '—'
	}); separator >= 0 {
		binding = binding[:separator]
	}
	binding = normalizeClaimPath(resultClaimLineSuffix.ReplaceAllString(binding, ""))
	if binding == "" {
		return []string{}
	}
	return []string{binding}
}

func normalizeClaimPath(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	value = strings.TrimPrefix(value, "./")
	if value == "" || strings.Contains(value, "://") || strings.HasPrefix(value, "/") {
		return ""
	}
	cleaned := path.Clean(value)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return ""
	}
	return cleaned
}

func hasPath(paths map[string]struct{}, candidate string) bool {
	_, ok := paths[candidate]
	return ok
}

func setKeys(values map[string]struct{}) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func anyDivergentChange(changes []ChangeObservation) bool {
	for _, change := range changes {
		if change.Divergent {
			return true
		}
	}
	return false
}

func invalidCapturedBindingClaims(changes []ChangeObservation, touchedFiles []string) []string {
	var claims []string
	for _, change := range changes {
		if len(claimFilePaths(change.Claim, touchedFiles)) > 0 && !change.HasCapturedPathBinding(touchedFiles) {
			claims = append(claims, change.Claim)
		}
	}
	return claims
}

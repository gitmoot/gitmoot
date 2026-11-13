package workflow

import (
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/gitmoot/gitmoot/internal/evidence"
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

func claimFilePaths(claim string, touchedFiles []string) []string {
	tokens := strings.FieldsFunc(claim, claimTokenSeparator)
	if len(tokens) == 0 {
		return []string{}
	}

	touchedFiles = sortedUniquePaths(touchedFiles)
	touched := make(map[string]struct{}, len(touchedFiles))
	byBase := make(map[string][]string, len(touchedFiles))
	for _, file := range touchedFiles {
		touched[file] = struct{}{}
		byBase[path.Base(file)] = append(byBase[path.Base(file)], file)
	}

	leading, hasLineSuffix := normalizeClaimToken(tokens[0])
	if leading != "" && (hasLineSuffix || claimPathMatchesTouched(leading, touched, byBase, true)) {
		return []string{leading}
	}

	candidates := make([]string, 0)
	for _, token := range tokens[1:] {
		candidate, _ := normalizeClaimToken(token)
		if candidate != "" && claimPathMatchesTouched(candidate, touched, byBase, false) {
			candidates = append(candidates, candidate)
		}
	}
	return sortedUniquePaths(candidates)
}

func claimTokenSeparator(r rune) bool {
	return unicode.IsSpace(r) || strings.ContainsRune("\"'`()[]{}<>,;", r)
}

func normalizeClaimToken(token string) (string, bool) {
	token = strings.Trim(token, ".,;!?\"'`()[]{}<>")
	hasLineSuffix := resultClaimLineSuffix.MatchString(token)
	token = resultClaimLineSuffix.ReplaceAllString(token, "")
	token = strings.TrimSuffix(token, ":")
	return normalizeClaimPath(token), hasLineSuffix
}

func claimPathMatchesTouched(candidate string, touched map[string]struct{}, byBase map[string][]string, allowUniqueBasename bool) bool {
	if hasPath(touched, candidate) {
		return true
	}
	return allowUniqueBasename && !strings.Contains(candidate, "/") && len(byBase[candidate]) == 1
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

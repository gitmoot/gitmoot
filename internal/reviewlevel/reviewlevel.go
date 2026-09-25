// Package reviewlevel decides whether a pull request head needs an independent
// review before merge, a review after merge, or none (gitmoot/gitmoot#2265).
//
// Fixed rules come first and never call the model: gitmoot/gitmoot is always
// reviewed, and a small set of paths (agent instructions, CI, deploy config,
// dependency locks, migrations, Gitmoot's merge gate and credential gateway)
// always is. Everything else is judged by JEV on the diff itself. Every failure
// to judge decides "review before merge".
package reviewlevel

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/gitmoot/gitmoot/internal/jev"
)

const (
	LevelNoReview   = "level1_no_review"
	LevelBackground = "level2_background"
	LevelRequired   = "level3_required"

	// RiskThreshold: a high-impact-category probability ABOVE this forces
	// review before merge. Measured on 259 historical PRs; at 0.35 both real
	// permission-bypass findings in the sample were forced to level 3.
	RiskThreshold = 0.35
	// StateBudgetBytes bounds the diff text sent to the model.
	StateBudgetBytes = 48000

	questionRisk    = "fixed_category"
	questionLevel   = "level"
	levelUncertain  = "uncertain"
	truncatedMarker = "\n[... file truncated ...]\n"
)

// AlwaysReviewRepos are reviewed before every merge regardless of the diff.
var AlwaysReviewRepos = []string{"gitmoot/gitmoot"}

// FixedPathPattern names paths whose change always needs review before merge.
var FixedPathPattern = regexp.MustCompile(`(^|/)(AGENTS|CLAUDE)\.md$|^\.agents/|^skills/.*SKILL\.md$|^\.github/workflows/|(^|/)(fly|render|vercel|netlify)\.(toml|json|ya?ml)$|^deploy/|(^|/)go\.(mod|sum)$|(^|/)package(-lock)?\.json$|(^|/)(pnpm-lock\.yaml|yarn\.lock|Podfile\.lock|Package\.resolved|Cargo\.lock|poetry\.lock|uv\.lock)$|(^|/)migrations?/|^internal/workflow/merge_gate|^internal/credgw/|^internal/config/org\.go$`)

// File is one changed file. Patch is empty when GitHub omitted it (binary or
// too large), which makes the diff incomplete.
type File struct {
	Path  string
	Patch string
}

// Input is one pull request head.
type Input struct {
	Repo    string
	PR      int
	Title   string
	HeadSHA string
	Files   []File
}

// Decision is the chosen level and why.
type Decision struct {
	Level           string             `json:"level"`
	Source          string             `json:"source"`
	Reason          string             `json:"reason"`
	HeadSHA         string             `json:"head_sha"`
	RiskProbability *float64           `json:"risk_probability,omitempty"`
	JEVChoice       string             `json:"jev_choice,omitempty"`
	Probabilities   map[string]float64 `json:"probabilities,omitempty"`
	DiffComplete    bool               `json:"diff_complete"`
}

// Judge evaluates one JEV request; *jev.Client satisfies it.
type Judge interface {
	Evaluate(ctx context.Context, request jev.Request) (jev.Exchange, error)
}

// Questions returns the frozen rubric questions (levels-v2-rubric.json).
func Questions() map[string]jev.Question {
	return map[string]jev.Question{
		questionRisk: {
			Type:         "noul",
			Instructions: "Answer yes ONLY if the diff itself changes one of: (a) security, credentials/secrets, authentication, permissions, sandbox/isolation; (b) data that cannot easily be restored (schema/migration, destructive deletes of stored user data); (c) deploy, CI, merge, or review rules; (d) WHETHER money is spent or charged, or WHETHER messages/emails go to people outside the team (a new send path, or changing the gate that allows it); (e) deletes or weakens existing test assertions without an equivalent replacement. Merely mentioning payments, paid features, outreach wording, monitoring, or cost is NOT enough. Ordinary features and bug fixes are NO.",
			Criteria: map[string]string{
				"true":  "The diff itself changes one of the listed high-impact categories.",
				"false": "Ordinary feature, bug fix, docs, or refactor; none of the listed categories.",
			},
		},
		questionLevel: {
			Type:         "choice",
			Instructions: "Pick the review level for this change by the IMPACT of a possible mistake, not by size, and not by whether it is a feature. Assume checks pass and the change can be reverted. If diffComplete=false, do not pick level1. Pick uncertain only when essential evidence is missing, never just because a change is large or complex.",
			Criteria: map[string]string{
				LevelNoReview:   "No behavior change for users or agents: docs, comments, formatting, test-only additions, internal scripts, or a refactor covered by existing tests.",
				LevelBackground: "Changes behavior (feature or bug fix, even a large one) but a mistake would be contained and fixed by reverting: no lasting damage, no outside side effects.",
				LevelRequired:   "A mistake would be hard to undo by revert, or would cause wide damage before anyone could notice.",
				levelUncertain:  "Essential evidence is missing.",
			},
		},
	}
}

// Decide chooses the review level for one head. It never returns an error:
// anything that prevents a judgement decides review before merge.
func Decide(ctx context.Context, judge Judge, model string, in Input) Decision {
	state, complete := BuildState(in)
	decision := Decision{HeadSHA: in.HeadSHA, DiffComplete: complete}
	required := func(source, reason string) Decision {
		decision.Level, decision.Source, decision.Reason = LevelRequired, source, reason
		return decision
	}

	for _, repo := range AlwaysReviewRepos {
		if strings.EqualFold(strings.TrimSpace(in.Repo), repo) {
			return required("repo", repo+" always requires review before merge")
		}
	}
	var fixed []string
	for _, file := range in.Files {
		if FixedPathPattern.MatchString(file.Path) {
			fixed = append(fixed, file.Path)
		}
	}
	if len(fixed) > 0 {
		if len(fixed) > 3 {
			fixed = fixed[:3]
		}
		return required("path", "fixed path: "+strings.Join(fixed, ", "))
	}
	if judge == nil {
		return required("classifier_unavailable", "no JEV API key configured")
	}

	exchange, err := judge.Evaluate(ctx, jev.Request{Model: model, State: state, Questions: Questions()})
	if err != nil {
		return required("classifier_error", err.Error())
	}
	risk, riskErr := riskAnswer(exchange.Response)
	level, levelErr := levelAnswer(exchange.Response)
	if err := errors.Join(riskErr, levelErr); err != nil {
		return required("classifier_error", err.Error())
	}
	decision.RiskProbability = &risk
	decision.JEVChoice = level.Choice
	decision.Probabilities = level.Probabilities

	switch {
	case risk > RiskThreshold:
		return required("risk", fmt.Sprintf("high-impact category (p=%.2f)", risk))
	case level.Choice == levelUncertain:
		return required("uncertain", "JEV could not decide from the available evidence")
	case level.Choice == LevelNoReview && !complete:
		decision.Level, decision.Source, decision.Reason = LevelBackground, "truncated", "diff was truncated, so no-review is not allowed"
		return decision
	}
	decision.Level, decision.Source = level.Choice, "jev"
	decision.Reason = fmt.Sprintf("impact judged by JEV (risk p=%.2f)", risk)
	return decision
}

func riskAnswer(response jev.Response) (float64, error) {
	answer, ok := response.Answers[questionRisk]
	if !ok {
		return 0, fmt.Errorf("%s answer missing", questionRisk)
	}
	if answer.Noul == nil || !(*answer.Noul >= 0 && *answer.Noul <= 1) {
		return 0, fmt.Errorf("%s answer has no probability in [0,1]", questionRisk)
	}
	return *answer.Noul, nil
}

func levelAnswer(response jev.Response) (jev.Answer, error) {
	answer, ok := response.Answers[questionLevel]
	if !ok {
		return answer, fmt.Errorf("%s answer missing", questionLevel)
	}
	switch answer.Choice {
	case LevelNoReview, LevelBackground, LevelRequired, levelUncertain:
		return answer, nil
	}
	return answer, fmt.Errorf("%s answer %q is not an offered option", questionLevel, answer.Choice)
}

// BuildState renders the model input. Every file path is always listed in
// "files"; if the diff exceeds StateBudgetBytes each file gets a fair share of
// the budget, smallest first, so one huge file cannot hide the others. A file
// whose share cannot even hold the truncation marker is left out of "diff"
// (its path is still in "files"), so the diff never exceeds the budget.
func BuildState(in Input) (map[string]any, bool) {
	complete := true
	texts := make([]string, len(in.Files))
	names := make([]string, len(in.Files))
	total := 0
	for i, file := range in.Files {
		if file.Patch == "" {
			complete = false
		}
		added, deleted := 0, 0
		for _, line := range strings.Split(file.Patch, "\n") {
			switch {
			case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
			case strings.HasPrefix(line, "+"):
				added++
			case strings.HasPrefix(line, "-"):
				deleted++
			}
		}
		names[i] = fmt.Sprintf("%s (+%d/-%d)", file.Path, added, deleted)
		texts[i] = "diff --git a/" + file.Path + " b/" + file.Path + "\n" + file.Patch
		total += len(texts[i])
	}
	if total > StateBudgetBytes {
		complete = false
		order := make([]int, len(texts))
		for i := range order {
			order[i] = i
		}
		sort.SliceStable(order, func(a, b int) bool { return len(texts[order[a]]) < len(texts[order[b]]) })
		remaining := StateBudgetBytes
		for n, index := range order {
			share := remaining / (len(order) - n)
			if len(texts[index]) > share {
				if share < len(truncatedMarker) {
					texts[index] = ""
				} else {
					texts[index] = texts[index][:share-len(truncatedMarker)] + truncatedMarker
				}
			}
			remaining -= len(texts[index])
		}
	}
	return map[string]any{
		"repo":         in.Repo,
		"pr":           in.PR,
		"title":        in.Title,
		"head":         in.HeadSHA,
		"files":        names,
		"diffComplete": complete,
		"diff":         strings.Join(texts, ""),
	}, complete
}

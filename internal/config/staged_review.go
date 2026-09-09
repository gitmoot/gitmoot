package config

import (
	"fmt"
	"os"
	"strings"
)

// StagedReview is one per-repo staged-review declaration parsed from a
// [repos."owner/repo"] config section (#1821).
//
// A staged review splits one review into two chained job stages: a cheap
// preflight parent that answers "can this review be performed here", and a
// verdict child whose result is the review. The child runs on the STRONG
// reviewer, and which agent that is has to be an operator's decision rather
// than a constant in this repository - the owner's ruling on #1821 - because
// the answer differs per repo and changes as runtimes come and go.
//
// UNDECLARED MEANS REFUSE, NOT PICK-SOMETHING. There is no default and no
// fallback list. That is the same non-fallback clause the rest of the campaign
// enforces: a strong reviewer that cannot be identified must never degrade to
// "the cheap stage approved it". A repo with no declaration simply cannot
// dispatch a staged review, and the refusal says so.
type StagedReview struct {
	// Repo is the full name "owner/repo" the section keys on.
	Repo string
	// VerdictAgent names the registered agent that runs the VERDICT stage. Empty
	// (missing) means this repo has not declared one, and a staged review
	// dispatch for it is refused rather than defaulted.
	VerdictAgent string
}

// LoadStagedReview collects every [repos."owner/repo"] staged-review
// declaration from the config file, in config order.
//
// OFF BY DEFAULT: a config with no declaration returns an empty slice and never
// errors, so a repo with no section keeps today's single-stage review behaviour
// byte-identically.
func LoadStagedReview(paths Paths) ([]StagedReview, error) {
	content, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		return nil, err
	}
	collected := map[string]*StagedReview{}
	order := make([]string, 0)
	var current *StagedReview
	for _, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(stripConfigComment(raw))
		if line == "" {
			continue
		}
		if section, ok := sectionHeader(line); ok {
			current = nil
			repo, ok := parseRepoConcurrencySection(section)
			if !ok {
				continue
			}
			if collected[repo] == nil {
				collected[repo] = &StagedReview{Repo: repo}
				order = append(order, repo)
			}
			current = collected[repo]
			continue
		}
		if current == nil {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if strings.TrimSpace(key) != "staged_review_verdict_agent" {
			continue
		}
		parsed, err := parseConfigString(strings.TrimSpace(value))
		if err != nil {
			return nil, fmt.Errorf("staged review [repos.%q]: %w", current.Repo, err)
		}
		current.VerdictAgent = parsed
	}
	entries := make([]StagedReview, 0, len(order))
	for _, repo := range order {
		entry := collected[repo]
		entry.Repo = strings.TrimSpace(entry.Repo)
		entry.VerdictAgent = strings.TrimSpace(entry.VerdictAgent)
		if entry.VerdictAgent == "" {
			// A section that declared nothing is not a declaration. Dropping it here
			// keeps "declared" and "has a section" from being confusable by any
			// caller, which is the distinction the refusal depends on.
			continue
		}
		entries = append(entries, *entry)
	}
	return entries, nil
}

// StagedReviewVerdictAgent reports the declared verdict-stage agent for a repo
// and whether one was declared at all.
//
// THE BOOL MEANS "a usable agent name was declared", and nothing finer. An
// absent declaration and an explicitly empty one BOTH return declared=false,
// deliberately: LoadStagedReview drops the empty case, so the two are
// indistinguishable by the time a caller sees them.
//
// That collapse is the fail-safe direction - staging is off by default and an
// empty name cannot dispatch anything, so treating it as undeclared refuses
// rather than half-configures. The cost is that a typo which empties the value
// reads as "never configured".
//
// #2026's review raised this as a P3: the prior version of this comment claimed
// the bool separated those two cases and called the second a config error, which
// the implementation does not do. THE COMMENT WAS WRONG, NOT THE CODE, and it is
// corrected here rather than the behaviour changed.
//
// Callers must still branch on the bool rather than on emptiness, or an
// undeclared repo silently becomes a repo with an unnamed agent.
func StagedReviewVerdictAgent(paths Paths, repo string) (string, bool, error) {
	wanted := strings.TrimSpace(repo)
	if wanted == "" {
		return "", false, nil
	}
	entries, err := LoadStagedReview(paths)
	if err != nil {
		return "", false, err
	}
	for _, entry := range entries {
		// Exact comparison, matching every other [repos."owner/repo"] lookup in
		// this package (#2026 review, P3). EqualFold here would be a SECOND
		// convention for the same config section: RepoConcurrencyFor compares
		// `entry.Repo == repo`, and the require-workflow, merge-gate and review
		// loaders match exactly too. Case-insensitive repo matching may well be
		// the better rule - a GitHub owner/repo is case-insensitive in practice -
		// but changing it in ONE loader gives the same section two behaviours
		// depending on which key you read, which is worse than either rule
		// applied consistently. If it should change it should change repo-wide.
		if entry.Repo == wanted {
			return entry.VerdictAgent, true, nil
		}
	}
	return "", false, nil
}

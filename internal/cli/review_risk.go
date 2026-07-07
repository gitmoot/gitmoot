package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/github"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// loadReviewPolicy reads the [review] section for a `home` that may be either an
// already-resolved <home>/.gitmoot root or a raw --home (resolveConfigFile
// handles both). It fails safe to the default (risk tiers OFF) so a missing or
// malformed config never turns the opt-in path on or breaks the daemon.
func loadReviewPolicy(home string) config.ReviewPolicy {
	cfg := resolveConfigFile(home)
	if cfg == "" {
		return config.DefaultReviewPolicy()
	}
	policy, err := config.LoadReviewPolicy(config.Paths{ConfigFile: cfg})
	if err != nil {
		return config.DefaultReviewPolicy()
	}
	return policy
}

// applyReviewPolicy copies the opt-in [review] risk-tiered review policy (#650)
// onto the engine. With risk tiers off (the default) it sets RiskTiersEnabled
// false and the engine's review fan-out is byte-identical, so calling it
// unconditionally at engine construction is safe.
func applyReviewPolicy(engine *workflow.Engine, home string) {
	policy := loadReviewPolicy(home)
	engine.RiskTiersEnabled = policy.RiskTiersEnabled
	engine.HighRiskPaths = policy.HighRiskPaths
	engine.RiskLabelHigh = policy.RiskLabelHigh
	engine.RiskLabelRoutine = policy.RiskLabelRoutine
}

// wireReviewRiskSignals attaches the best-effort PR-signals resolver (#650) that
// HandlePullRequestOpened uses on the in-process implement->PR trigger to classify
// risk (labels + changed paths). It is a GitHub read, so it is wired ONLY when
// risk tiers are enabled to keep the default path free of any extra API call; when
// off the engine seam stays nil and behavior is byte-identical.
func wireReviewRiskSignals(engine *workflow.Engine, gh github.Client) {
	if engine == nil || !engine.RiskTiersEnabled || gh == nil {
		return
	}
	engine.PullRequestSignals = func(ctx context.Context, repo string, number int) ([]string, []string, error) {
		owner, name, ok := strings.Cut(strings.TrimSpace(repo), "/")
		if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
			return nil, nil, fmt.Errorf("risk signals: invalid repo %q", repo)
		}
		r := github.Repository{Owner: owner, Name: name}
		pr, err := gh.GetPullRequest(ctx, r, int64(number))
		if err != nil {
			return nil, nil, err
		}
		labels := pr.LabelNames()
		files, err := gh.ListPullRequestFiles(ctx, r, int64(number))
		if err != nil {
			// Labels alone still classify (a label wins over paths); a changed-files
			// lookup failure must not block the review.
			return labels, nil, nil
		}
		paths := make([]string, 0, len(files))
		for _, f := range files {
			if n := strings.TrimSpace(f.Filename); n != "" {
				paths = append(paths, n)
			}
		}
		return labels, paths, nil
	}
}

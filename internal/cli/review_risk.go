package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// loadReviewConfig reads the global and repository-scoped [review] settings for
// a `home` that may be either an already-resolved <home>/.gitmoot root or a raw
// --home. It fails safe to the default: native fanout and risk tiers both off.
func loadReviewConfig(home string) config.ReviewConfig {
	cfg := resolveConfigFile(home)
	if cfg == "" {
		return config.ReviewConfig{Global: config.DefaultReviewPolicy()}
	}
	policy, err := config.LoadReviewConfig(config.Paths{ConfigFile: cfg})
	if err != nil {
		return config.ReviewConfig{Global: config.DefaultReviewPolicy()}
	}
	return policy
}

// applyReviewPolicy copies the global risk-tier policy and installs the
// repository-aware native-fanout resolver onto the engine.
func applyReviewPolicy(engine *workflow.Engine, home string) {
	cfg := loadReviewConfig(home)
	policy := cfg.For("")
	engine.NativeReviewFanoutEnabled = func(repo string) bool {
		return cfg.For(repo).NativeFanoutEnabled
	}
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

package cli

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/gitmoot/gitmoot/internal/config"
	gitutil "github.com/gitmoot/gitmoot/internal/git"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/subprocess"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// loadReviewConfig reads the global and repository-scoped [review] settings for
// a `home` that may be either an already-resolved <home>/.gitmoot root or a raw
// --home. It fails safe to the defaults: blocking severity P3, with native
// fanout and risk tiers both off.
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
// repository-aware native-fanout and blocking-severity resolvers onto the engine.
func applyReviewPolicy(engine *workflow.Engine, home string) {
	cfg := loadReviewConfig(home)
	policy := cfg.For("")
	engine.NativeReviewFanoutEnabled = func(repo string) bool {
		return cfg.For(repo).NativeFanoutEnabled
	}
	engine.ReviewBlockingSeverity = func(repo string) string {
		return cfg.For(repo).BlockingSeverity
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

// wireReviewChangedFiles installs the engine seam that scopes an incremental
// review to the files a follow-up range actually changed.
//
// Completeness is the whole difficulty: an INCOMPLETE list silently
// under-reviews, so the seam must only ever return a list it can prove whole.
// No hosted compare response can supply that proof (see
// github.compareFilesCap), so the primary instrument is the daemon's own
// checkout, where `git diff -z --name-status base...head` is uncapped and
// unambiguous. The API remains the fallback for a daemon with no checkout or a
// range whose objects cannot be fetched, and there it fails CLOSED on a capped
// page: a conservative ReviewScopeUnavailableError costs one unscoped full
// review at the same head, which HandlePullRequestOpened already degrades to.
func wireReviewChangedFiles(engine *workflow.Engine, gh github.Client, checkout string, runner subprocess.Runner) {
	if engine == nil {
		return
	}
	checkout = strings.TrimSpace(checkout)
	if gh == nil && checkout == "" {
		return
	}
	engine.ReviewChangedFiles = func(ctx context.Context, repo string, number int, previousHead string, currentHead string) ([]string, error) {
		owner, name, ok := strings.Cut(strings.TrimSpace(repo), "/")
		if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
			return nil, fmt.Errorf("review scope: invalid repo %q", repo)
		}
		base, head := strings.TrimSpace(previousHead), strings.TrimSpace(currentHead)
		if checkout != "" {
			paths, err := localReviewChangedFiles(ctx, jobGitClient(checkout, runner), number, base, head)
			if err == nil {
				return paths, nil
			}
			var unavailable workflow.ReviewScopeUnavailableError
			if errors.As(err, &unavailable) {
				// The local objects PROVE the range is not a direct follow-up.
				// The API cannot overturn a proof, so do not spend a call on it.
				return nil, err
			}
			// The instrument could not RUN (cold checkout, refused fetch, git
			// failure). Fall through to the API, which fails closed on a capped
			// page rather than guessing at the remainder.
		}
		if gh == nil {
			return nil, workflow.ReviewScopeUnavailableError{
				Reason: "review scope has no local checkout and no GitHub client",
			}
		}
		compare, err := gh.CompareCommits(ctx, github.Repository{Owner: owner, Name: name}, base, head)
		if err != nil {
			return nil, err
		}
		switch strings.TrimSpace(compare.Status) {
		case "ahead", "identical":
		default:
			return nil, workflow.ReviewScopeUnavailableError{
				Reason: fmt.Sprintf("review scope compare is %q, not a direct follow-up", compare.Status),
			}
		}
		// A capped page is not a complete list and nothing in the response can
		// make it one, so a range at or past the cap is unscopable from the API
		// alone however large the returned array looks.
		if compare.Truncated {
			return nil, workflow.ReviewScopeUnavailableError{
				Reason: fmt.Sprintf(
					"review scope compare file list is capped at %d files and no local checkout could prove the range complete",
					len(compare.Files)),
			}
		}
		paths := make([]string, 0, len(compare.Files))
		for _, file := range compare.Files {
			paths = append(paths, file.Filename)
		}
		return sortedUniqueReviewPaths(paths), nil
	}
}

// localReviewChangedFiles enumerates base...head from the daemon's own checkout.
//
// A ReviewScopeUnavailableError return is a PROOF that the range is unscopable
// (the reviewed head is not an ancestor of the current head, so the range is not
// a direct follow-up — the local equivalent of a "behind"/"diverged" compare).
// Any other error means the instrument could not run and the caller falls back.
func localReviewChangedFiles(ctx context.Context, git gitutil.Client, number int, base string, head string) ([]string, error) {
	if base == "" || head == "" {
		return nil, errors.New("review scope: local enumeration needs both heads")
	}
	if err := ensureReviewScopeCommits(ctx, git, number, base, head); err != nil {
		return nil, err
	}
	// `git merge-base --is-ancestor X X` succeeds, so this also accepts the
	// identical-heads range the compare endpoint reports as "identical".
	ancestor, err := git.IsAncestor(ctx, base, head)
	if err != nil {
		return nil, err
	}
	if !ancestor {
		return nil, workflow.ReviewScopeUnavailableError{
			Reason: fmt.Sprintf(
				"review scope %s...%s is not a direct follow-up: the reviewed head is not an ancestor of the current head",
				base, head),
		}
	}
	files, err := git.ChangedFiles(ctx, base, head)
	if err != nil {
		return nil, err
	}
	return sortedUniqueReviewPaths(files), nil
}

// ensureReviewScopeCommits makes both range endpoints present in the local
// object database, fetching the PR head ref once the way the review dispatch
// path already does when a cold checkout lacks the reviewed commit. A commit
// still missing afterwards is an error, never an empty enumeration.
func ensureReviewScopeCommits(ctx context.Context, git gitutil.Client, number int, refs ...string) error {
	missing, err := missingReviewScopeCommits(ctx, git, refs)
	if err != nil || len(missing) == 0 {
		return err
	}
	if number > 0 {
		if err := git.FetchPullRequest(ctx, "", number); err != nil {
			return fmt.Errorf("review scope: fetch pull/%d/head: %w", number, err)
		}
	} else if err := git.FetchRemote(ctx, ""); err != nil {
		return fmt.Errorf("review scope: fetch origin: %w", err)
	}
	if missing, err = missingReviewScopeCommits(ctx, git, refs); err != nil {
		return err
	}
	if len(missing) > 0 {
		return fmt.Errorf("review scope: commits missing from the local checkout: %s", strings.Join(missing, ", "))
	}
	return nil
}

func missingReviewScopeCommits(ctx context.Context, git gitutil.Client, refs []string) ([]string, error) {
	var missing []string
	for _, ref := range refs {
		present, err := git.CommitExists(ctx, ref)
		if err != nil {
			return nil, err
		}
		if !present {
			missing = append(missing, ref)
		}
	}
	return missing, nil
}

// sortedUniqueReviewPaths is the review scope's canonical shape: deduplicated,
// blank-free and sorted, so a scope is byte-comparable across rounds regardless
// of which instrument produced it.
func sortedUniqueReviewPaths(paths []string) []string {
	seen := make(map[string]struct{}, len(paths))
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		if _, exists := seen[path]; exists {
			continue
		}
		seen[path] = struct{}{}
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

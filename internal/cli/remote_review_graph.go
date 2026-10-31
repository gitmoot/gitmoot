package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/gitmoot/gitmoot/internal/github"
)

// The authenticated forge supplies the same identity/ancestry answers as local
// Git. Comparison failures remain unknown, matching the existing citation rule.
type remoteReviewGraph struct {
	client      github.Client
	repo        github.Repository
	head        string
	comparisons map[string]string
}

func (g *remoteReviewGraph) RevParse(ctx context.Context, ref string) (string, error) {
	ref = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(ref), "^{commit}"))
	if ref == g.head {
		return g.head, nil
	}
	comparison, err := g.client.CompareCommits(ctx, g.repo, ref, g.head)
	if err != nil {
		return "", err
	}
	resolved := strings.ToLower(strings.TrimSpace(comparison.BaseCommit.SHA))
	if resolved == "" {
		return "", fmt.Errorf("forge comparison did not resolve citation %s", ref)
	}
	if g.comparisons == nil {
		g.comparisons = make(map[string]string)
	}
	g.comparisons[resolved] = comparison.Status
	return resolved, nil
}

func (g *remoteReviewGraph) IsAncestor(ctx context.Context, ancestor, descendant string) (bool, error) {
	if ancestor == descendant {
		return true, nil
	}
	status, known := g.comparisons[ancestor]
	if !known || descendant != g.head {
		comparison, err := g.client.CompareCommits(ctx, g.repo, ancestor, descendant)
		if err != nil {
			return false, err
		}
		status = comparison.Status
	}
	switch status {
	case "identical", "ahead":
		return true, nil
	case "behind", "diverged":
		return false, nil
	default:
		return false, fmt.Errorf("forge comparison returned unknown relationship %q", status)
	}
}

func (g *remoteReviewGraph) ChangedFiles(ctx context.Context, _ int, base, head string) ([]string, error) {
	return forgeReviewChangedFiles(ctx, g.client, g.repo, base, head)
}

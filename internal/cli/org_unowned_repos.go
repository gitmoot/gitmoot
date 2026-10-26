package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/doctor"
)

const (
	orgUnownedReposDoctorCheckName = "org repo ownership"
	// orgUnownedReposWindow bounds the repos checked to ones with recent work.
	orgUnownedReposWindow = 7 * 24 * time.Hour
)

// unownedRepo is a repo with recent jobs and no single org owner. Tied names
// the roles that match it equally; empty Tied means no role matches at all.
type unownedRepo struct {
	Repo string
	Tied []string
}

func (r unownedRepo) String() string {
	if len(r.Tied) > 1 {
		return fmt.Sprintf("%s (tied: %s)", r.Repo, strings.Join(r.Tied, ", "))
	}
	return r.Repo + " (no role)"
}

// findUnownedRepos lists repos with jobs since the cutoff that resolve to no
// single owner. It uses the same resolver as blocked and verdict routing, so a
// repo listed here is exactly one whose role-less notices reach no owner
// (#2363). Listing a repo never assigns it.
func findUnownedRepos(ctx context.Context, store *db.Store, cfg config.OrgConfig, since time.Time) ([]unownedRepo, error) {
	repos, err := store.ListJobReposSince(ctx, since)
	if err != nil {
		return nil, err
	}
	roster := loadOrgRoster(ctx, store, cfg)
	var unowned []unownedRepo
	for _, repo := range repos {
		resolution := resolveRepoOrgOwner(roster, cfg, repo)
		if resolution.Owner == "" {
			unowned = append(unowned, unownedRepo{Repo: repo, Tied: resolution.Tied})
		}
	}
	return unowned, nil
}

func formatUnownedRepos(unowned []unownedRepo) string {
	parts := make([]string, len(unowned))
	for i, repo := range unowned {
		parts[i] = repo.String()
	}
	return strings.Join(parts, ", ")
}

// unownedReposWarning is the org status line for unowned repos, or "" when
// every recently active repo has a single owner.
func unownedReposWarning(ctx context.Context, store *db.Store, cfg config.OrgConfig, now time.Time) string {
	if !cfg.Enabled() || store == nil {
		return ""
	}
	unowned, err := findUnownedRepos(ctx, store, cfg, now.Add(-orgUnownedReposWindow))
	if err != nil {
		return fmt.Sprintf("repo ownership unverified: %v", err)
	}
	if len(unowned) == 0 {
		return ""
	}
	return fmt.Sprintf("%d repo(s) with jobs in the last 7d have no single org owner, so their role-less notices reach observers only: %s. Add each repo to its owning seat's scope.",
		len(unowned), formatUnownedRepos(unowned))
}

// orgUnownedReposDoctorCheck reports repos with recent jobs and no single org
// owner. It is a warning, never a required failure.
func orgUnownedReposDoctorCheck(paths config.Paths) (doctor.Check, bool) {
	cfg, err := config.LoadOrg(paths)
	if err != nil || !cfg.Enabled() {
		return doctor.Check{}, false
	}
	if strings.TrimSpace(paths.Database) == "" {
		return doctor.Check{}, false
	}
	store, err := db.OpenReadOnly(paths.Database)
	if err != nil {
		return doctor.Check{Name: orgUnownedReposDoctorCheckName, Detail: fmt.Sprintf("repo ownership unverified: open store: %v", err)}, true
	}
	defer store.Close()
	unowned, err := findUnownedRepos(context.Background(), store, cfg, time.Now().UTC().Add(-orgUnownedReposWindow))
	if err != nil {
		return doctor.Check{Name: orgUnownedReposDoctorCheckName, Detail: fmt.Sprintf("repo ownership unverified: %v", err)}, true
	}
	if len(unowned) > 0 {
		return doctor.Check{
			Name:   orgUnownedReposDoctorCheckName,
			Detail: "repos with recent jobs and no single org owner: " + formatUnownedRepos(unowned) + "; add each to its owning seat's scope",
		}, true
	}
	return doctor.Check{Name: orgUnownedReposDoctorCheckName, OK: true, Detail: "every repo with jobs in the last 7d has a single org owner"}, true
}

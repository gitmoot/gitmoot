package cli

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/hub"
)

// hubHandler serves one authenticated hub request for caller, whose role is
// in org. It returns the HTTP status and the JSON value, or an error message.
type hubHandler func(ctx context.Context, caller hub.Caller, org config.OrgConfig, r *http.Request) (int, any, error)

var hubFullSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// hubReviewPurposes are the purposes `review request --purpose` documents.
var hubReviewPurposes = map[string]bool{"code": true, "security": true, "ui": true, "architecture": true}

// registerHubWorkEndpoints adds the endpoints beyond messages (#2710):
//
//	GET  /v1/org/brief                    org brief for the caller's role
//	GET  /v1/tasks/{ref}?repo=OWNER/REPO  task show
//	POST /v1/reviews/level {"repo","pr"}  review level
//	POST /v1/reviews {"repo","pr","head_sha","message",...} review request
//	POST /v1/notifications/claim {"runtime","event"}        turn-hook claim
//
// Every repository must be inside the caller role's org scope. Review
// requests take no host paths and no runtime or execution overrides: those
// stay on the hub machine.
func registerHubWorkEndpoints(handle func(string, hubHandler), home string, store *db.Store) {
	handle("GET /v1/org/brief", func(ctx context.Context, caller hub.Caller, org config.OrgConfig, _ *http.Request) (int, any, error) {
		role, _ := org.Role(caller.Role)
		if err := store.TouchOrgRolePresence(ctx, role.Name, "org brief (hub: "+caller.Machine.Name+")"); err != nil {
			return http.StatusInternalServerError, nil, errors.New("presence unavailable")
		}
		rows, err := store.ListOrgRolePresence(ctx)
		if err != nil {
			return http.StatusInternalServerError, nil, errors.New("presence unavailable")
		}
		presence := make(map[string]db.OrgRolePresence, len(rows))
		for _, row := range rows {
			presence[row.Role] = row
		}
		snapshot, snapshotErr := orgProviderSnapshot(ctx, org)
		return http.StatusOK, buildOrgBriefOutput(org, presence, role, snapshot, snapshotErr), nil
	})
	handle("GET /v1/tasks/{ref}", func(ctx context.Context, caller hub.Caller, org config.OrgConfig, r *http.Request) (int, any, error) {
		record, err := resolvePRRecord(ctx, store, r.PathValue("ref"), strings.TrimSpace(r.URL.Query().Get("repo")))
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) || strings.Contains(err.Error(), "not found") {
				return http.StatusNotFound, nil, err
			}
			return http.StatusBadRequest, nil, err
		}
		if status, err := hubRequireScope(org, caller.Role, record.Repo); err != nil {
			return status, nil, err
		}
		return http.StatusOK, record, nil
	})
	handle("POST /v1/reviews/level", func(ctx context.Context, caller hub.Caller, org config.OrgConfig, r *http.Request) (int, any, error) {
		var input struct {
			Repo string `json:"repo"`
			PR   int    `json:"pr"`
		}
		if err := decodeHubBody(r, &input); err != nil {
			return http.StatusBadRequest, nil, err
		}
		repo, err := github.ParseRepository(input.Repo)
		if err != nil || input.PR <= 0 {
			return http.StatusBadRequest, nil, errors.New("repo OWNER/REPO and pr > 0 are required")
		}
		if status, err := hubRequireScope(org, caller.Role, repo.FullName()); err != nil {
			return status, nil, err
		}
		// Attribution first: the decision posts a commit status and writes the
		// level log, which cannot share a transaction with hub_writes.
		ref := fmt.Sprintf("level:%s#%d", repo.FullName(), input.PR)
		if err := store.RecordHubWrite(ctx, caller.Machine.ID, caller.Role, "review.level", ref, time.Now().UTC()); err != nil {
			return http.StatusInternalServerError, nil, errors.New("could not record the hub write; nothing was decided")
		}
		var warnings bytes.Buffer
		decision, err := decideReviewLevel(ctx, home, repo, input.PR, &warnings)
		if err != nil {
			return http.StatusBadGateway, nil, fmt.Errorf("review level: %w", err)
		}
		return http.StatusOK, map[string]any{"decision": decision, "warnings": hubWarnings(warnings)}, nil
	})
	handle("POST /v1/reviews", func(ctx context.Context, caller hub.Caller, org config.OrgConfig, r *http.Request) (int, any, error) {
		var input struct {
			Repo        string `json:"repo"`
			PR          int    `json:"pr"`
			HeadSHA     string `json:"head_sha"`
			Message     string `json:"message"`
			Purpose     string `json:"purpose"`
			Reviewer    string `json:"reviewer"`
			Model       string `json:"model"`
			PostMerge   bool   `json:"post_merge"`
			NoExecution bool   `json:"no_execution"`
		}
		if err := decodeHubBody(r, &input); err != nil {
			return http.StatusBadRequest, nil, err
		}
		repo, err := github.ParseRepository(input.Repo)
		if err != nil || input.PR <= 0 {
			return http.StatusBadRequest, nil, errors.New("repo OWNER/REPO and pr > 0 are required")
		}
		head := strings.ToLower(strings.TrimSpace(input.HeadSHA))
		if !hubFullSHA.MatchString(head) {
			return http.StatusBadRequest, nil, errors.New("head_sha must be the full 40-character commit SHA")
		}
		purpose := strings.ToLower(strings.TrimSpace(input.Purpose))
		if purpose == "" {
			purpose = "code"
		}
		if !hubReviewPurposes[purpose] {
			return http.StatusBadRequest, nil, errors.New("purpose must be code, security, ui or architecture")
		}
		if status, err := hubRequireScope(org, caller.Role, repo.FullName()); err != nil {
			return status, nil, err
		}
		// Attribution first, for the same reason as review level.
		ref := fmt.Sprintf("review:%s#%d@%s", repo.FullName(), input.PR, head)
		if err := store.RecordHubWrite(ctx, caller.Machine.ID, caller.Role, "review.request", ref, time.Now().UTC()); err != nil {
			return http.StatusInternalServerError, nil, errors.New("could not record the hub write; no review was requested")
		}
		var warnings bytes.Buffer
		output, err := requestReview(ctx, store, reviewRequestOptions{
			home: home, repo: repo.FullName(), pr: input.PR, head: head, purpose: purpose,
			role: caller.Role, ttl: defaultReviewRequestTTL, message: input.Message,
			reviewer: strings.TrimSpace(input.Reviewer), model: strings.TrimSpace(input.Model),
			postMerge: input.PostMerge, noExecution: input.NoExecution, json: true,
		}, &warnings)
		if err != nil {
			return http.StatusBadRequest, nil, fmt.Errorf("review request: %w", err)
		}
		return http.StatusCreated, map[string]any{"review": output, "warnings": hubWarnings(warnings)}, nil
	})
	handle("POST /v1/notifications/claim", func(ctx context.Context, caller hub.Caller, _ config.OrgConfig, r *http.Request) (int, any, error) {
		var input struct {
			Runtime string `json:"runtime"`
			Event   string `json:"event"`
		}
		if err := decodeHubBody(r, &input); err != nil {
			return http.StatusBadRequest, nil, err
		}
		request := turnHookRequest{runtime: strings.ToLower(strings.TrimSpace(input.Runtime)), event: strings.TrimSpace(input.Event)}
		if err := request.validate(); err != nil {
			return http.StatusBadRequest, nil, err
		}
		// Attribution first: a claim delivers rows, which cannot be undone.
		if err := store.RecordHubWrite(ctx, caller.Machine.ID, caller.Role, "notifications.claim", request.runtime+":"+request.event, time.Now().UTC()); err != nil {
			return http.StatusInternalServerError, nil, errors.New("could not record the hub write; nothing was claimed")
		}
		start := turnHookNow()
		claimCtx, cancel := context.WithDeadline(ctx, start.Add(turnHookClaimBudget))
		defer cancel()
		var warnings bytes.Buffer
		output, err := claimTurnHookForRole(claimCtx, start, store, caller.Role, request, &warnings)
		if err != nil {
			return http.StatusInternalServerError, nil, fmt.Errorf("claim notifications: %w", err)
		}
		return http.StatusOK, map[string]any{"hook_output": output, "warnings": hubWarnings(warnings)}, nil
	})
}

// hubRequireScope refuses a repository outside the role's org scope.
func hubRequireScope(org config.OrgConfig, roleName, repo string) (int, error) {
	role, ok := org.Role(roleName)
	if !ok || !config.ScopeMatches(role.Scope, repo) {
		return http.StatusForbidden, fmt.Errorf("%s is outside role %s's scope", repo, roleName)
	}
	return 0, nil
}

func hubWarnings(buffer bytes.Buffer) []string {
	var lines []string
	for _, line := range strings.Split(buffer.String(), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

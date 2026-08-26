package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
)

// The #1635 archived-seat ingest: the ONE write site for the org_role_archived
// mirror. It runs inside the daemon's one-minute org lane, strictly after the
// herdr availability gate, and mutates the mirror only after a SUCCESSFUL
// `herdr agent list` read (bare invocation — JSON is that command's default
// output; the contract's original `--json` spelling errors on deployed herdr
// and was amended, workflow note 85577).
//
// Fail direction, chosen deliberately (PLAN v2, note 85481): a failed read
// leaves the mirror untouched, so herdr-down PRESERVES exclusion. A seat
// unarchived during an outage stays excluded until the next successful poll —
// visible and recoverable — while the inverse (sweeping an archived seat)
// is the confusion herdrup#173 exists to end. Staleness goes LOUD instead:
// org_archive_poll.last_success_at is written only on success and the doctor
// warns when it ages while archived rows exist.

// herdrArchiveListTimeout bounds one agent-list read; the lane runs every
// minute, so a hung herdr must not stack invocations.
const herdrArchiveListTimeout = 10 * time.Second

// orgArchiveStaleAfter is how old the last successful poll may be, while
// archived rows exist, before the doctor goes loud. ~15 missed one-minute
// lane ticks.
const orgArchiveStaleAfter = 15 * time.Minute

func defaultHerdrAgentList(ctx context.Context) ([]byte, error) {
	bounded, cancel := context.WithTimeout(ctx, herdrArchiveListTimeout)
	defer cancel()
	return exec.CommandContext(bounded, "herdr", "agent", "list").Output()
}

// herdrAgentListEnvelope is the bare `herdr agent list` output shape
// (confirmed contract + live sample, notes 85540/85567).
type herdrAgentListEnvelope struct {
	Result struct {
		Agents []herdrAgentListEntry `json:"agents"`
	} `json:"result"`
}

type herdrAgentListEntry struct {
	Name     string                 `json:"name"`
	Archived *herdrArchivedBlock    `json:"archived"`
	Parked   json.RawMessage        `json:"parked_work"`
	Extra    map[string]interface{} `json:"-"`
}

type herdrArchivedBlock struct {
	At     time.Time `json:"at"`
	By     string    `json:"by"`
	Reason string    `json:"reason"`
}

// parseHerdrArchivedAgents extracts the archived seats from an agent-list
// read. PRESENCE of the archived block means archived; absence means active
// (key off the block, never agent_status — herdr-app keeps status `idle` on
// archived entries by design). The join key is the agent NAME, which matches
// gitmoot's role name / pane binding. parked_work is kept verbatim.
func parseHerdrArchivedAgents(raw []byte, observedAt time.Time) (map[string]orgArchivedObservation, map[string]string, error) {
	var envelope herdrAgentListEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, nil, fmt.Errorf("parse herdr agent list: %w", err)
	}
	if envelope.Result.Agents == nil {
		return nil, nil, fmt.Errorf("herdr agent list returned no agents array; refusing to treat a malformed read as an empty fleet")
	}
	archived := map[string]orgArchivedObservation{}
	parked := map[string]string{}
	for _, agent := range envelope.Result.Agents {
		if agent.Archived == nil {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(agent.Name))
		if name == "" {
			continue
		}
		archived[name] = orgArchivedObservation{
			At:         agent.Archived.At,
			By:         agent.Archived.By,
			Reason:     agent.Archived.Reason,
			ObservedAt: observedAt.UTC(),
		}
		if len(agent.Parked) > 0 {
			parked[name] = string(agent.Parked)
		}
	}
	return archived, parked, nil
}

// refreshOrgArchiveMirror reconciles the mirror against one successful herdr
// read. Every mutation is caused by that read: upserts for observed archived
// seats, deletes + directive UNPARK for observed archived->active transitions
// (the successful read is what attests the transition — unparking is never
// driven by absence of data), directive PARK for the observed archived set
// (idempotent, so directives minted after archive time still get parked), and
// the poll-success stamp last. A failed or malformed read logs and changes
// nothing.
func refreshOrgArchiveMirror(ctx context.Context, store *db.Store, stdout io.Writer, now time.Time, list func(context.Context) ([]byte, error)) {
	if store == nil {
		return
	}
	// nil means the caller did not wire an agent-list source — refresh is
	// skipped entirely. The production default deps wire defaultHerdrAgentList;
	// resolving nil to the real binary HERE would make every injected-deps test
	// silently read the live herdr, which unit tests must never do.
	if list == nil {
		return
	}
	raw, err := list(ctx)
	if err != nil {
		writeLine(stdout, "org archive mirror: herdr agent list failed; mirror preserved (exclusions unchanged): %v", err)
		return
	}
	archived, parked, err := parseHerdrArchivedAgents(raw, now)
	if err != nil {
		writeLine(stdout, "org archive mirror: %v; mirror preserved (exclusions unchanged)", err)
		return
	}
	prior, err := store.ListOrgRolesArchived(ctx)
	if err != nil {
		writeLine(stdout, "org archive mirror: list mirror failed: %v", err)
		return
	}
	priorRoles := make(map[string]bool, len(prior))
	for _, row := range prior {
		priorRoles[row.Role] = true
	}
	names := make([]string, 0, len(archived))
	for name := range archived {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		observation := archived[name]
		if err := store.UpsertOrgRoleArchived(ctx, db.OrgRoleArchived{
			Role:       name,
			ArchivedAt: observation.At.UTC().Format(time.RFC3339Nano),
			ArchivedBy: observation.By,
			Reason:     observation.Reason,
			ParkedWork: parked[name],
			ObservedAt: observation.ObservedAt.Format(time.RFC3339Nano),
		}); err != nil {
			writeLine(stdout, "org archive mirror: upsert %s failed: %v", name, err)
			continue
		}
		if !priorRoles[name] {
			writeLine(stdout, "org seat archived observed: %s (at %s by %s)", name, observation.At.UTC().Format(time.RFC3339), observation.By)
		}
	}
	for _, row := range prior {
		if _, still := archived[row.Role]; still {
			continue
		}
		deleted, err := store.DeleteOrgRoleArchived(ctx, row.Role)
		if err != nil {
			writeLine(stdout, "org archive mirror: delete %s failed: %v", row.Role, err)
			continue
		}
		if deleted {
			unparked, err := store.UnparkOrgDirectivesForRole(ctx, row.Role, now)
			if err != nil {
				writeLine(stdout, "org seat unarchive observed: %s; directive unpark failed: %v", row.Role, err)
				continue
			}
			writeLine(stdout, "org seat unarchive observed: %s; %d directives unparked with fresh TTL anchors", row.Role, unparked)
		}
	}
	if n, err := parkOutstandingDirectivesForArchivedSeats(ctx, store, archived, now); err != nil {
		writeLine(stdout, "org archive mirror: directive park failed: %v", err)
	} else if n > 0 {
		writeLine(stdout, "org archive mirror: parked %d open directives for archived seats", n)
	}
	if err := store.RecordOrgArchivePollSuccess(ctx, now); err != nil {
		writeLine(stdout, "org archive mirror: poll-success stamp failed: %v", err)
	}
}

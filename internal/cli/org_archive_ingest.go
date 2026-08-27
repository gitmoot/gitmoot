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
// read. PRESENCE of the archived block means archived; absence of the block on
// a PRESENT agent means active (key off the block, never agent_status —
// herdr-app keeps status `idle` on archived entries by design). The join key
// is the agent NAME, which matches gitmoot's role name / pane binding.
// parked_work is kept verbatim. The returned present set carries EVERY agent
// name in the well-formed list: reconciliation needs it because an agent
// merely OMITTED from the list is a different fact from one present without
// the block — omission is not evidence of anything (#1643 review block 1).
func parseHerdrArchivedAgents(raw []byte, observedAt time.Time) (archived map[string]orgArchivedObservation, parked map[string]string, present map[string]bool, err error) {
	var envelope herdrAgentListEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, nil, nil, fmt.Errorf("parse herdr agent list: %w", err)
	}
	if envelope.Result.Agents == nil {
		return nil, nil, nil, fmt.Errorf("herdr agent list returned no agents array; refusing to treat a malformed read as an empty fleet")
	}
	archived = map[string]orgArchivedObservation{}
	parked = map[string]string{}
	present = map[string]bool{}
	for _, agent := range envelope.Result.Agents {
		name := strings.ToLower(strings.TrimSpace(agent.Name))
		if name == "" {
			continue
		}
		present[name] = true
		if agent.Archived == nil {
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
	return archived, parked, present, nil
}

// unarchiveOrgSeatTransition is a fault-injection seam over the ATOMIC store
// transition (unpark + mirror delete in one transaction, #1643 round 3): a
// forced failure must leave the archived state fully intact — row present,
// directives still parked — so the next positive-evidence tick retries the
// whole transition and no partial state can be stranded by a later omission.
var unarchiveOrgSeatTransition = func(ctx context.Context, store *db.Store, role string, at time.Time) (int64, error) {
	return store.UnarchiveOrgSeatTransition(ctx, role, at)
}

// parkOutstandingForArchived is a fault-injection seam over the park helper:
// the retry-under-omission test forces one park failure through it and then
// proves the next tick retries FROM THE MIRROR even when the role is omitted
// from the current list.
var parkOutstandingForArchived = parkOutstandingDirectivesForArchivedSeats

// deleteOrgArchivePending is a fault-injection seam over the drain's routine
// pending cleanup: the round-7 guard test fails it to prove a leftover pending
// row still dies inside the atomic transition rather than resurrecting a
// superseded observation on a later omission tick.
var deleteOrgArchivePending = func(ctx context.Context, store *db.Store, role string) error {
	return store.DeleteOrgArchivePending(ctx, role)
}

// upsertOrgRoleArchived is a fault-injection seam over the mirror upsert: the
// round-4 test forces the FIRST mirror write to fail and then proves the
// pending ledger retries it on a later tick even under list omission.
var upsertOrgRoleArchived = func(ctx context.Context, store *db.Store, row db.OrgRoleArchived) error {
	return store.UpsertOrgRoleArchived(ctx, row)
}

// refreshOrgArchiveMirror reconciles the mirror against one successful herdr
// read. Every mutation is caused by that read: upserts for observed archived
// seats, directive UNPARK + mirror delete for observed archived->active
// transitions, directive PARK for the observed archived set (idempotent, so
// directives minted after archive time still get parked), and the poll-success
// stamp last. A failed or malformed read logs and changes nothing.
//
// LIFTING an exclusion requires evidence, exactly as imposing one does
// (#1643 review block 1): a transition fires only when the agent is PRESENT
// in the well-formed list WITHOUT its archived block. An agent merely absent
// from the list is unknown — its mirror row and parked directives are
// preserved, and only a log line records the omission.
//
// Transitions run unpark-FIRST, delete-after (#1643 review block 2): a failed
// unpark leaves the mirror row, so the next tick retries (unpark is
// idempotent) instead of stranding parked directives forever. The poll stamp
// records success ONLY on a fully clean reconciliation — a tick with any
// failed write leaves the stamp alone so the staleness alarm can fire.
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
	archived, parked, present, err := parseHerdrArchivedAgents(raw, now)
	if err != nil {
		writeLine(stdout, "org archive mirror: %v; mirror preserved (exclusions unchanged)", err)
		return
	}
	// PHASE 1 (#1643 round 4): record every observed archived seat in the
	// durable PENDING ledger as the tick's FIRST write, in one transaction.
	// The observation must exist durably before any mirror write can fail —
	// otherwise a transient upsert failure followed by a valid list OMISSION
	// loses the only retry state and the loss is stamped clean. If this first
	// write itself fails, the tick aborts with the stamp withheld: the
	// failed-read equivalence, loud via staleness rather than silent.
	names := make([]string, 0, len(archived))
	observedRows := make([]db.OrgRoleArchived, 0, len(archived))
	for name := range archived {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		observation := archived[name]
		observedRows = append(observedRows, db.OrgRoleArchived{
			Role:       name,
			ArchivedAt: observation.At.UTC().Format(time.RFC3339Nano),
			ArchivedBy: observation.By,
			Reason:     observation.Reason,
			ParkedWork: parked[name],
			ObservedAt: observation.ObservedAt.Format(time.RFC3339Nano),
		})
	}
	if err := store.MergeOrgArchivePending(ctx, observedRows); err != nil {
		writeLine(stdout, "org archive mirror: observation record failed; tick aborted, stamp withheld: %v", err)
		return
	}
	prior, err := store.ListOrgRolesArchived(ctx)
	if err != nil {
		writeLine(stdout, "org archive mirror: list mirror failed: %v", err)
		return
	}
	failed := false
	priorRoles := make(map[string]bool, len(prior))
	for _, row := range prior {
		priorRoles[row.Role] = true
	}
	// PHASE 2: drain the pending ledger into the mirror — today's observations
	// AND any leftovers from earlier failed ticks, so the retry fires here
	// regardless of whether the role appears in today's list. A pending row is
	// deleted only after its mirror upsert succeeds.
	//
	// EVIDENCE RECENCY WINS (#1643 round 6, both families independently): a
	// pending observation CONTRADICTED by this tick's read — the agent PRESENT
	// WITHOUT its archived block — is STALE and is discarded, never applied.
	// Without this, the durable record outranks a live contradicting
	// observation and resurrects an archive the fleet has already left:
	// permanent wrongful exclusion under a green doctor. A drained pending
	// observation must never override positive evidence from the same or a
	// later tick.
	pending, err := store.ListOrgArchivePending(ctx)
	if err != nil {
		writeLine(stdout, "org archive mirror: list pending failed: %v", err)
		return
	}
	for _, row := range pending {
		// A pending row CONTRADICTED by this tick's read (agent present
		// without its block) is superseded through the ATOMIC transition
		// DIRECTLY — without ever touching the shipping mirror (#1643 round
		// 8, codex): the round-7 apply-then-transition path committed the
		// stale observation to org_role_archived for a window in which
		// concurrent readers could classify an actively observed seat as
		// archived. The transition transaction unparks, removes any mirror
		// row, and deletes the pending row as one write; a failure leaves the
		// pending row to re-derive the contradiction next tick (round 7's
		// property, kept), and success writes nothing a reader could misread.
		if _, stillArchived := archived[row.Role]; present[row.Role] && !stillArchived {
			if _, err := unarchiveOrgSeatTransition(ctx, store, row.Role, now); err != nil {
				writeLine(stdout, "org archive mirror: atomic supersede of stale pending %s failed, retried next tick: %v", row.Role, err)
				failed = true
				continue
			}
			writeLine(stdout, "org archive mirror: pending observation for %s superseded by fresher positive evidence (atomic)", row.Role)
			continue
		}
		if err := upsertOrgRoleArchived(ctx, store, row); err != nil {
			writeLine(stdout, "org archive mirror: upsert %s failed, retried next tick from pending: %v", row.Role, err)
			failed = true
			continue
		}
		if err := deleteOrgArchivePending(ctx, store, row.Role); err != nil {
			// The mirror row landed; a lingering pending row only means a
			// harmless re-upsert next tick. Still not a clean tick.
			writeLine(stdout, "org archive mirror: pending cleanup for %s failed: %v", row.Role, err)
			failed = true
		}
		if !priorRoles[row.Role] {
			writeLine(stdout, "org seat archived observed: %s (at %s by %s)", row.Role, row.ArchivedAt, row.ArchivedBy)
		}
	}
	// The transition loop reads POST-DRAIN state (the round-6 invariant): the
	// pre-drain snapshot is kept only for new-archive logging above, so a row
	// that landed this tick is still visible to the same tick's
	// positive-evidence transition check.
	postDrain, err := store.ListOrgRolesArchived(ctx)
	if err != nil {
		writeLine(stdout, "org archive mirror: post-drain list failed: %v", err)
		return
	}
	for _, row := range postDrain {
		if _, still := archived[row.Role]; still {
			continue
		}
		if !present[row.Role] {
			// Omission is not evidence: only an agent PRESENT without its
			// archived block proves an unarchive. Preserve the row and its
			// parked directives until positive evidence arrives.
			writeLine(stdout, "org seat %s absent from herdr list; archive state preserved pending positive evidence", row.Role)
			continue
		}
		// The transition is ATOMIC (unpark + mirror delete in one transaction):
		// a failure leaves the archived state fully in force — row present,
		// directives still parked — so the next positive-evidence tick retries
		// the WHOLE transition. Sequential writes here were #1643 round 3's
		// finding: a partial transition plus a later list omission stranded an
		// inconsistent state under a clean stamp forever.
		unparked, err := unarchiveOrgSeatTransition(ctx, store, row.Role, now)
		if err != nil {
			writeLine(stdout, "org seat unarchive observed: %s; transition failed atomically, retried on next positive evidence: %v", row.Role, err)
			failed = true
			continue
		}
		writeLine(stdout, "org seat unarchive observed: %s; %d directives unparked with fresh TTL anchors", row.Role, unparked)
	}
	// Parking is driven by the MIRROR, not the transient list: the mirror row
	// is the durable retry state, so a park that failed on an earlier tick is
	// retried here even when the role is OMITTED from today's list (#1643
	// round 3 block A — a retry that can only fire when the subject happens to
	// reappear is not a retry). Idempotent by WHERE, so re-parking every tick
	// costs nothing.
	postRows, err := store.ListOrgRolesArchived(ctx)
	if err != nil {
		writeLine(stdout, "org archive mirror: re-list for parking failed: %v", err)
		failed = true
	} else {
		mirrorSet := make(map[string]orgArchivedObservation, len(postRows))
		for _, row := range postRows {
			archivedAt, _ := time.Parse(time.RFC3339Nano, row.ArchivedAt)
			observedAt, _ := time.Parse(time.RFC3339Nano, row.ObservedAt)
			mirrorSet[row.Role] = orgArchivedObservation{At: archivedAt, By: row.ArchivedBy, Reason: row.Reason, ObservedAt: observedAt}
		}
		if n, err := parkOutstandingForArchived(ctx, store, mirrorSet, now); err != nil {
			writeLine(stdout, "org archive mirror: directive park failed: %v", err)
			failed = true
		} else if n > 0 {
			writeLine(stdout, "org archive mirror: parked %d open directives for archived seats", n)
		}
	}
	if failed {
		// A tick with any failed write must not stamp success: the stamp's age
		// is the staleness alarm, and advancing it here would disable the
		// alarm on exactly the tick that needs it.
		writeLine(stdout, "org archive mirror: reconciliation incomplete; poll-success stamp withheld")
		return
	}
	if err := store.RecordOrgArchivePollSuccess(ctx, now); err != nil {
		writeLine(stdout, "org archive mirror: poll-success stamp failed: %v", err)
	}
}

package cli

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	dbtest "github.com/gitmoot/gitmoot/internal/db/dbtest"
)

// Fixture shaped from the REAL archived row herdr-app served live on this box
// (archive-canary, captured 2026-08-26; workflow notes 85567/85714): archived
// block present, empty pane ids, agent_status stays idle, parked_work verbatim.
const herdrAgentListArchivedFixture = `{"id":"cli:agent:list","result":{"agents":[
{"name":"keeper","agent":"claude","agent_status":"working","pane_id":"w1:p1"},
{"name":"scout","agent":"claude","agent_status":"idle","pane_id":"",
 "archived":{"at":"2026-08-26T14:29:33.87975607Z","by":"herdr-app","reason":"firing test row"},
 "parked_work":[{"id":"jerryfane/herdrup#173","kind":"issue","repo":"jerryfane/herdrup","title":"Archive agents"},{"id":"85471","kind":"directive","title":"fold the archived contract into the roster filter"}]}
]}}`

const herdrAgentListAllActiveFixture = `{"id":"cli:agent:list","result":{"agents":[
{"name":"keeper","agent":"claude","agent_status":"working","pane_id":"w1:p1"},
{"name":"scout","agent":"claude","agent_status":"idle","pane_id":"w1:p2"}
]}}`

// A well-formed list that simply OMITS scout — the #1643 review-block-1
// adversary: omission carries no evidence in either direction.
const herdrAgentListScoutOmittedFixture = `{"id":"cli:agent:list","result":{"agents":[
{"name":"keeper","agent":"claude","agent_status":"working","pane_id":"w1:p1"}
]}}`

func TestParseHerdrArchivedAgents(t *testing.T) {
	observed := time.Date(2026, 8, 26, 15, 0, 0, 0, time.UTC)
	archived, parked, present, err := parseHerdrArchivedAgents([]byte(herdrAgentListArchivedFixture), observed)
	if err != nil {
		t.Fatal(err)
	}
	if len(archived) != 1 {
		t.Fatalf("archived = %v, want only scout — presence of the block is the signal, agent_status is not", archived)
	}
	if len(present) != 2 || !present["keeper"] || !present["scout"] {
		t.Fatalf("present = %v, want both agents — reconciliation distinguishes present-without-block from absent", present)
	}
	obs, ok := archived["scout"]
	if !ok || obs.By != "herdr-app" || obs.Reason != "firing test row" ||
		!obs.At.Equal(time.Date(2026, 8, 26, 14, 29, 33, 879756070, time.UTC)) || !obs.ObservedAt.Equal(observed) {
		t.Fatalf("scout observation = %+v", obs)
	}
	if !strings.Contains(parked["scout"], "jerryfane/herdrup#173") || !strings.Contains(parked["scout"], "85471") {
		t.Fatalf("parked_work not carried verbatim: %q", parked["scout"])
	}

	if _, _, _, err := parseHerdrArchivedAgents([]byte("not json"), observed); err == nil {
		t.Fatal("malformed read parsed; a malformed read must never look like an empty fleet")
	}
	if _, _, _, err := parseHerdrArchivedAgents([]byte(`{"result":{}}`), observed); err == nil {
		t.Fatal("missing agents array accepted; refusing is what keeps a partial read from unarchiving everyone")
	}
}

func orgArchiveIngestTestStore(t *testing.T) *db.Store {
	t.Helper()
	store, err := dbtest.Open(t, config.PathsForHome(t.TempDir()).Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestRefreshOrgArchiveMirrorObserveParkUnparkAndPreserve(t *testing.T) {
	store := orgArchiveIngestTestStore(t)
	ctx := context.Background()
	directive, err := store.InsertWorkflowNote(ctx, db.WorkflowNote{
		WorkflowID: "wave/ingest", Author: "owner",
		Body: "[org:directive to=scout from=owner wf=wave/ingest] parked with the seat",
	})
	if err != nil {
		t.Fatal(err)
	}
	var sink strings.Builder
	now1 := time.Date(2026, 8, 26, 15, 10, 0, 0, time.UTC)

	// Tick 1: scout archived -> mirror row, directive parked, poll stamped.
	refreshOrgArchiveMirror(ctx, store, &sink, now1, func(context.Context) ([]byte, error) {
		return []byte(herdrAgentListArchivedFixture), nil
	})
	rows, err := store.ListOrgRolesArchived(ctx)
	if err != nil || len(rows) != 1 || rows[0].Role != "scout" || !strings.Contains(rows[0].ParkedWork, "85471") {
		t.Fatalf("mirror after archive tick = %+v err=%v", rows, err)
	}
	if open, err := store.ListOpenOrgDirectiveObligations(ctx, 10); err != nil || len(open) != 0 {
		t.Fatalf("open directives after archive tick = %+v err=%v, want scout's parked", open, err)
	}
	if last, ok, err := store.OrgArchivePollLastSuccess(ctx); err != nil || !ok || !last.Equal(now1) {
		t.Fatalf("poll success = %v ok=%v err=%v, want %v", last, ok, err, now1)
	}

	// Tick 2: herdr read FAILS -> everything preserved (the fail direction).
	now2 := now1.Add(time.Minute)
	refreshOrgArchiveMirror(ctx, store, &sink, now2, func(context.Context) ([]byte, error) {
		return nil, errors.New("herdr unreachable")
	})
	if rows, err := store.ListOrgRolesArchived(ctx); err != nil || len(rows) != 1 {
		t.Fatalf("mirror after failed read = %+v err=%v, want preserved", rows, err)
	}
	if last, _, _ := store.OrgArchivePollLastSuccess(ctx); !last.Equal(now1) {
		t.Fatalf("poll stamp advanced on a FAILED read: %v", last)
	}

	// Tick 3: block gone -> row deleted, directive unparked, anchor = now3.
	now3 := now2.Add(time.Minute)
	refreshOrgArchiveMirror(ctx, store, &sink, now3, func(context.Context) ([]byte, error) {
		return []byte(herdrAgentListAllActiveFixture), nil
	})
	if rows, err := store.ListOrgRolesArchived(ctx); err != nil || len(rows) != 0 {
		t.Fatalf("mirror after unarchive tick = %+v err=%v, want empty", rows, err)
	}
	open, err := store.ListOpenOrgDirectiveObligations(ctx, 10)
	if err != nil || len(open) != 1 || open[0].ID != directive.ID {
		t.Fatalf("open directives after unarchive = %+v err=%v", open, err)
	}
	if got, want := open[0].LastNudgedAt, now3.UTC().Format(time.RFC3339Nano); got != want {
		t.Fatalf("unpark anchor = %q, want %q — a returning seat gets a fresh TTL", got, want)
	}
	if !strings.Contains(sink.String(), "org seat archived observed: scout") ||
		!strings.Contains(sink.String(), "org seat unarchive observed: scout") {
		t.Fatalf("transition log lines missing:\n%s", sink.String())
	}
}

// The #1643 review-block-1 adversary: an agent OMITTED from a WELL-FORMED
// list is not evidence of an unarchive. The mirror row survives, its
// directives stay parked, and the poll stamp still advances (a clean read
// with an omission is a successful poll — the seat's state is preserved, not
// stale). Only positive evidence — the agent PRESENT without its archived
// block — lifts the exclusion.
func TestRefreshOrgArchiveMirrorOmissionPreservesArchiveState(t *testing.T) {
	store := orgArchiveIngestTestStore(t)
	ctx := context.Background()
	directive, err := store.InsertWorkflowNote(ctx, db.WorkflowNote{
		WorkflowID: "wave/ingest", Author: "owner",
		Body: "[org:directive to=scout from=owner wf=wave/ingest] parked with the seat",
	})
	if err != nil {
		t.Fatal(err)
	}
	now1 := time.Date(2026, 8, 27, 1, 0, 0, 0, time.UTC)
	refreshOrgArchiveMirror(ctx, store, io.Discard, now1, func(context.Context) ([]byte, error) {
		return []byte(herdrAgentListArchivedFixture), nil
	})

	// Tick 2: scout is simply ABSENT from a well-formed list.
	now2 := now1.Add(time.Minute)
	var sink strings.Builder
	refreshOrgArchiveMirror(ctx, store, &sink, now2, func(context.Context) ([]byte, error) {
		return []byte(herdrAgentListScoutOmittedFixture), nil
	})
	if rows, err := store.ListOrgRolesArchived(ctx); err != nil || len(rows) != 1 || rows[0].Role != "scout" {
		t.Fatalf("mirror after omission = %+v err=%v, want scout preserved — omission is not evidence", rows, err)
	}
	if open, err := store.ListOpenOrgDirectiveObligations(ctx, 10); err != nil || len(open) != 0 {
		t.Fatalf("open directives after omission = %+v err=%v, want scout's still parked", open, err)
	}
	if last, ok, err := store.OrgArchivePollLastSuccess(ctx); err != nil || !ok || !last.Equal(now2) {
		t.Fatalf("poll stamp = %v ok=%v err=%v, want %v — a clean read with an omission is still a successful poll", last, ok, err, now2)
	}
	if !strings.Contains(sink.String(), "absent from herdr list; archive state preserved") {
		t.Fatalf("omission log line missing:\n%s", sink.String())
	}

	// Tick 3: positive evidence — scout PRESENT without the block — unarchives.
	now3 := now2.Add(time.Minute)
	refreshOrgArchiveMirror(ctx, store, io.Discard, now3, func(context.Context) ([]byte, error) {
		return []byte(herdrAgentListAllActiveFixture), nil
	})
	if rows, err := store.ListOrgRolesArchived(ctx); err != nil || len(rows) != 0 {
		t.Fatalf("mirror after positive evidence = %+v err=%v, want empty", rows, err)
	}
	if open, err := store.ListOpenOrgDirectiveObligations(ctx, 10); err != nil || len(open) != 1 || open[0].ID != directive.ID {
		t.Fatalf("directives after positive evidence = %+v err=%v, want unparked", open, err)
	}
}

// The #1643 round-3 adversary applied to the TRANSITION (block B): a failed
// atomic transition followed by a list OMISSION. Because unpark+delete are one
// transaction, the failure leaves the archived state FULLY in force — row
// present AND directives still parked — so the omission tick preserves a
// consistent state (and cleanly stamps), and the next positive-evidence tick
// completes the whole transition. Mutant M-order/P2 (sequential writes) dies
// here and in the db-level rollback test.
func TestRefreshOrgArchiveMirrorFailedTransitionStaysConsistentUnderOmission(t *testing.T) {
	store := orgArchiveIngestTestStore(t)
	ctx := context.Background()
	directive, err := store.InsertWorkflowNote(ctx, db.WorkflowNote{
		WorkflowID: "wave/ingest", Author: "owner",
		Body: "[org:directive to=scout from=owner wf=wave/ingest] parked with the seat",
	})
	if err != nil {
		t.Fatal(err)
	}
	now1 := time.Date(2026, 8, 27, 2, 0, 0, 0, time.UTC)
	refreshOrgArchiveMirror(ctx, store, io.Discard, now1, func(context.Context) ([]byte, error) {
		return []byte(herdrAgentListArchivedFixture), nil
	})

	// Tick 2: unarchive observed, but the ATOMIC transition fails.
	original := unarchiveOrgSeatTransition
	unarchiveOrgSeatTransition = func(context.Context, *db.Store, string, time.Time) (int64, error) {
		return 0, errors.New("injected transition failure")
	}
	now2 := now1.Add(time.Minute)
	refreshOrgArchiveMirror(ctx, store, io.Discard, now2, func(context.Context) ([]byte, error) {
		return []byte(herdrAgentListAllActiveFixture), nil
	})
	unarchiveOrgSeatTransition = original
	if rows, err := store.ListOrgRolesArchived(ctx); err != nil || len(rows) != 1 || rows[0].Role != "scout" {
		t.Fatalf("mirror after failed transition = %+v err=%v, want row PRESERVED", rows, err)
	}
	if parked, err := store.ListParkedOrgDirectives(ctx, "scout"); err != nil || len(parked) != 1 {
		t.Fatalf("parked after failed transition = %+v err=%v, want STILL PARKED — atomicity means no partial state", parked, err)
	}
	if last, _, _ := store.OrgArchivePollLastSuccess(ctx); !last.Equal(now1) {
		t.Fatalf("poll stamp after failed transition = %v, want unmoved %v", last, now1)
	}

	// Tick 3: scout is OMITTED from a valid list — the round-3 adversary. The
	// state is consistent (archived + parked), so this is a CLEAN tick: row
	// preserved, directives still parked, stamp advances.
	now3 := now2.Add(time.Minute)
	refreshOrgArchiveMirror(ctx, store, io.Discard, now3, func(context.Context) ([]byte, error) {
		return []byte(herdrAgentListScoutOmittedFixture), nil
	})
	if rows, err := store.ListOrgRolesArchived(ctx); err != nil || len(rows) != 1 {
		t.Fatalf("mirror after omission = %+v err=%v, want preserved", rows, err)
	}
	if parked, err := store.ListParkedOrgDirectives(ctx, "scout"); err != nil || len(parked) != 1 {
		t.Fatalf("parked after omission = %+v err=%v, want still parked", parked, err)
	}
	if last, _, _ := store.OrgArchivePollLastSuccess(ctx); !last.Equal(now3) {
		t.Fatalf("poll stamp after consistent omission tick = %v, want %v — nothing was pending, the tick is clean", last, now3)
	}

	// Tick 4: positive evidence returns; the whole transition completes.
	now4 := now3.Add(time.Minute)
	refreshOrgArchiveMirror(ctx, store, io.Discard, now4, func(context.Context) ([]byte, error) {
		return []byte(herdrAgentListAllActiveFixture), nil
	})
	if rows, err := store.ListOrgRolesArchived(ctx); err != nil || len(rows) != 0 {
		t.Fatalf("mirror after retry = %+v err=%v, want transition completed", rows, err)
	}
	open, err := store.ListOpenOrgDirectiveObligations(ctx, 10)
	if err != nil || len(open) != 1 || open[0].ID != directive.ID {
		t.Fatalf("directives after retry = %+v err=%v, want unparked", open, err)
	}
	if got, want := open[0].LastNudgedAt, now4.UTC().Format(time.RFC3339Nano); got != want {
		t.Fatalf("unpark anchor = %q, want the retry tick's stamp %q", got, want)
	}
}

// The #1643 round-3 adversary applied to PARKING (block A): a failed park
// followed by a list OMISSION. Parking is driven by the MIRROR — the durable
// retry state — so the retry fires on the omission tick even though the role
// never reappears in the list. Mutant P1 (park driven by the transient list)
// dies here: under P1 the omission tick receives an empty archived map and the
// directive is never parked while the stamp advances.
func TestRefreshOrgArchiveMirrorFailedParkRetriesUnderOmission(t *testing.T) {
	store := orgArchiveIngestTestStore(t)
	ctx := context.Background()
	directive, err := store.InsertWorkflowNote(ctx, db.WorkflowNote{
		WorkflowID: "wave/ingest", Author: "owner",
		Body: "[org:directive to=scout from=owner wf=wave/ingest] parked with the seat",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Tick 1: scout archived, but the PARK fails.
	original := parkOutstandingForArchived
	parkOutstandingForArchived = func(context.Context, *db.Store, map[string]orgArchivedObservation, time.Time) (int64, error) {
		return 0, errors.New("injected park failure")
	}
	now1 := time.Date(2026, 8, 27, 7, 0, 0, 0, time.UTC)
	refreshOrgArchiveMirror(ctx, store, io.Discard, now1, func(context.Context) ([]byte, error) {
		return []byte(herdrAgentListArchivedFixture), nil
	})
	parkOutstandingForArchived = original
	if rows, err := store.ListOrgRolesArchived(ctx); err != nil || len(rows) != 1 {
		t.Fatalf("mirror after failed park = %+v err=%v, want row present", rows, err)
	}
	if open, err := store.ListOpenOrgDirectiveObligations(ctx, 10); err != nil || len(open) != 1 {
		t.Fatalf("directive after failed park = %+v err=%v, want still OPEN (unparked)", open, err)
	}
	if _, recorded, _ := store.OrgArchivePollLastSuccess(ctx); recorded {
		t.Fatal("poll stamp recorded on a tick with a failed park — the alarm was disabled by its own failure")
	}

	// Tick 2: scout is OMITTED from a valid list. The mirror row drives the
	// park retry regardless.
	now2 := now1.Add(time.Minute)
	refreshOrgArchiveMirror(ctx, store, io.Discard, now2, func(context.Context) ([]byte, error) {
		return []byte(herdrAgentListScoutOmittedFixture), nil
	})
	if open, err := store.ListOpenOrgDirectiveObligations(ctx, 10); err != nil || len(open) != 0 {
		t.Fatalf("directive after omission tick = %+v err=%v, want PARKED — the mirror is the retry state, not the list", open, err)
	}
	if parked, err := store.ListParkedOrgDirectives(ctx, "scout"); err != nil || len(parked) != 1 || parked[0].ID != directive.ID {
		t.Fatalf("parked after omission tick = %+v err=%v", parked, err)
	}
	if last, _, _ := store.OrgArchivePollLastSuccess(ctx); !last.Equal(now2) {
		t.Fatalf("poll stamp after the clean retry tick = %v, want %v", last, now2)
	}
}

// The supplier now reads the mirror: a mirrored archived seat disappears from
// every roster view of a store-backed loadOrgRoster; an empty mirror is
// identity. This is the moment the twelve converted consumers go live.
func TestLoadOrgRosterReadsArchiveMirror(t *testing.T) {
	cfg := orgRosterTestConfig(t)
	store := orgArchiveIngestTestStore(t)
	ctx := context.Background()
	roster := loadOrgRoster(ctx, store, cfg)
	if len(roster.Members()) != 3 {
		t.Fatalf("empty mirror Members() = %v, want identity 3", roster.Members())
	}
	if err := store.UpsertOrgRoleArchived(ctx, db.OrgRoleArchived{
		Role: "scout", ArchivedAt: "2026-08-26T14:29:33Z", ArchivedBy: "herdr-app",
		Reason: "test", ObservedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatal(err)
	}
	roster = loadOrgRoster(ctx, store, cfg)
	if len(roster.Members()) != 2 || len(roster.Nudgeable()) != 2 {
		t.Fatalf("mirrored scout still in roster: members=%v nudgeable=%v", roster.Members(), roster.Nudgeable())
	}
	archived := roster.Archived()
	if len(archived) != 1 || archived[0].Role.Name != "scout" || archived[0].By != "herdr-app" {
		t.Fatalf("Archived() = %+v", archived)
	}
	if s := roster.Standing("scout"); s != orgSeatArchived {
		t.Fatalf("Standing(scout) = %q", s)
	}
}

func TestBuildOrgArchiveMirrorDoctorCheck(t *testing.T) {
	now := time.Date(2026, 8, 26, 16, 0, 0, 0, time.UTC)
	rows := []db.OrgRoleArchived{{Role: "scout"}}
	fresh := buildOrgArchiveMirrorDoctorCheck(rows, nil, now.Add(-2*time.Minute), true, now)
	if !fresh.OK || !strings.Contains(fresh.Detail, "scout") {
		t.Fatalf("fresh = %+v", fresh)
	}
	stale := buildOrgArchiveMirrorDoctorCheck(rows, nil, now.Add(-16*time.Minute), true, now)
	if stale.OK || !strings.Contains(stale.Detail, "STALE") || !strings.Contains(stale.Detail, "exclusions preserved") {
		t.Fatalf("stale = %+v", stale)
	}
	never := buildOrgArchiveMirrorDoctorCheck(rows, nil, time.Time{}, false, now)
	if never.OK || !strings.Contains(never.Detail, "NO recorded successful herdr poll") {
		t.Fatalf("never-succeeded = %+v", never)
	}
}

// The #1643 round-4 adversary: a transient failure creating the FIRST mirror
// row, followed by a valid list OMITTING the role. The pending ledger — the
// tick's first durable write — survives the failure, so the omission tick
// retries the upsert FROM PENDING, parks the directives, and only then stamps.
// Mutant R4-M (drain only today's observations, ignore pending leftovers)
// dies here while every earlier-round test still passes.
func TestRefreshOrgArchiveMirrorFailedFirstUpsertRetriesFromPendingUnderOmission(t *testing.T) {
	store := orgArchiveIngestTestStore(t)
	ctx := context.Background()
	directive, err := store.InsertWorkflowNote(ctx, db.WorkflowNote{
		WorkflowID: "wave/ingest", Author: "owner",
		Body: "[org:directive to=scout from=owner wf=wave/ingest] parked with the seat",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Tick 1: scout archived in the list, but the FIRST mirror upsert fails.
	original := upsertOrgRoleArchived
	upsertOrgRoleArchived = func(context.Context, *db.Store, db.OrgRoleArchived) error {
		return errors.New("injected upsert failure")
	}
	now1 := time.Date(2026, 8, 27, 9, 0, 0, 0, time.UTC)
	refreshOrgArchiveMirror(ctx, store, io.Discard, now1, func(context.Context) ([]byte, error) {
		return []byte(herdrAgentListArchivedFixture), nil
	})
	upsertOrgRoleArchived = original
	if rows, err := store.ListOrgRolesArchived(ctx); err != nil || len(rows) != 0 {
		t.Fatalf("mirror after failed first upsert = %+v err=%v, want empty", rows, err)
	}
	if pending, err := store.ListOrgArchivePending(ctx); err != nil || len(pending) != 1 || pending[0].Role != "scout" {
		t.Fatalf("pending after failed first upsert = %+v err=%v, want the durable observation", pending, err)
	}
	if _, recorded, _ := store.OrgArchivePollLastSuccess(ctx); recorded {
		t.Fatal("poll stamp recorded on the failing tick")
	}

	// Tick 2: a valid list OMITS scout entirely. The pending ledger drives the
	// retry anyway: mirror row lands, directive parks, stamp advances.
	now2 := now1.Add(time.Minute)
	refreshOrgArchiveMirror(ctx, store, io.Discard, now2, func(context.Context) ([]byte, error) {
		return []byte(herdrAgentListScoutOmittedFixture), nil
	})
	if rows, err := store.ListOrgRolesArchived(ctx); err != nil || len(rows) != 1 || rows[0].Role != "scout" {
		t.Fatalf("mirror after omission tick = %+v err=%v, want scout retried FROM PENDING", rows, err)
	}
	if pending, err := store.ListOrgArchivePending(ctx); err != nil || len(pending) != 0 {
		t.Fatalf("pending after successful drain = %+v err=%v, want empty", pending, err)
	}
	if parked, err := store.ListParkedOrgDirectives(ctx, "scout"); err != nil || len(parked) != 1 || parked[0].ID != directive.ID {
		t.Fatalf("parked after retry = %+v err=%v", parked, err)
	}
	if last, ok, _ := store.OrgArchivePollLastSuccess(ctx); !ok || !last.Equal(now2) {
		t.Fatalf("poll stamp = %v ok=%v, want %v — stamped only after the pending work landed", last, ok, now2)
	}
}

// The #1643 round-6 adversary (both families independently; kimi's exact
// three-tick probe): stale archive evidence must not override fresher
// positive evidence. Tick 1 archives with a forced upsert failure (pending
// survives); tick 2's ALL-ACTIVE list contradicts the pending observation —
// it is DISCARDED, never applied; tick 3's omission then has nothing to
// resurrect. Evidence recency wins. Mutant R6-M (apply pending
// unconditionally) dies only here.
func TestRefreshOrgArchiveMirrorFresherPositiveEvidenceSupersedesPending(t *testing.T) {
	store := orgArchiveIngestTestStore(t)
	ctx := context.Background()
	directive, err := store.InsertWorkflowNote(ctx, db.WorkflowNote{
		WorkflowID: "wave/ingest", Author: "owner",
		Body: "[org:directive to=scout from=owner wf=wave/ingest] must never wrongly park",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Tick 1: archived observed, mirror upsert fails — pending survives.
	original := upsertOrgRoleArchived
	upsertOrgRoleArchived = func(context.Context, *db.Store, db.OrgRoleArchived) error {
		return errors.New("injected upsert failure")
	}
	now1 := time.Date(2026, 8, 27, 11, 0, 0, 0, time.UTC)
	refreshOrgArchiveMirror(ctx, store, io.Discard, now1, func(context.Context) ([]byte, error) {
		return []byte(herdrAgentListArchivedFixture), nil
	})
	upsertOrgRoleArchived = original

	// Tick 2: ALL-ACTIVE — scout present WITHOUT its block. The stale pending
	// observation is superseded and discarded.
	now2 := now1.Add(time.Minute)
	var sink strings.Builder
	refreshOrgArchiveMirror(ctx, store, &sink, now2, func(context.Context) ([]byte, error) {
		return []byte(herdrAgentListAllActiveFixture), nil
	})
	if pending, err := store.ListOrgArchivePending(ctx); err != nil || len(pending) != 0 {
		t.Fatalf("pending after contradiction = %+v err=%v, want discarded", pending, err)
	}
	if rows, err := store.ListOrgRolesArchived(ctx); err != nil || len(rows) != 0 {
		t.Fatalf("mirror after contradiction = %+v err=%v, want empty — stale evidence must not resurrect", rows, err)
	}
	// The supersede runs through apply + the atomic transition (round 7): the
	// stale observation lands and is unarchived in the same tick, its pending
	// row dying inside the transition transaction.
	if !strings.Contains(sink.String(), "org seat unarchive observed: scout") {
		t.Fatalf("supersede-by-transition log line missing:\n%s", sink.String())
	}

	// Tick 3: omission — nothing to resurrect, directive stays open, stamp clean.
	now3 := now2.Add(time.Minute)
	refreshOrgArchiveMirror(ctx, store, io.Discard, now3, func(context.Context) ([]byte, error) {
		return []byte(herdrAgentListScoutOmittedFixture), nil
	})
	if rows, err := store.ListOrgRolesArchived(ctx); err != nil || len(rows) != 0 {
		t.Fatalf("mirror after omission = %+v err=%v, want still empty", rows, err)
	}
	if open, err := store.ListOpenOrgDirectiveObligations(ctx, 10); err != nil || len(open) != 1 || open[0].ID != directive.ID {
		t.Fatalf("directive = %+v err=%v, want OPEN and never wrongly parked", open, err)
	}
	if last, ok, _ := store.OrgArchivePollLastSuccess(ctx); !ok || !last.Equal(now3) {
		t.Fatalf("poll stamp = %v ok=%v, want %v", last, ok, now3)
	}
}

// The downgrade-visibility line (#1643 ruling, directive 86986): AGED pending
// rows warn with age+count and the named rollback condition; YOUNG pending
// rows do not warn (a normal in-flight tick or fresh rollback is not a
// problem, and a wolf-crying check gets whitelisted). Mutant D-M (drop the
// pending clause) dies to the aged case.
func TestBuildOrgArchiveMirrorDoctorCheckPendingObservations(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	aged := []db.OrgRoleArchived{{Role: "scout", ObservedAt: now.Add(-20 * time.Minute).Format(time.RFC3339Nano)}}
	check := buildOrgArchiveMirrorDoctorCheck(nil, aged, now.Add(-time.Minute), true, now)
	if check.OK || !strings.Contains(check.Detail, "undrained pending archive observation") ||
		!strings.Contains(check.Detail, "binary rollback window") || !strings.Contains(check.Detail, "20m0s") {
		t.Fatalf("aged pending = %+v, want an age-and-count warning naming the rollback condition", check)
	}
	young := []db.OrgRoleArchived{{Role: "scout", ObservedAt: now.Add(-time.Minute).Format(time.RFC3339Nano)}}
	check = buildOrgArchiveMirrorDoctorCheck([]db.OrgRoleArchived{{Role: "keeper"}}, young, now.Add(-time.Minute), true, now)
	if !check.OK {
		t.Fatalf("young pending = %+v, want no warning — a fresh ledger is normal, not a verdict", check)
	}
}

// The #1643 round-7 adversary, part 1: the supersede must be atomic with its
// consumer. The drain's routine pending cleanup FAILS, so a pending row
// outlives its application — and the same tick's atomic transition still
// kills it inside the transaction, so a later omission has nothing to
// resurrect. Mutant R7-M1 (pending-delete dropped from the transition tx)
// dies only here: the leftover pending row would reapply the stale archive on
// the omission tick and park the seat under a clean stamp.
func TestRefreshOrgArchiveMirrorSupersededPendingDiesInsideTheTransition(t *testing.T) {
	store := orgArchiveIngestTestStore(t)
	ctx := context.Background()
	directive, err := store.InsertWorkflowNote(ctx, db.WorkflowNote{
		WorkflowID: "wave/ingest", Author: "owner",
		Body: "[org:directive to=scout from=owner wf=wave/ingest] must never resurrect",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Tick 1: archived observed, mirror upsert fails — pending only.
	originalUpsert := upsertOrgRoleArchived
	upsertOrgRoleArchived = func(context.Context, *db.Store, db.OrgRoleArchived) error {
		return errors.New("injected upsert failure")
	}
	now1 := time.Date(2026, 8, 27, 13, 0, 0, 0, time.UTC)
	refreshOrgArchiveMirror(ctx, store, io.Discard, now1, func(context.Context) ([]byte, error) {
		return []byte(herdrAgentListArchivedFixture), nil
	})
	upsertOrgRoleArchived = originalUpsert

	// Tick 2: ALL-ACTIVE; the drain's routine pending cleanup FAILS, so only
	// the transition transaction can kill the pending row — and does.
	originalDelete := deleteOrgArchivePending
	deleteOrgArchivePending = func(context.Context, *db.Store, string) error {
		return errors.New("injected pending-cleanup failure")
	}
	now2 := now1.Add(time.Minute)
	refreshOrgArchiveMirror(ctx, store, io.Discard, now2, func(context.Context) ([]byte, error) {
		return []byte(herdrAgentListAllActiveFixture), nil
	})
	deleteOrgArchivePending = originalDelete
	if pending, err := store.ListOrgArchivePending(ctx); err != nil || len(pending) != 0 {
		t.Fatalf("pending after supersede = %+v err=%v, want dead INSIDE the transition transaction", pending, err)
	}
	if rows, err := store.ListOrgRolesArchived(ctx); err != nil || len(rows) != 0 {
		t.Fatalf("mirror after supersede = %+v err=%v, want empty", rows, err)
	}
	if _, recorded, _ := store.OrgArchivePollLastSuccess(ctx); recorded {
		t.Fatal("stamp recorded on a tick with a failed cleanup write")
	}

	// Tick 3: omission — nothing left to resurrect.
	now3 := now2.Add(time.Minute)
	refreshOrgArchiveMirror(ctx, store, io.Discard, now3, func(context.Context) ([]byte, error) {
		return []byte(herdrAgentListScoutOmittedFixture), nil
	})
	if rows, err := store.ListOrgRolesArchived(ctx); err != nil || len(rows) != 0 {
		t.Fatalf("mirror after omission = %+v err=%v, want still empty — the superseded observation must not return", rows, err)
	}
	if open, err := store.ListOpenOrgDirectiveObligations(ctx, 10); err != nil || len(open) != 1 || open[0].ID != directive.ID {
		t.Fatalf("directive = %+v err=%v, want OPEN, never wrongly parked", open, err)
	}
	if last, ok, _ := store.OrgArchivePollLastSuccess(ctx); !ok || !last.Equal(now3) {
		t.Fatalf("stamp = %v ok=%v, want clean %v", last, ok, now3)
	}
}

// The #1643 round-7 adversary, part 2: UNREADABLE IS NOT EMPTY. A doctor that
// reports healthy when it cannot read the pending ledger converts an unknown
// into a false negative — the absence-as-evidence class reborn inside the
// guard built to expose a different absence. Mutant D2-M (swallow the read
// error and treat the ledger as empty) dies only here.
func TestOrgArchiveMirrorDoctorFailsLoudOnUnreadableLedger(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	original := listOrgArchivePendingForDoctor
	listOrgArchivePendingForDoctor = func(context.Context, *db.Store) ([]db.OrgRoleArchived, error) {
		return nil, errors.New("injected ledger read failure")
	}
	t.Cleanup(func() { listOrgArchivePendingForDoctor = original })
	check, present := orgArchiveMirrorDoctorCheck(paths)
	if !present || check.OK || !strings.Contains(check.Detail, "UNREADABLE") || !strings.Contains(check.Detail, "UNKNOWN, not healthy") {
		t.Fatalf("unreadable-ledger doctor = present=%v %+v, want a loud UNKNOWN", present, check)
	}
}

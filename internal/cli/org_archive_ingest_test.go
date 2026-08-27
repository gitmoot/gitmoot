package cli

import (
	"context"
	"errors"
	"io"
	"path/filepath"
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
	// A readable, fresh observed_at: this test's axis is the POLL stamp, and
	// since round 9 an empty observed_at correctly trips the unusable-evidence
	// warning instead (its own test below).
	rows := []db.OrgRoleArchived{{Role: "scout", ArchivedAt: now.Add(-time.Hour).Format(time.RFC3339Nano), ObservedAt: now.Add(-time.Minute).Format(time.RFC3339Nano)}}
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
	// The supersede runs through the atomic transition DIRECTLY (round 8):
	// the stale observation never touches the shipping mirror, and its
	// pending row dies inside the transition transaction.
	if !strings.Contains(sink.String(), "superseded by fresher positive evidence (atomic)") {
		t.Fatalf("atomic-supersede log line missing:\n%s", sink.String())
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
	aged := []db.OrgRoleArchived{{Role: "scout", ArchivedAt: now.Add(-time.Hour).Format(time.RFC3339Nano), ObservedAt: now.Add(-20 * time.Minute).Format(time.RFC3339Nano)}}
	check := buildOrgArchiveMirrorDoctorCheck(nil, aged, now.Add(-time.Minute), true, now)
	if check.OK || !strings.Contains(check.Detail, "undrained pending archive observation") ||
		!strings.Contains(check.Detail, "binary rollback window") || !strings.Contains(check.Detail, "20m0s") {
		t.Fatalf("aged pending = %+v, want an age-and-count warning naming the rollback condition", check)
	}
	young := []db.OrgRoleArchived{{Role: "scout", ArchivedAt: now.Add(-time.Hour).Format(time.RFC3339Nano), ObservedAt: now.Add(-time.Minute).Format(time.RFC3339Nano)}}
	check = buildOrgArchiveMirrorDoctorCheck([]db.OrgRoleArchived{{Role: "keeper", ArchivedAt: now.Add(-time.Hour).Format(time.RFC3339Nano), ObservedAt: now.Add(-time.Minute).Format(time.RFC3339Nano)}}, young, now.Add(-time.Minute), true, now)
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
	// The contradicted row bypassed the drain entirely (round 8), so the
	// injected drain-cleanup failure never fired: tick 2 is CLEAN and stamps.
	if last, ok, _ := store.OrgArchivePollLastSuccess(ctx); !ok || !last.Equal(now2) {
		t.Fatalf("stamp = %v ok=%v, want clean %v — the atomic supersede is one clean write", last, ok, now2)
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
	check, present := orgArchiveMirrorDoctorCheck(paths, nil)
	if !present || check.OK || !strings.Contains(check.Detail, "UNREADABLE") || !strings.Contains(check.Detail, "UNKNOWN, not healthy") {
		t.Fatalf("unreadable-ledger doctor = present=%v %+v, want a loud UNKNOWN", present, check)
	}
}

// The #1643 round-8 codex finding: a contradicted stale observation must
// NEVER touch the shipping mirror — the round-7 apply-then-transition path
// gave concurrent readers a window classifying an actively observed seat as
// archived. The recording seam proves no upsert is attempted for the
// contradicted role. Mutant C8-M (revert to apply-then-transition) dies here.
func TestRefreshOrgArchiveMirrorContradictedPendingNeverTouchesTheMirror(t *testing.T) {
	store := orgArchiveIngestTestStore(t)
	ctx := context.Background()
	// Tick 1: archived, upsert fails — pending only.
	originalUpsert := upsertOrgRoleArchived
	upsertOrgRoleArchived = func(context.Context, *db.Store, db.OrgRoleArchived) error {
		return errors.New("injected upsert failure")
	}
	now1 := time.Date(2026, 8, 27, 14, 0, 0, 0, time.UTC)
	refreshOrgArchiveMirror(ctx, store, io.Discard, now1, func(context.Context) ([]byte, error) {
		return []byte(herdrAgentListArchivedFixture), nil
	})
	// Tick 2: ALL-ACTIVE. Record every upsert attempt: the contradicted role
	// must not appear — the supersede goes straight through the transition.
	var upserted []string
	upsertOrgRoleArchived = func(ctx context.Context, store *db.Store, row db.OrgRoleArchived) error {
		upserted = append(upserted, row.Role)
		return store.UpsertOrgRoleArchived(ctx, row)
	}
	refreshOrgArchiveMirror(ctx, store, io.Discard, now1.Add(time.Minute), func(context.Context) ([]byte, error) {
		return []byte(herdrAgentListAllActiveFixture), nil
	})
	upsertOrgRoleArchived = originalUpsert
	for _, role := range upserted {
		if role == "scout" {
			t.Fatalf("contradicted pending row was written to the shipping mirror (upserts: %v) — readers could classify an active seat as archived", upserted)
		}
	}
	if pending, err := store.ListOrgArchivePending(ctx); err != nil || len(pending) != 0 {
		t.Fatalf("pending after atomic supersede = %+v err=%v, want gone", pending, err)
	}
}

// The #1643 round-8 opus finding (adversary 8): a seat carried on OMISSION
// alone is UNPROVEN, and its observed_at — refreshed only by positive
// archived evidence — is that state's own clock. The doctor warns when it
// ages past the threshold, with age, count, and the condition named; a
// freshly confirmed row never warns. Mutant A8-M (ignore row age) dies here.
func TestBuildOrgArchiveMirrorDoctorCheckUnconfirmedExclusions(t *testing.T) {
	now := time.Date(2026, 8, 27, 15, 0, 0, 0, time.UTC)
	aged := []db.OrgRoleArchived{{Role: "scout", ArchivedAt: now.Add(-time.Hour).Format(time.RFC3339Nano), ObservedAt: now.Add(-30 * time.Minute).Format(time.RFC3339Nano)}}
	check := buildOrgArchiveMirrorDoctorCheck(aged, nil, now.Add(-time.Minute), true, now)
	if check.OK || !strings.Contains(check.Detail, "UNCONFIRMED") ||
		!strings.Contains(check.Detail, "30m0s") || !strings.Contains(check.Detail, "scout") ||
		!strings.Contains(check.Detail, "aging without bound") {
		t.Fatalf("aged unconfirmed exclusion = %+v, want age+count+condition — the stamp answers did-this-tick-complete, not is-the-mirror-true", check)
	}
	fresh := []db.OrgRoleArchived{{Role: "scout", ArchivedAt: now.Add(-time.Hour).Format(time.RFC3339Nano), ObservedAt: now.Add(-time.Minute).Format(time.RFC3339Nano)}}
	if check := buildOrgArchiveMirrorDoctorCheck(fresh, nil, now.Add(-time.Minute), true, now); !check.OK {
		t.Fatalf("freshly confirmed exclusion = %+v, want healthy", check)
	}
}

// The #1643 round-8 codex doctor finding: an unreadable MIRROR must fail loud
// exactly as the unreadable pending ledger does — the round-7 fix named one
// field and not the adjacent one. Mutant C8b-M (absent on mirror read error)
// dies here.
func TestOrgArchiveMirrorDoctorFailsLoudOnUnreadableMirror(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	original := listOrgRolesArchivedForDoctor
	listOrgRolesArchivedForDoctor = func(context.Context, *db.Store) ([]db.OrgRoleArchived, error) {
		return nil, errors.New("injected mirror read failure")
	}
	t.Cleanup(func() { listOrgRolesArchivedForDoctor = original })
	check, present := orgArchiveMirrorDoctorCheck(paths, nil)
	if !present || check.OK || !strings.Contains(check.Detail, "mirror UNREADABLE") || !strings.Contains(check.Detail, "UNKNOWN, not healthy") {
		t.Fatalf("unreadable-mirror doctor = present=%v %+v, want a loud UNKNOWN", present, check)
	}
}

// The #1643 round-9 codex finding: a nameless agents-list entry (missing
// name, whitespace name, or a null element) was silently skipped, so an
// incomplete read the ingest could not key still reached the success stamp —
// including a possible archived block with no owner. Refusing the whole list
// is the malformed-read rule one element down. Mutant N9-M (restore the
// `continue` on an empty name) dies here; the envelope-level refusal tests in
// TestParseHerdrArchivedAgents still pass under it, proving the adversaries
// are distinct.
func TestParseHerdrArchivedAgentsRefusesNamelessEntries(t *testing.T) {
	observed := time.Date(2026, 8, 27, 5, 0, 0, 0, time.UTC)
	for _, tc := range []struct{ label, raw string }{
		{"missing name field", `{"result":{"agents":[{"name":"keeper"},{"agent_status":"idle"}]}}`},
		{"whitespace name", `{"result":{"agents":[{"name":"   "}]}}`},
		{"null element", `{"result":{"agents":[{"name":"keeper"},null]}}`},
		{"nameless with archived block", `{"result":{"agents":[{"archived":{"at":"2026-08-26T14:29:33Z","by":"x","reason":"y"}}]}}`},
	} {
		if _, _, _, err := parseHerdrArchivedAgents([]byte(tc.raw), observed); err == nil {
			t.Fatalf("%s: parsed without error; an unkeyable entry must refuse the read so the tick never stamps success over it", tc.label)
		}
	}
	// Control: named entries still parse.
	if _, _, present, err := parseHerdrArchivedAgents([]byte(`{"result":{"agents":[{"name":"keeper"}]}}`), observed); err != nil || !present["keeper"] {
		t.Fatalf("named entry refused: present=%v err=%v", present, err)
	}
}

// Adversary 9 (#1643 round 9, both families, opus's probe table): observed_at
// is load-bearing for the round-8 exclusion-age alarm, and the alarm treated
// an UNREADABLE stamp as a fresh one — empty, garbage, wrong layout, unix
// seconds, and future timestamps all silenced the exact check built to bound
// exclusion age. Unusable evidence is UNKNOWN, never young. Mutant A9-M1
// (restore `if err != nil || age <= threshold { continue }`) dies here while
// TestBuildOrgArchiveMirrorDoctorCheckUnconfirmedExclusions (readable-ancient)
// still passes, proving unreadable and aged are distinct adversaries.
func TestBuildOrgArchiveMirrorDoctorCheckUnusableEvidenceTimestamps(t *testing.T) {
	now := time.Date(2026, 8, 27, 15, 0, 0, 0, time.UTC)
	for _, tc := range []struct{ label, observedAt string }{
		{"empty", ""},
		{"garbage", "not-a-timestamp"},
		{"wrong layout", "2026-07-27"},
		{"unix seconds", "1756300000"},
		{"future", now.Add(48 * time.Hour).Format(time.RFC3339Nano)},
	} {
		rows := []db.OrgRoleArchived{{Role: "scout", ArchivedAt: now.Add(-time.Hour).Format(time.RFC3339Nano), ObservedAt: tc.observedAt}}
		check := buildOrgArchiveMirrorDoctorCheck(rows, nil, now.Add(-time.Minute), true, now)
		if check.OK || !strings.Contains(check.Detail, "UNKNOWN") || !strings.Contains(check.Detail, "scout") {
			t.Fatalf("%s observed_at = %+v, want an UNKNOWN warning naming the seat — unreadable evidence must not read as fresh", tc.label, check)
		}
	}
}

// The pending sibling of adversary 9: pendingObservationsDetail's err==nil
// fold meant a ledger of entirely-unparseable rows reported age zero and
// never warned. Mutant A9-M2 (restore the err==nil fold) dies here while the
// mirror-side unusable test above still passes — same principle, distinct
// code path, separately mutable.
func TestBuildOrgArchiveMirrorDoctorCheckUnusablePendingTimestamps(t *testing.T) {
	now := time.Date(2026, 8, 27, 15, 0, 0, 0, time.UTC)
	for _, observedAt := range []string{"", "xxx", now.Add(time.Hour).Format(time.RFC3339Nano)} {
		pending := []db.OrgRoleArchived{{Role: "scout", ArchivedAt: now.Add(-time.Hour).Format(time.RFC3339Nano), ObservedAt: observedAt}}
		check := buildOrgArchiveMirrorDoctorCheck(nil, pending, now.Add(-time.Minute), true, now)
		if check.OK || !strings.Contains(check.Detail, "UNKNOWN") {
			t.Fatalf("unusable pending observed_at %q = %+v, want an UNKNOWN warning — an unparseable ledger is not a young one", observedAt, check)
		}
	}
	// Control: a young readable pending ledger with no mirrored rows stays quiet.
	young := []db.OrgRoleArchived{{Role: "scout", ArchivedAt: now.Add(-time.Hour).Format(time.RFC3339Nano), ObservedAt: now.Add(-time.Minute).Format(time.RFC3339Nano)}}
	if check := buildOrgArchiveMirrorDoctorCheck(nil, young, now.Add(-time.Minute), true, now); !check.OK {
		t.Fatalf("young readable pending = %+v, want healthy", check)
	}
}

// The third load-bearing timestamp, found by the round-9 checklist walk
// rather than a reviewer: a poll-success stamp in the FUTURE yields negative
// age and passed the staleness gate as maximally fresh. Noncausal is UNKNOWN.
// Mutant F9-M (drop the last.After(now) branch) dies here; the stale and
// fresh stamp cases in TestBuildOrgArchiveMirrorDoctorCheck still pass.
func TestBuildOrgArchiveMirrorDoctorCheckFuturePollStamp(t *testing.T) {
	now := time.Date(2026, 8, 27, 15, 0, 0, 0, time.UTC)
	rows := []db.OrgRoleArchived{{Role: "scout", ArchivedAt: now.Add(-time.Hour).Format(time.RFC3339Nano), ObservedAt: now.Add(-time.Minute).Format(time.RFC3339Nano)}}
	check := buildOrgArchiveMirrorDoctorCheck(rows, nil, now.Add(time.Hour), true, now)
	if check.OK || !strings.Contains(check.Detail, "FUTURE") || !strings.Contains(check.Detail, "UNKNOWN") {
		t.Fatalf("future poll stamp = %+v, want an UNKNOWN warning — negative age must not read as fresh", check)
	}
}

// F1 (#1643 round 10, codex): an unopenable database made the archive doctor
// check VANISH — absence-by-failure wearing the absent-by-design shape, so
// every guard in the file was conditional on a read that failed open. An
// unreachable database must yield a LOUD check, because an absent check and a
// healthy check read identically. Mutant F1-M (restore `return Check{},
// false` on the open error) dies here; the unreadable-mirror and
// unreadable-ledger tests still pass under it, proving the open, the mirror
// read, and the ledger read are three distinct failure points.
func TestOrgArchiveMirrorDoctorFailsLoudOnUnopenableDatabase(t *testing.T) {
	paths := config.PathsForHome(t.TempDir())
	paths.Database = filepath.Join(t.TempDir(), "missing", "gitmoot.db")
	check, present := orgArchiveMirrorDoctorCheck(paths, nil)
	if !present {
		t.Fatal("check absent on an unopenable database; absence-by-failure is indistinguishable from healthy")
	}
	if check.OK || !strings.Contains(check.Detail, "UNKNOWN") || !strings.Contains(check.Detail, "UNOPENABLE") {
		t.Fatalf("unopenable database = %+v, want a loud UNKNOWN", check)
	}
}

// F2 (#1643 round 10, codex): `Scout` carrying an archived block beside
// ` scout ` without one normalised into one key — contradictory lifecycle
// evidence silently reconciled, and the tick stamped success. The name is the
// join key; a list repeating it is internally malformed whether or not the
// copies agree. Mutant F2-M (drop the duplicate refusal) dies here; the
// nameless-entry test still passes under it — unkeyable and doubly-keyed are
// distinct adversaries.
func TestParseHerdrArchivedAgentsRefusesDuplicateNames(t *testing.T) {
	observed := time.Date(2026, 8, 27, 6, 0, 0, 0, time.UTC)
	contradictory := `{"result":{"agents":[{"name":"Scout","archived":{"at":"2026-08-26T14:29:33Z","by":"x","reason":"y"}},{"name":" scout "}]}}`
	if _, _, _, err := parseHerdrArchivedAgents([]byte(contradictory), observed); err == nil {
		t.Fatal("contradictory duplicate parsed; a contradiction must never be reported as successfully reconciled")
	}
	agreeing := `{"result":{"agents":[{"name":"keeper"},{"name":"KEEPER"}]}}`
	if _, _, _, err := parseHerdrArchivedAgents([]byte(agreeing), observed); err == nil {
		t.Fatal("agreeing duplicate parsed; the join key must be unique regardless of agreement")
	}
	distinct := `{"result":{"agents":[{"name":"keeper"},{"name":"scout"}]}}`
	if _, _, present, err := parseHerdrArchivedAgents([]byte(distinct), observed); err != nil || len(present) != 2 {
		t.Fatalf("distinct names refused: present=%v err=%v", present, err)
	}
}

// F3 (#1643 round 10, opus): the FOURTH timestamp. An archived block with no
// `at` at all — `{}`, or by/reason without at — excluded a seat on zero
// evidence, and a future `at` was stored silently; the malformed-string case
// failed closed only as an encoding/json side effect, which absence does not
// trigger. The guard is now explicit. Mutant F3-M (drop the at validation)
// dies here; every other parse refusal test passes under it.
func TestParseHerdrArchivedAgentsRefusesEvidencelessArchivedBlocks(t *testing.T) {
	observed := time.Date(2026, 8, 27, 6, 0, 0, 0, time.UTC)
	for _, tc := range []struct{ label, raw string }{
		{"empty block", `{"result":{"agents":[{"name":"scout","archived":{}}]}}`},
		{"missing at", `{"result":{"agents":[{"name":"scout","archived":{"by":"x","reason":"y"}}]}}`},
		{"future at", `{"result":{"agents":[{"name":"scout","archived":{"at":"2999-01-01T00:00:00Z","by":"x","reason":"y"}}]}}`},
	} {
		if _, _, _, err := parseHerdrArchivedAgents([]byte(tc.raw), observed); err == nil {
			t.Fatalf("%s: parsed without error; an exclusion needs evidence, exactly as lifting one does", tc.label)
		}
	}
	valid := `{"result":{"agents":[{"name":"scout","archived":{"at":"2026-08-26T14:29:33Z","by":"x","reason":"y"}}]}}`
	if archived, _, _, err := parseHerdrArchivedAgents([]byte(valid), observed); err != nil || len(archived) != 1 {
		t.Fatalf("valid archived block refused: %v err=%v", archived, err)
	}
}

// F1/F4 (#1643 round 11, codex): a discarded pathsFromFlag error let a zero
// config.Paths reach the empty-database branch, and the whole check VANISHED —
// a FAILURE manufacturing what reads as absence-by-configuration. The
// declared-absence branch itself is sound (opus) and stays; this pins the
// distinction mechanically: resolution error -> loud UNKNOWN; empty path with
// NO error -> absent, because someone declared it. Mutant R11-M1 (pass nil at
// the call site / drop the pathsErr branch) dies here while the
// unopenable-database test still passes — resolution and open are distinct
// failure points.
func TestOrgArchiveMirrorDoctorFailsLoudOnPathResolutionFailure(t *testing.T) {
	check, present := orgArchiveMirrorDoctorCheck(config.Paths{}, errors.New("HOME unset"))
	if !present {
		t.Fatal("check absent on a path-resolution FAILURE; a failure must not wear the declared-absence shape")
	}
	if check.OK || !strings.Contains(check.Detail, "home resolution FAILED") || !strings.Contains(check.Detail, "UNKNOWN") {
		t.Fatalf("path-resolution failure = %+v, want a loud UNKNOWN", check)
	}
	if _, present := orgArchiveMirrorDoctorCheck(config.Paths{}, nil); present {
		t.Fatal("empty database path with NO error must stay absent — absence-by-configuration is a statement someone made")
	}
}

// F2/F4 (#1643 round 11, codex): the round-10 ingress guard covered the door,
// not the room — a PRE-FIX pending row with a zero archived_at was drained
// into the mirror and the stamp advanced. The drain now rejects unusable
// durable rows: mirror unwritten, pending row preserved, stamp withheld; the
// next valid observation heals it through the ingress guard. Mutant R11-M2
// (drop the drain-side validation) dies here while the round-10 ingress
// refusal test still passes — door and room are distinct guards.
func TestRefreshOrgArchiveMirrorRefusesPreFixPendingRows(t *testing.T) {
	store := orgArchiveIngestTestStore(t)
	ctx := context.Background()
	now1 := time.Date(2026, 8, 27, 9, 0, 0, 0, time.UTC)
	// A pre-round-10 row already durable in the pending ledger: zero
	// archived_at, valid observation stamp.
	if err := store.MergeOrgArchivePending(ctx, []db.OrgRoleArchived{{
		Role: "scout", ArchivedAt: time.Time{}.Format(time.RFC3339Nano),
		ArchivedBy: "herdr-app", ObservedAt: now1.Add(-time.Hour).Format(time.RFC3339Nano),
	}}); err != nil {
		t.Fatal(err)
	}
	var sink strings.Builder
	refreshOrgArchiveMirror(ctx, store, &sink, now1, func(context.Context) ([]byte, error) {
		return []byte(herdrAgentListScoutOmittedFixture), nil
	})
	if rows, err := store.ListOrgRolesArchived(ctx); err != nil || len(rows) != 0 {
		t.Fatalf("mirror after bad-row tick = %+v err=%v, want empty — an evidence-free row must not ship", rows, err)
	}
	if pending, err := store.ListOrgArchivePending(ctx); err != nil || len(pending) != 1 {
		t.Fatalf("pending after bad-row tick = %+v err=%v, want preserved — deletion would lose the observation", pending, err)
	}
	if _, ok, err := store.OrgArchivePollLastSuccess(ctx); err != nil || ok {
		t.Fatalf("stamp advanced over a rejected durable row (ok=%v err=%v); the tick proved nothing about scout", ok, err)
	}
	if !strings.Contains(sink.String(), "unusable archived_at") {
		t.Fatalf("rejection not logged:\n%s", sink.String())
	}
	// A valid observation heals it: fresh pending row overwrites, mirror
	// lands, stamp advances.
	now2 := now1.Add(time.Minute)
	refreshOrgArchiveMirror(ctx, store, io.Discard, now2, func(context.Context) ([]byte, error) {
		return []byte(herdrAgentListArchivedFixture), nil
	})
	rows, err := store.ListOrgRolesArchived(ctx)
	if err != nil || len(rows) != 1 || rows[0].Role != "scout" {
		t.Fatalf("mirror after healing tick = %+v err=%v", rows, err)
	}
	if unusableArchiveTimestamp(rows[0].ArchivedAt, now2) {
		t.Fatalf("healed mirror row still carries unusable archived_at %q", rows[0].ArchivedAt)
	}
	if last, ok, err := store.OrgArchivePollLastSuccess(ctx); err != nil || !ok || !last.Equal(now2) {
		t.Fatalf("stamp after healing tick = %v ok=%v err=%v, want %v", last, ok, err, now2)
	}
}

// F3 (#1643 round 11, opus): the doctor validates observed_at and did not
// read archived_at, so a zero archived_at under fresh observation and poll
// stamps produced OK=true. archived_at now joins the unusable partition in
// BOTH helpers. Mutant R11-M3 (drop archived_at from the partition) dies here
// while the round-9 observed_at unusable tests still pass — same rule, new
// field, separately mutable.
func TestBuildOrgArchiveMirrorDoctorCheckUnusableArchivedAt(t *testing.T) {
	now := time.Date(2026, 8, 27, 15, 0, 0, 0, time.UTC)
	freshObs := now.Add(-time.Minute).Format(time.RFC3339Nano)
	for _, tc := range []struct{ label, archivedAt string }{
		{"zero", time.Time{}.Format(time.RFC3339Nano)},
		{"empty", ""},
		{"future", now.Add(48 * time.Hour).Format(time.RFC3339Nano)},
	} {
		mirror := buildOrgArchiveMirrorDoctorCheck([]db.OrgRoleArchived{{Role: "scout", ArchivedAt: tc.archivedAt, ObservedAt: freshObs}}, nil, now.Add(-time.Minute), true, now)
		if mirror.OK || !strings.Contains(mirror.Detail, "UNKNOWN") {
			t.Fatalf("mirror row with %s archived_at = %+v, want UNKNOWN — the fourth timestamp must be visible to the guard", tc.label, mirror)
		}
		pending := buildOrgArchiveMirrorDoctorCheck(nil, []db.OrgRoleArchived{{Role: "scout", ArchivedAt: tc.archivedAt, ObservedAt: freshObs}}, now.Add(-time.Minute), true, now)
		if pending.OK || !strings.Contains(pending.Detail, "UNKNOWN") {
			t.Fatalf("pending row with %s archived_at = %+v, want UNKNOWN", tc.label, pending)
		}
	}
}

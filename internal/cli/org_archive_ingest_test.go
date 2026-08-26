package cli

import (
	"context"
	"errors"
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

func TestParseHerdrArchivedAgents(t *testing.T) {
	observed := time.Date(2026, 8, 26, 15, 0, 0, 0, time.UTC)
	archived, parked, err := parseHerdrArchivedAgents([]byte(herdrAgentListArchivedFixture), observed)
	if err != nil {
		t.Fatal(err)
	}
	if len(archived) != 1 {
		t.Fatalf("archived = %v, want only scout — presence of the block is the signal, agent_status is not", archived)
	}
	obs, ok := archived["scout"]
	if !ok || obs.By != "herdr-app" || obs.Reason != "firing test row" ||
		!obs.At.Equal(time.Date(2026, 8, 26, 14, 29, 33, 879756070, time.UTC)) || !obs.ObservedAt.Equal(observed) {
		t.Fatalf("scout observation = %+v", obs)
	}
	if !strings.Contains(parked["scout"], "jerryfane/herdrup#173") || !strings.Contains(parked["scout"], "85471") {
		t.Fatalf("parked_work not carried verbatim: %q", parked["scout"])
	}

	if _, _, err := parseHerdrArchivedAgents([]byte("not json"), observed); err == nil {
		t.Fatal("malformed read parsed; a malformed read must never look like an empty fleet")
	}
	if _, _, err := parseHerdrArchivedAgents([]byte(`{"result":{}}`), observed); err == nil {
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
	fresh := buildOrgArchiveMirrorDoctorCheck(rows, now.Add(-2*time.Minute), true, now)
	if !fresh.OK || !strings.Contains(fresh.Detail, "scout") {
		t.Fatalf("fresh = %+v", fresh)
	}
	stale := buildOrgArchiveMirrorDoctorCheck(rows, now.Add(-16*time.Minute), true, now)
	if stale.OK || !strings.Contains(stale.Detail, "STALE") || !strings.Contains(stale.Detail, "exclusions preserved") {
		t.Fatalf("stale = %+v", stale)
	}
	never := buildOrgArchiveMirrorDoctorCheck(rows, time.Time{}, false, now)
	if never.OK || !strings.Contains(never.Detail, "NO recorded successful herdr poll") {
		t.Fatalf("never-succeeded = %+v", never)
	}
}

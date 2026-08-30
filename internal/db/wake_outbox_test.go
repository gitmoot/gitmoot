package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestWakeOutboxStateClassificationIsExhaustive(t *testing.T) {
	tests := []struct {
		state          string
		interpretation wakeOutboxStateInterpretation
	}{
		{WakeOutboxStatePending, wakeOutboxStatePendingObligation},
		{WakeOutboxStateAttempted, wakeOutboxStateAgedAttemptObligation},
		{WakeOutboxStateDelivered, wakeOutboxStateTerminal},
		{WakeOutboxStateStalled, wakeOutboxStateTerminal},
		{WakeOutboxStateFailed, wakeOutboxStateTerminal},
		{WakeOutboxStateSuperseded, wakeOutboxStateTerminal},
		{WakeOutboxStateDeliveryUnknown, wakeOutboxStateDeliveryUnknown},
	}
	if len(tests) != int(wakeOutboxStateCount) {
		t.Fatalf("classified states = %d, declared states = %d", len(tests), wakeOutboxStateCount)
	}
	for _, test := range tests {
		interpretation, ok := interpretWakeOutboxState(test.state)
		if !ok || interpretation != test.interpretation {
			t.Errorf(
				"interpretWakeOutboxState(%q) = (%d, %v), want (%d, true)",
				test.state, interpretation, ok, test.interpretation,
			)
		}
	}
}

type recordingWakeOutboxQueryer struct {
	delegate wakeOutboxQueryer
	query    string
	args     []any
}

func (r *recordingWakeOutboxQueryer) QueryContext(
	ctx context.Context,
	query string,
	args ...any,
) (*sql.Rows, error) {
	r.query = query
	r.args = append([]any(nil), args...)
	return r.delegate.QueryContext(ctx, query, args...)
}

const expectedWakeOutboxObligationQuery = `
SELECT id, source_kind, source_id, target_role, coalesce_key, state,
		attempt_count, last_error, created_at, COALESCE(attempted_at, ''),
		COALESCE(finished_at, ''), updated_at,
		CASE
			WHEN source_kind != 'workflow_note' OR coalesce_key NOT LIKE 'directive:%' THEN ''
			WHEN EXISTS (
				SELECT 1
				FROM workflow_notes d
				JOIN workflow_notes r ON r.workflow_id = d.workflow_id
				WHERE d.id = CAST(wake_outbox.source_id AS INTEGER)
					AND (
						substr(r.body, 1, length('[org:directive-cancel id=' || wake_outbox.source_id || ' ')) = '[org:directive-cancel id=' || wake_outbox.source_id || ' '
						OR substr(r.body, 1, length('[org:directive-done id=' || wake_outbox.source_id || ' ')) = '[org:directive-done id=' || wake_outbox.source_id || ' '
					)
			) THEN 'terminal'
			WHEN EXISTS (
				SELECT 1
				FROM workflow_notes d
				JOIN workflow_notes r ON r.workflow_id = d.workflow_id
				WHERE d.id = CAST(wake_outbox.source_id AS INTEGER)
					AND substr(r.body, 1, length('[org:directive-ack id=' || wake_outbox.source_id || ' ')) = '[org:directive-ack id=' || wake_outbox.source_id || ' '
			) THEN 'completion'
			ELSE 'acknowledgment'
		END
FROM wake_outbox
WHERE state = ? OR (state = ? AND attempted_at IS NOT NULL AND attempted_at <= ?)
ORDER BY created_at, id`

func TestListWakeOutboxObligationsExecutesGeneratedQuery(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	attemptedBefore := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	recorder := &recordingWakeOutboxQueryer{delegate: store.db}

	if _, err := listWakeOutboxObligations(ctx, recorder, attemptedBefore); err != nil {
		t.Fatal(err)
	}
	if recorder.query != expectedWakeOutboxObligationQuery {
		t.Fatal("executed query does not match the independent obligation contract")
	}
	wantArgs := []any{
		"pending",
		"attempted",
		attemptedBefore.UTC().Format(BlockedEpisodeTimeLayout),
	}
	if !reflect.DeepEqual(recorder.args, wantArgs) {
		t.Fatalf("executed args = %#v, want independent args %#v", recorder.args, wantArgs)
	}
}

func TestListWakeOutboxObligationsScopesDirectiveReceiptsToWorkflow(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	addDirective := func(workflowID string) WorkflowNote {
		note, err := store.InsertWorkflowNote(ctx, WorkflowNote{
			WorkflowID:        workflowID,
			Author:            "owner",
			Body:              "[org:directive to=worker from=owner wf=" + workflowID + "]\nact",
			AddressedTarget:   "worker",
			AddressedWakeKind: WakeOutboxKindDirective,
		})
		if err != nil {
			t.Fatal(err)
		}
		return note
	}
	addReceipt := func(workflowID, kind string, directiveID int64) {
		if _, err := store.InsertWorkflowNote(ctx, WorkflowNote{
			WorkflowID: workflowID,
			Author:     "worker",
			Body:       fmt.Sprintf("[org:directive-%s id=%d by=worker]", kind, directiveID),
		}); err != nil {
			t.Fatal(err)
		}
	}

	unread := addDirective("phase/main")
	crossWorkflow := addDirective("phase/main")
	acknowledged := addDirective("phase/main")
	terminated := addDirective("phase/main")
	addReceipt("phase/other", "done", crossWorkflow.ID)
	addReceipt("phase/main", "ack", acknowledged.ID)
	addReceipt("phase/main", "done", terminated.ID)

	obligations, err := store.ListWakeOutboxObligations(ctx, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[int64]string, len(obligations.Pending))
	for _, obligation := range obligations.Pending {
		sourceID, err := strconv.ParseInt(obligation.SourceID, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		got[sourceID] = obligation.DirectivePhase
	}
	want := map[int64]string{
		unread.ID:        WakeOutboxDirectivePhaseAcknowledgment,
		crossWorkflow.ID: WakeOutboxDirectivePhaseAcknowledgment,
		acknowledged.ID:  WakeOutboxDirectivePhaseCompletion,
		terminated.ID:    WakeOutboxDirectivePhaseTerminal,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("directive phases = %v, want %v", got, want)
	}
}

func TestWakeOutboxEntryStateRemainsSerializable(t *testing.T) {
	encoded, err := json.Marshal(WakeOutboxEntry{State: WakeOutboxStatePending})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"state":"pending"`) {
		t.Fatalf("encoded wake outbox entry = %s, want serialized state", encoded)
	}
	var decoded WakeOutboxEntry
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.State != WakeOutboxStatePending {
		t.Fatalf("decoded state = %q, want %q", decoded.State, WakeOutboxStatePending)
	}
}

func TestListWakeOutboxObligationsClassifiesAllStates(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	attemptedBefore := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	states := []struct {
		state       string
		attemptedAt any
	}{
		{WakeOutboxStatePending, nil},
		{WakeOutboxStateAttempted, attemptedBefore.Add(-time.Minute).Format(BlockedEpisodeTimeLayout)},
		{WakeOutboxStateDelivered, nil},
		{WakeOutboxStateStalled, nil},
		{WakeOutboxStateFailed, nil},
		{WakeOutboxStateDeliveryUnknown, nil},
	}
	for index, state := range states {
		if _, err := store.db.ExecContext(ctx, `
INSERT INTO wake_outbox(
	source_kind, source_id, target_role, coalesce_key, state, attempted_at
) VALUES ('test', ?, 'owner', 'reply:owner', ?, ?)`,
			strconv.Itoa(index), state.state, state.attemptedAt,
		); err != nil {
			t.Fatal(err)
		}
	}

	obligations, err := store.ListWakeOutboxObligations(ctx, attemptedBefore)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for range obligations.Pending {
		got = append(got, WakeOutboxStatePending)
	}
	for range obligations.AgedAttempted {
		got = append(got, WakeOutboxStateAttempted)
	}
	want := []string{WakeOutboxStatePending, WakeOutboxStateAttempted}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("obligation states = %v, want %v", got, want)
	}
}

func TestAddressedWorkflowNoteWritesOutboxThroughEveryInsertEntryPoint(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	observation := func(content string) MemoryObservation {
		return MemoryObservation{
			Owner: MemoryOwner{Kind: "shared", Ref: "shared"}, AuthorRef: "operator",
			Repo: "owner/repo", Scope: "repo", Content: content, TrustMark: "low",
		}
	}

	var noteIDs []int64
	first, err := store.InsertWorkflowNote(ctx, WorkflowNote{
		WorkflowID: "wake/simple", Author: "operator", Body: "simple",
		AddressedTarget: "OWNER",
	})
	if err != nil {
		t.Fatal(err)
	}
	noteIDs = append(noteIDs, first.ID)

	second, err := store.InsertWorkflowNoteWithMeta(ctx, WorkflowNote{
		WorkflowID: "wake/meta", Author: "operator", Body: "meta",
		AddressedTarget: "owner",
	}, WorkflowMeta{Summary: "summary", SummarySet: true})
	if err != nil {
		t.Fatal(err)
	}
	noteIDs = append(noteIDs, second.ID)

	third, _, err := store.InsertWorkflowNoteWithObservation(ctx, WorkflowNote{
		WorkflowID: "wake/observation", Author: "operator", Body: "observation",
		Repo: "owner/repo", AddressedTarget: "owner",
	}, observation("observation"))
	if err != nil {
		t.Fatal(err)
	}
	noteIDs = append(noteIDs, third.ID)

	fourth, _, err := store.InsertWorkflowNoteWithObservationAndMeta(ctx, WorkflowNote{
		WorkflowID: "wake/both", Author: "operator", Body: "both",
		Repo: "owner/repo", AddressedTarget: "owner",
	}, observation("both"), WorkflowMeta{Description: "description", DescriptionSet: true})
	if err != nil {
		t.Fatal(err)
	}
	noteIDs = append(noteIDs, fourth.ID)

	outbox, err := store.ListWakeOutbox(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(outbox) != 4 {
		t.Fatalf("outbox rows = %+v, want four", outbox)
	}
	for index, entry := range outbox {
		if entry.SourceKind != WakeOutboxSourceWorkflowNote ||
			entry.SourceID != strconv.FormatInt(noteIDs[index], 10) ||
			entry.TargetRole != "owner" ||
			entry.CoalesceKey != WakeOutboxReplyCoalescePrefix+"owner" ||
			entry.State != WakeOutboxStatePending ||
			entry.AttemptCount != 0 || entry.AttemptedAt != "" || entry.FinishedAt != "" {
			t.Fatalf("outbox[%d] = %+v", index, entry)
		}
	}
}

func TestAddressedWorkflowNoteAndOutboxAreAtomic(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	if _, err := store.db.ExecContext(ctx, `
CREATE TRIGGER reject_wake_outbox
BEFORE INSERT ON wake_outbox
BEGIN
	SELECT RAISE(ABORT, 'forced wake outbox failure');
END;`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.InsertWorkflowNote(ctx, WorkflowNote{
		WorkflowID: "wake/atomic", Author: "operator", Body: "must roll back",
		AddressedTarget: "owner",
	}); err == nil {
		t.Fatal("InsertWorkflowNote unexpectedly succeeded")
	}
	for _, table := range []string{"workflow_notes", "wake_outbox"} {
		var count int
		if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s count = %d after failed transaction, want zero", table, count)
		}
	}
}

func TestAddressedChatMessageWritesOneOutboxRowPerRole(t *testing.T) {
	store := openChatTestStore(t)
	ctx := context.Background()
	thread, err := store.CreateChatThread(ctx, ChatThread{Slug: "wake-chat", Repo: "owner/repo"})
	if err != nil {
		t.Fatal(err)
	}
	message, err := store.AddChatMessage(ctx, ChatMessage{
		ThreadID: thread.ID, AuthorName: "human", Kind: ChatKindChat,
		Body:     "@owner and @reviewer",
		Mentions: []string{"Owner", "owner", " reviewer ", ""},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddChatMessage(ctx, ChatMessage{
		ThreadID: thread.ID, AuthorKind: ChatAuthorKindAgent, AuthorName: "worker",
		Kind: ChatKindJobResult, Body: "non-triggering back-link", Mentions: []string{"owner"},
	}); err != nil {
		t.Fatal(err)
	}

	outbox, err := store.ListWakeOutbox(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(outbox) != 2 {
		t.Fatalf("outbox rows = %+v, want one per distinct addressed role", outbox)
	}
	byRole := make(map[string]WakeOutboxEntry, len(outbox))
	for _, entry := range outbox {
		byRole[entry.TargetRole] = entry
	}
	for _, role := range []string{"owner", "reviewer"} {
		entry, ok := byRole[role]
		if !ok {
			t.Fatalf("outbox roles = %+v, want %q", byRole, role)
		}
		if entry.SourceKind != WakeOutboxSourceChatMessage ||
			entry.SourceID != message.ID ||
			entry.CoalesceKey != WakeOutboxReplyCoalescePrefix+role ||
			entry.State != WakeOutboxStatePending {
			t.Fatalf("outbox[%s] = %+v", role, entry)
		}
	}
}

func TestAddressedChatMessageAndOutboxAreAtomic(t *testing.T) {
	store := openChatTestStore(t)
	ctx := context.Background()
	thread, err := store.CreateChatThread(ctx, ChatThread{Slug: "wake-atomic-chat", Repo: "owner/repo"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
CREATE TRIGGER reject_chat_wake_outbox
BEFORE INSERT ON wake_outbox
WHEN NEW.source_kind = 'chat_message'
BEGIN
	SELECT RAISE(ABORT, 'forced chat wake outbox failure');
END;`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddChatMessage(ctx, ChatMessage{
		ThreadID: thread.ID, AuthorName: "human", Kind: ChatKindChat,
		Body: "@owner must roll back", Mentions: []string{"owner"},
	}); err == nil {
		t.Fatal("AddChatMessage unexpectedly succeeded")
	}
	for _, table := range []string{"chat_messages", "wake_outbox"} {
		var count int
		if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s count = %d after failed transaction, want zero", table, count)
		}
	}
}

func TestWakeOutboxClaimAndFinishStatesAreQueryable(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	note, err := store.InsertWorkflowNote(ctx, WorkflowNote{
		WorkflowID: "wake/state", Author: "operator", Body: "state",
		AddressedTarget: "owner",
	})
	if err != nil {
		t.Fatal(err)
	}
	pending, err := store.ListWakeOutbox(ctx, WakeOutboxStatePending)
	if err != nil || len(pending) != 1 || pending[0].SourceID != fmt.Sprint(note.ID) {
		t.Fatalf("pending = %+v, err=%v", pending, err)
	}
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	claimed, err := store.ClaimWakeOutbox(ctx, []int64{pending[0].ID}, now)
	if err != nil || !claimed {
		t.Fatalf("ClaimWakeOutbox = %v, %v", claimed, err)
	}
	attempted, err := store.ListWakeOutbox(ctx, WakeOutboxStateAttempted)
	if err != nil || len(attempted) != 1 || attempted[0].AttemptCount != 1 || attempted[0].AttemptedAt == "" {
		t.Fatalf("attempted = %+v, err=%v", attempted, err)
	}
	if err := store.FinishWakeOutbox(ctx, []int64{pending[0].ID}, WakeOutboxStateDelivered, "", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	delivered, err := store.ListWakeOutbox(ctx, WakeOutboxStateDelivered)
	if err != nil || len(delivered) != 1 || delivered[0].FinishedAt == "" {
		t.Fatalf("delivered = %+v, err=%v", delivered, err)
	}
}

func TestExpireAgedWakeOutboxRecordsDeliveryUnknownWithoutRetry(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	if _, err := store.InsertWorkflowNote(ctx, WorkflowNote{
		WorkflowID: "wake/unknown", Author: "operator", Body: "unknown",
		AddressedTarget: "owner",
	}); err != nil {
		t.Fatal(err)
	}
	pending, err := store.ListWakeOutbox(ctx, WakeOutboxStatePending)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending = %+v, err=%v", pending, err)
	}
	attemptedAt := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	claimed, err := store.ClaimWakeOutbox(ctx, []int64{pending[0].ID}, attemptedAt)
	if err != nil || !claimed {
		t.Fatalf("claim = %v, err=%v", claimed, err)
	}

	obligations, err := store.ListWakeOutboxObligations(ctx, attemptedAt)
	if err != nil || len(obligations.Pending) != 0 || len(obligations.AgedAttempted) != 1 {
		t.Fatalf("obligations = %+v, err=%v", obligations, err)
	}
	expired, err := store.ExpireAgedWakeOutbox(ctx, attemptedAt, attemptedAt.Add(time.Minute))
	if err != nil || len(expired) != 1 || expired[0].ID != pending[0].ID {
		t.Fatalf("expired = %+v, err=%v", expired, err)
	}
	unknown, err := store.ListWakeOutbox(ctx, WakeOutboxStateDeliveryUnknown)
	if err != nil || len(unknown) != 1 ||
		unknown[0].AttemptCount != 1 ||
		unknown[0].FinishedAt == "" ||
		!strings.Contains(unknown[0].LastError, "not retried") {
		t.Fatalf("delivery unknown = %+v, err=%v", unknown, err)
	}
	events, err := store.ListJobEvents(ctx, fmt.Sprintf("wake-outbox:%d", pending[0].ID))
	if err != nil || len(events) != 1 ||
		events[0].Kind != WakeOutboxDeliveryUnknownEventKind ||
		!strings.Contains(events[0].Message, "policy=expire_without_retry") {
		t.Fatalf("delivery unknown events = %+v, err=%v", events, err)
	}
	obligations, err = store.ListWakeOutboxObligations(ctx, attemptedAt.Add(time.Hour))
	if err != nil || obligations.Len() != 0 {
		t.Fatalf("obligations after expiry = %+v, err=%v", obligations, err)
	}
	expired, err = store.ExpireAgedWakeOutbox(ctx, attemptedAt.Add(time.Hour), attemptedAt.Add(2*time.Hour))
	if err != nil || len(expired) != 0 {
		t.Fatalf("second expiry = %+v, err=%v", expired, err)
	}
	events, err = store.ListJobEvents(ctx, fmt.Sprintf("wake-outbox:%d", pending[0].ID))
	if err != nil || len(events) != 1 {
		t.Fatalf("events after second expiry = %+v, err=%v", events, err)
	}
}

func TestWakeOutboxSupersededMigrationPreservesRowsAndScopesTerminalMarkers(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gitmoot.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	preMigration := &Store{db: raw}
	for version, migration := range migrationsBefore(
		t,
		"ALTER TABLE wake_outbox RENAME TO wake_outbox_before_superseded",
	) {
		if err := preMigration.applyMigration(ctx, version+1, migration); err != nil {
			t.Fatalf("applyMigration(%d): %v", version+1, err)
		}
	}
	addDirective := func(workflowID string) WorkflowNote {
		note, err := preMigration.InsertWorkflowNote(ctx, WorkflowNote{
			WorkflowID: workflowID,
			Author:     "owner",
			Body:       "[org:directive to=worker from=owner wf=" + workflowID + "]\nact",
		})
		if err != nil {
			t.Fatal(err)
		}
		return note
	}
	addReceipt := func(workflowID, kind string, directiveID int64) {
		if _, err := preMigration.InsertWorkflowNote(ctx, WorkflowNote{
			WorkflowID: workflowID,
			Author:     "worker",
			Body:       fmt.Sprintf("[org:directive-%s id=%d by=worker]", kind, directiveID),
		}); err != nil {
			t.Fatal(err)
		}
	}

	terminalPending := addDirective("migration/main")
	crossWorkflow := addDirective("migration/main")
	acknowledged := addDirective("migration/main")
	terminalAttempted := addDirective("migration/main")
	addReceipt("migration/main", "done", terminalPending.ID)
	addReceipt("migration/other", "done", crossWorkflow.ID)
	addReceipt("migration/main", "ack", acknowledged.ID)
	addReceipt("migration/main", "cancel", terminalAttempted.ID)

	type fixture struct {
		id           int64
		sourceKind   string
		sourceID     string
		coalesceKey  string
		state        string
		attemptCount int
		lastError    string
		createdAt    string
		attemptedAt  any
		finishedAt   any
		updatedAt    string
	}
	fixtures := []fixture{
		{101, WakeOutboxSourceWorkflowNote, strconv.FormatInt(terminalPending.ID, 10), "directive:worker", WakeOutboxStatePending, 0, "", "2026-08-30T01:00:00.000Z", nil, nil, "2026-08-30T01:00:01.000Z"},
		{102, WakeOutboxSourceWorkflowNote, strconv.FormatInt(crossWorkflow.ID, 10), "directive:worker", WakeOutboxStatePending, 1, "cross-workflow-control", "2026-08-30T02:00:00.000Z", nil, nil, "2026-08-30T02:00:01.000Z"},
		{103, WakeOutboxSourceWorkflowNote, strconv.FormatInt(acknowledged.ID, 10), "directive:worker", WakeOutboxStatePending, 2, "completion-control", "2026-08-30T03:00:00.000Z", nil, nil, "2026-08-30T03:00:01.000Z"},
		{104, WakeOutboxSourceWorkflowNote, strconv.FormatInt(terminalAttempted.ID, 10), "directive:worker", WakeOutboxStateAttempted, 3, "attempted-control", "2026-08-30T04:00:00.000Z", "2026-08-30T04:00:02.000Z", nil, "2026-08-30T04:00:03.000Z"},
		{105, WakeOutboxSourceWorkflowNote, "generic", "reply:worker", WakeOutboxStateDelivered, 4, "generic-control", "2026-08-30T05:00:00.000Z", "2026-08-30T05:00:02.000Z", "2026-08-30T05:00:03.000Z", "2026-08-30T05:00:04.000Z"},
	}
	for _, row := range fixtures {
		if _, err := raw.ExecContext(ctx, `
INSERT INTO wake_outbox(
	id, source_kind, source_id, target_role, coalesce_key, state,
	attempt_count, last_error, created_at, attempted_at, finished_at, updated_at
) VALUES (?, ?, ?, 'worker', ?, ?, ?, ?, ?, ?, ?, ?)`,
			row.id, row.sourceKind, row.sourceID, row.coalesceKey, row.state,
			row.attemptCount, row.lastError, row.createdAt, row.attemptedAt,
			row.finishedAt, row.updatedAt,
		); err != nil {
			t.Fatal(err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	upgraded, err := openRealTestStore(t, path)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	rows, err := upgraded.ListWakeOutbox(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(fixtures) {
		t.Fatalf("migrated rows = %+v, want %d", rows, len(fixtures))
	}
	byID := make(map[int64]WakeOutboxEntry, len(rows))
	for _, row := range rows {
		byID[row.ID] = row
	}
	superseded := byID[101]
	if superseded.State != WakeOutboxStateSuperseded ||
		superseded.CreatedAt != fixtures[0].createdAt ||
		superseded.AttemptCount != fixtures[0].attemptCount ||
		superseded.FinishedAt == "" ||
		superseded.UpdatedAt == fixtures[0].updatedAt {
		t.Fatalf("terminal pending migration = %+v", superseded)
	}
	for _, fixture := range fixtures[1:] {
		row := byID[fixture.id]
		wantAttempted, _ := fixture.attemptedAt.(string)
		wantFinished, _ := fixture.finishedAt.(string)
		if row.SourceKind != fixture.sourceKind ||
			row.SourceID != fixture.sourceID ||
			row.CoalesceKey != fixture.coalesceKey ||
			row.State != fixture.state ||
			row.AttemptCount != fixture.attemptCount ||
			row.LastError != fixture.lastError ||
			row.CreatedAt != fixture.createdAt ||
			row.AttemptedAt != wantAttempted ||
			row.FinishedAt != wantFinished ||
			row.UpdatedAt != fixture.updatedAt {
			t.Fatalf("migration changed control row %d: %+v", fixture.id, row)
		}
	}
	var indexCount int
	if err := upgraded.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM sqlite_master
WHERE type = 'index' AND name IN ('idx_wake_outbox_pending', 'idx_wake_outbox_attempted')`,
	).Scan(&indexCount); err != nil {
		t.Fatal(err)
	}
	if indexCount != 2 {
		t.Fatalf("wake outbox indexes after migration = %d, want 2", indexCount)
	}
}

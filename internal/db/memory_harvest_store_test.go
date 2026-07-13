package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMemoryHarvestReceiptStateMachine(t *testing.T) {
	ctx := context.Background()
	store := openMemTestStore(t)
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)

	claimed, err := store.ClaimMemoryHarvestRun(ctx, "job-1", "hash-1", now, now.Add(-2*time.Minute))
	if err != nil || !claimed {
		t.Fatalf("fresh claim = %v, err=%v", claimed, err)
	}
	claimed, err = store.ClaimMemoryHarvestRun(ctx, "job-1", "hash-1", now.Add(time.Minute), now.Add(-time.Minute))
	if err != nil || claimed {
		t.Fatalf("live claimed receipt reclaimed = %v, err=%v", claimed, err)
	}
	claimed, err = store.ClaimMemoryHarvestRun(ctx, "job-1", "hash-1", now.Add(3*time.Minute), now.Add(2*time.Minute))
	if err != nil || !claimed {
		t.Fatalf("expired never-started claim = %v, err=%v", claimed, err)
	}
	started, err := store.StartMemoryHarvestRun(ctx, "job-1", "hash-1", now.Add(3*time.Minute))
	if err != nil || !started {
		t.Fatalf("start = %v, err=%v", started, err)
	}
	claimed, err = store.ClaimMemoryHarvestRun(ctx, "job-1", "hash-1", now.Add(10*time.Minute), now.Add(9*time.Minute))
	if err != nil || claimed {
		t.Fatalf("started receipt must never reclaim = %v, err=%v", claimed, err)
	}

	expired, err := store.ExpireStartedMemoryHarvestRuns(ctx, now.Add(4*time.Minute), now.Add(5*time.Minute))
	if err != nil || len(expired) != 1 || expired[0].JobID != "job-1" {
		t.Fatalf("expire started = %+v, err=%v", expired, err)
	}
	run, ok, err := store.GetMemoryHarvestRun(ctx, "job-1", "hash-1")
	if err != nil || !ok || run.State != MemoryHarvestUncertain {
		t.Fatalf("expired run = %+v ok=%v err=%v", run, ok, err)
	}
}

func TestCompleteMemoryHarvestRunTransactionRollback(t *testing.T) {
	ctx := context.Background()
	store := openMemTestStore(t)
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	if claimed, err := store.ClaimMemoryHarvestRun(ctx, "job-rollback", "hash", now, now.Add(-time.Minute)); err != nil || !claimed {
		t.Fatalf("claim = %v err=%v", claimed, err)
	}
	if started, err := store.StartMemoryHarvestRun(ctx, "job-rollback", "hash", now); err != nil || !started {
		t.Fatalf("start = %v err=%v", started, err)
	}
	// The second row is invalid. The first insert and receipt transition must both
	// roll back, leaving the started receipt visible for uncertain handling.
	err := store.CompleteMemoryHarvestRun(ctx, "job-rollback", "hash", []MemoryObservation{
		{Owner: MemoryOwner{Kind: "shared", Ref: "shared"}, Repo: "owner/repo", Scope: "repo", Key: "ok", Content: "valid durable fact"},
		{Owner: MemoryOwner{}, Repo: "owner/repo", Scope: "repo", Key: "bad", Content: "invalid owner"},
	}, now.Add(time.Second))
	if err == nil {
		t.Fatal("expected staging transaction to fail")
	}
	observations, err := store.ListMemoryObservations(ctx, "", "owner/repo")
	if err != nil || len(observations) != 0 {
		t.Fatalf("rollback observations = %+v err=%v", observations, err)
	}
	run, ok, err := store.GetMemoryHarvestRun(ctx, "job-rollback", "hash")
	if err != nil || !ok || run.State != MemoryHarvestStarted {
		t.Fatalf("rollback receipt = %+v ok=%v err=%v", run, ok, err)
	}
}

func TestMemoryHarvestMigrationFreshAndUpgrade(t *testing.T) {
	ctx := context.Background()
	fresh := openMemTestStore(t)
	for _, table := range []string{"memory_harvest_runs", "memory_harvest_state"} {
		if ok, err := fresh.tableExists(ctx, table); err != nil || !ok {
			t.Fatalf("fresh table %s: ok=%v err=%v", table, ok, err)
		}
	}

	migrationIndex := -1
	for i, migration := range migrations {
		if strings.Contains(migration, "CREATE TABLE memory_harvest_runs") {
			migrationIndex = i
			break
		}
	}
	if migrationIndex < 0 {
		t.Fatal("memory harvest migration not found")
	}
	raw, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "upgrade.db"))
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer raw.Close()
	raw.SetMaxOpenConns(1)
	if err := configureWritableSQLite(ctx, raw); err != nil {
		t.Fatalf("configure raw: %v", err)
	}
	upgrade := &Store{db: raw}
	for i, migration := range migrations[:migrationIndex] {
		if err := upgrade.applyMigration(ctx, i+1, migration); err != nil {
			t.Fatalf("apply pre-harvest migration %d: %v", i+1, err)
		}
	}
	if _, err := raw.ExecContext(ctx, `INSERT INTO jobs(id, agent, type, state, payload) VALUES ('legacy-harvest', 'a', 'ask', 'succeeded', '{}')`); err != nil {
		t.Fatalf("seed legacy job: %v", err)
	}
	if err := upgrade.applyMigration(ctx, migrationIndex+1, migrations[migrationIndex]); err != nil {
		t.Fatalf("apply harvest migration: %v", err)
	}
	var resultHash string
	if err := raw.QueryRowContext(ctx, `SELECT result_hash FROM jobs WHERE id='legacy-harvest'`).Scan(&resultHash); err != nil {
		t.Fatalf("read upgraded result_hash: %v", err)
	}
	if resultHash != "" {
		t.Fatalf("legacy result_hash = %q, want empty (no silent backfill)", resultHash)
	}
}

func TestJobResultHashMissingAndNullStayEmpty(t *testing.T) {
	for _, payload := range []string{
		`{}`,
		`{"repo":"owner/repo"}`,
		`{"repo":"owner/repo","result":null}`,
	} {
		if got := jobResultHashFromPayload(payload); got != "" {
			t.Fatalf("jobResultHashFromPayload(%s)=%q, want empty", payload, got)
		}
	}
	if got := jobResultHashFromPayload(`{"result":{"decision":"approved","summary":"done"}}`); got == "" {
		t.Fatal("non-null result produced an empty hash")
	}
}

func TestMemoryHarvestCandidateQueryUsesPartialIndex(t *testing.T) {
	ctx := context.Background()
	store := openMemTestStore(t)
	rows, err := store.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+ListMemoryHarvestCandidatesSQL,
		memoryHarvestDBTime(time.Now()), memoryHarvestInspectTestLimit)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		plan.WriteString(detail)
		plan.WriteByte('\n')
	}
	if !strings.Contains(plan.String(), "idx_jobs_memory_harvest_terminal") {
		t.Fatalf("candidate query does not use terminal partial index:\n%s", plan.String())
	}
}

func TestMemoryHarvestHighWaterAndBlockedResultHash(t *testing.T) {
	ctx := context.Background()
	store := openMemTestStore(t)
	oldPayload := `{"repo":"owner/repo","result":{"decision":"blocked","summary":"old result"}}`
	if err := store.CreateJob(ctx, Job{ID: "before-enable", Agent: "a", Type: "ask", State: "blocked", Payload: oldPayload}); err != nil {
		t.Fatalf("create pre-enable job: %v", err)
	}
	initialized, err := store.InitializeMemoryHarvestState(ctx)
	if err != nil || !initialized {
		t.Fatalf("initialize = %v err=%v", initialized, err)
	}
	rows, err := store.ListMemoryHarvestCandidates(ctx, time.Now().Add(-time.Minute), memoryHarvestInspectTestLimit)
	if err != nil || len(rows) != 0 {
		t.Fatalf("pre-enable history leaked into harvest: %+v err=%v", rows, err)
	}
	if err := store.CreateJob(ctx, Job{ID: "resultless", Agent: "a", Type: "ask", State: "succeeded", Payload: `{"repo":"owner/repo","result":null}`}); err != nil {
		t.Fatalf("create resultless terminal job: %v", err)
	}
	rows, err = store.ListMemoryHarvestCandidates(ctx, time.Now().Add(-time.Minute), memoryHarvestInspectTestLimit)
	if err != nil || len(rows) != 0 {
		t.Fatalf("resultless terminal job entered harvest: %+v err=%v", rows, err)
	}

	first := `{"repo":"owner/repo","result":{"decision":"blocked","summary":"first settled result"}}`
	if err := store.CreateJob(ctx, Job{ID: "resumable", Agent: "a", Type: "ask", State: "blocked", Payload: first}); err != nil {
		t.Fatalf("create blocked job: %v", err)
	}
	rows, err = store.ListMemoryHarvestCandidates(ctx, time.Now().Add(-time.Minute), memoryHarvestInspectTestLimit)
	if err != nil || len(rows) != 1 || rows[0].ResultHash != jobResultHashFromPayload(first) {
		t.Fatalf("first blocked candidate = %+v err=%v", rows, err)
	}
	now := time.Now().UTC()
	if claimed, err := store.ClaimMemoryHarvestRun(ctx, rows[0].JobID, rows[0].ResultHash, now, now.Add(-time.Minute)); err != nil || !claimed {
		t.Fatalf("claim first result = %v err=%v", claimed, err)
	}
	if err := store.SkipMemoryHarvestRun(ctx, rows[0].JobID, rows[0].ResultHash, "test", now); err != nil {
		t.Fatalf("skip first result: %v", err)
	}

	second := `{"repo":"owner/repo","result":{"decision":"blocked","summary":"second settled result"}}`
	if err := store.UpdateJobPayload(ctx, "resumable", second); err != nil {
		t.Fatalf("update blocked result: %v", err)
	}
	rows, err = store.ListMemoryHarvestCandidates(ctx, now.Add(-time.Minute), memoryHarvestInspectTestLimit)
	if err != nil || len(rows) != 1 || rows[0].ResultHash != jobResultHashFromPayload(second) {
		t.Fatalf("second blocked candidate = %+v err=%v", rows, err)
	}
}

const memoryHarvestInspectTestLimit = 50

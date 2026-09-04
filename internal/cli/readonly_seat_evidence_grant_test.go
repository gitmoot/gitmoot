package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// seedVerdictStore creates a REAL database at paths.Database holding one prior
// review verdict, then leaves it in WAL mode - which is the state that made the
// first version of this fix inert.
func seedVerdictStore(t *testing.T, paths config.Paths, summary string) {
	t.Helper()
	if err := os.MkdirAll(paths.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	payload, err := json.Marshal(workflow.JobPayload{
		Repo:        "gitmoot/gitmoot",
		PullRequest: 1845,
		Result:      &workflow.AgentResult{Decision: "approved", Summary: summary, Evidence: workflow.EvidenceExecuted},
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if err := store.UpsertAgent(context.Background(), db.Agent{
		Name: "gm-omp-gate", Role: "implementer", Runtime: "codex", RuntimeRef: "last",
		RepoScope: "gitmoot/gitmoot", Capabilities: []string{"implement"},
		AutonomyPolicy: "auto", HealthStatus: "ok",
	}); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	if err := store.CreateJobWithEvent(context.Background(), db.Job{
		ID: "prior-verdict", Agent: "gm-review-opus", Type: "review",
		State: string(workflow.JobSucceeded), Payload: string(payload),
	}, db.JobEvent{Kind: string(workflow.JobSucceeded), Message: "verdict"}); err != nil {
		t.Fatalf("seed verdict: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
}

// TestEvidenceSnapshotCanActuallyBeQueried is the SUFFICIENCY test the round-1
// version lacked.
//
// That version wrote the 6-byte string "sqlite" as the database and asserted
// the path appeared in a grant slice. It therefore could not notice that a
// read-only seat cannot open the live WAL store AT ALL - SQLite opens the -wal
// and -shm sidecars O_RDWR|O_CREAT, which a read-file grant refuses. Path
// membership is not readability, so this test opens the staged copy and reads
// a verdict out of it.
func TestEvidenceSnapshotCanActuallyBeQueried(t *testing.T) {
	live := config.PathsForHome(t.TempDir())
	seedVerdictStore(t, live, "the prior verdict a reviewer must be able to enumerate")

	cacheRoot := t.TempDir()
	evidenceHome, diagnostic := stageReviewEvidenceSnapshot(context.Background(), live, cacheRoot)
	if diagnostic != "" {
		t.Fatalf("staging reported %q", diagnostic)
	}
	if evidenceHome == "" {
		t.Fatal("no evidence home staged from a database that exists")
	}

	snapshot := config.PathsForHome(evidenceHome)
	if _, err := os.Stat(snapshot.Database); err != nil {
		t.Fatalf("snapshot database missing: %v", err)
	}
	// A snapshot carries no WAL sidecars, which is the point: nothing has to
	// be created to read it.
	for _, sidecar := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(snapshot.Database + sidecar); err == nil {
			t.Errorf("snapshot shipped a %s sidecar; the open would need to write it", sidecar)
		}
	}

	store, err := db.OpenReadOnly(snapshot.Database)
	if err != nil {
		t.Fatalf("open the snapshot READ-ONLY: %v", err)
	}
	defer func() { _ = store.Close() }()
	jobs, err := store.ListJobs(context.Background())
	if err != nil {
		t.Fatalf("enumerate prior verdicts: %v", err)
	}
	found := false
	for _, job := range jobs {
		if job.ID == "prior-verdict" && strings.Contains(job.Payload, "must be able to enumerate") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the seeded verdict is not readable from the snapshot; %d jobs found", len(jobs))
	}
}

// TestReadOnlyGrantsStageEvidenceThroughProduction drives the real grant
// builder rather than the helper, and asserts the live store is NOT granted -
// the credential-adjacency property, now held by construction because nothing
// beside the live database is reachable when the database itself is not.
func TestReadOnlyGrantsStageEvidenceThroughProduction(t *testing.T) {
	home := t.TempDir()
	live := config.PathsForHome(home)
	seedVerdictStore(t, live, "prior verdict via production")

	checkout := t.TempDir()
	runGit(t, checkout, "init", "-b", "main")
	runGit(t, checkout, "config", "user.email", "gitmoot@example.com")
	runGit(t, checkout, "config", "user.name", "Gitmoot")
	runGit(t, checkout, "commit", "--allow-empty", "-m", "init")

	agent := runtime.Agent{Name: "seat", Runtime: runtime.CodexRuntime, ReadOnlySeat: true}
	grants, err := readOnlyRuntimeSandboxGrants(home, agent, checkout, true)
	if err != nil {
		t.Fatalf("readOnlyRuntimeSandboxGrants: %v", err)
	}
	if grants.evidenceHome == "" {
		t.Fatal("production staged no evidence home: a seat cannot enumerate prior verdicts, the #1839 defect")
	}
	if !strings.HasPrefix(grants.evidenceHome, grants.cacheRoot) {
		t.Errorf("evidence home %q is outside the seat's own cache root %q, so it would need a grant of its own", grants.evidenceHome, grants.cacheRoot)
	}
	if want := EvidenceHomeEnv + "=" + grants.evidenceHome; !containsString(grants.env, want) {
		t.Errorf("env %v does not export %q, so nothing can select the snapshot", grants.env, want)
	}
	for _, granted := range grants.readFiles {
		if granted == live.Database || strings.HasPrefix(granted, live.Home+string(filepath.Separator)) {
			t.Errorf("the LIVE store or a file beside it is granted (%q); a snapshot exists so nothing there needs to be", granted)
		}
	}
	for _, root := range grants.reads {
		if root == live.Home {
			t.Errorf("the gitmoot home %q is granted as a read root, exposing every credential beside the store", root)
		}
	}
	// And the staged copy is queryable, so this is sufficiency and not just
	// wiring.
	store, err := db.OpenReadOnly(config.PathsForHome(grants.evidenceHome).Database)
	if err != nil {
		t.Fatalf("open the production-staged snapshot: %v", err)
	}
	defer func() { _ = store.Close() }()
	if jobs, err := store.ListJobs(context.Background()); err != nil || len(jobs) == 0 {
		t.Fatalf("production snapshot holds no verdicts (jobs=%d err=%v)", len(jobs), err)
	}
}

// TestEvidenceHomeNeverRedirectsWrites is the guard on the selection
// mechanism, and it is why GITMOOT_EVIDENCE_HOME is honoured by INSPECTION
// commands only.
//
// A blanket fallback for --home was considered and rejected: `job record`
// would then land in a throwaway snapshot copy and vanish, which is a worse
// failure than not reading evidence - it looks like success. So a write must
// reach the LIVE store even with the variable set.
func TestEvidenceHomeNeverRedirectsWrites(t *testing.T) {
	home := t.TempDir()
	live := config.PathsForHome(home)
	seedVerdictStore(t, live, "live store")

	cacheRoot := t.TempDir()
	evidenceHome, diagnostic := stageReviewEvidenceSnapshot(context.Background(), live, cacheRoot)
	if evidenceHome == "" {
		t.Fatalf("no snapshot staged (%s)", diagnostic)
	}
	t.Setenv(EvidenceHomeEnv, evidenceHome)

	var stdout, stderr strings.Builder
	if code := Run([]string{"repo", "add", "gitmoot/gitmoot", "--home", home, "--force"}, &stdout, &stderr); code != 0 {
		t.Fatalf("repo add exited %d: %s%s", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()

	// A WRITE command while the evidence variable is set.
	if code := Run([]string{"job", "record", "--agent", "gm-omp-gate", "--repo", "gitmoot/gitmoot",
		"--type", "implement", "--decision", "implemented", "--pr", "1845", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("job record exited %d: %s%s", code, stdout.String(), stderr.String())
	}

	liveStore, err := db.OpenReadOnly(live.Database)
	if err != nil {
		t.Fatalf("open live store: %v", err)
	}
	defer func() { _ = liveStore.Close() }()
	liveJobs, err := liveStore.ListJobs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	snapStore, err := db.OpenReadOnly(config.PathsForHome(evidenceHome).Database)
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	defer func() { _ = snapStore.Close() }()
	snapJobs, err := snapStore.ListJobs(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	countImplement := func(jobs []db.Job) int {
		n := 0
		for _, job := range jobs {
			if job.Type == "implement" {
				n++
			}
		}
		return n
	}
	if countImplement(liveJobs) != 1 {
		t.Errorf("the LIVE store holds %d implement rows, want 1: the write did not land where it must", countImplement(liveJobs))
	}
	if got := countImplement(snapJobs); got != 0 {
		t.Errorf("the SNAPSHOT holds %d implement rows, want 0: a write was redirected into a throwaway copy", got)
	}
}

// TestInspectionCommandsReadTheSnapshot proves the selection mechanism has a
// real CONSUMER: the production `job list` code path, with no --home, reads the
// staged snapshot rather than the operator's live store.
//
// Without this the whole snapshot would be dead wiring - staged, exported, and
// never opened by anything.
func TestInspectionCommandsReadTheSnapshot(t *testing.T) {
	live := config.PathsForHome(t.TempDir())
	seedVerdictStore(t, live, "a verdict that exists ONLY in the snapshot")

	cacheRoot := t.TempDir()
	evidenceHome, diagnostic := stageReviewEvidenceSnapshot(context.Background(), live, cacheRoot)
	if evidenceHome == "" {
		t.Fatalf("no snapshot staged (%s)", diagnostic)
	}

	// Point the DEFAULT home somewhere empty, so reading the seeded verdict
	// can only come from the snapshot.
	t.Setenv("HOME", t.TempDir())
	t.Setenv(EvidenceHomeEnv, evidenceHome)

	var stdout, stderr strings.Builder
	if code := Run([]string{"job", "list"}, &stdout, &stderr); code != 0 {
		t.Fatalf("job list exited %d: %s%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "prior-verdict") {
		t.Fatalf("job list did not read the snapshot; output=%q", stdout.String())
	}

	// And an explicit --home still wins, so the variable can never override a
	// caller who named a home.
	stdout.Reset()
	stderr.Reset()
	empty := t.TempDir()
	if code := Run([]string{"job", "list", "--home", empty}, &stdout, &stderr); code != 0 {
		t.Fatalf("job list --home exited %d: %s%s", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), "prior-verdict") {
		t.Fatalf("an explicit --home was overridden by %s; output=%q", EvidenceHomeEnv, stdout.String())
	}
}

func TestWriteCommandsNeverConsultTheEvidenceSnapshot(t *testing.T) {
	// The guard that catches the mistake I actually made: wiring a WRITE
	// command to the inspection store. A mutant pointing `job answer` at the
	// snapshot SURVIVED every other test here, because nothing exercised a
	// write command with the variable set. The discriminator needs no
	// interactivity: the job exists ONLY in the snapshot, so a command that
	// finds it has consulted the snapshot, and one that reports it missing has
	// correctly read the live store.
	live := config.PathsForHome(t.TempDir())
	seedVerdictStore(t, live, "verdict")

	cacheRoot := t.TempDir()
	evidenceHome, diagnostic := stageReviewEvidenceSnapshot(context.Background(), live, cacheRoot)
	if evidenceHome == "" {
		t.Fatalf("no snapshot staged (%s)", diagnostic)
	}
	snapshotOnly := config.PathsForHome(evidenceHome)
	store, err := db.Open(snapshotOnly.Database)
	if err != nil {
		t.Fatalf("open snapshot writable: %v", err)
	}
	if err := store.CreateJobWithEvent(context.Background(), db.Job{
		ID: "snapshot-only-job", Agent: "gm-review-opus", Type: "ask",
		State: string(workflow.JobQueued), Payload: `{"repo":"gitmoot/gitmoot"}`,
	}, db.JobEvent{Kind: string(workflow.JobQueued), Message: "queued"}); err != nil {
		t.Fatalf("seed snapshot-only job: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOME", t.TempDir())
	t.Setenv(EvidenceHomeEnv, evidenceHome)

	// `job cancel` is the right probe: its outcome DIVERGES on which store it
	// read. `job answer` was tried first and could not discriminate - it exits
	// nonzero either way, because a job with no pending escalation fails for a
	// second reason, so the mutant survived a green test.
	var stdout, stderr strings.Builder
	code := Run([]string{"job", "cancel", "snapshot-only-job"}, &stdout, &stderr)
	combined := stdout.String() + stderr.String()
	if code == 0 {
		t.Fatalf("a WRITE command MUTATED a job that exists only in the snapshot; output=%q", combined)
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"job", "show", "snapshot-only-job"}, &stdout, &stderr); code != 0 {
		t.Fatalf("job show could not read the snapshot-only job, so this test's control is broken: %s%s", stdout.String(), stderr.String())
	}
}

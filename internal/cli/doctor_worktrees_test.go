package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestInspectDelegationWorktreeUsageClassifiesOwnersAndSize(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	store := openCLIJobStore(t, home)
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	root := filepath.Join(paths.Home, "worktrees", "owner--repo", "delegations", "parent")
	type item struct {
		id    string
		state workflow.JobState
		age   time.Duration
		size  int
	}
	items := []item{
		{id: "old-final", state: workflow.JobFailed, age: 73 * time.Hour, size: 7},
		{id: "fresh-final", state: workflow.JobSucceeded, age: time.Hour, size: 11},
		{id: "blocked", state: workflow.JobBlocked, age: 30 * 24 * time.Hour, size: 13},
	}
	for _, item := range items {
		path := filepath.Join(root, item.id)
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "payload.bin"), make([]byte, item.size), 0o600); err != nil {
			t.Fatal(err)
		}
		seedCLIJob(t, store, db.Job{
			ID: item.id, Agent: "reader", Type: "ask", State: string(item.state),
			ParentJobID: "parent", DelegationID: item.id,
			Payload: mustJobPayload(t, workflow.JobPayload{Repo: "owner/repo", DelegationID: item.id, WorktreePath: path}),
		}, string(item.state))
	}
	unproven := filepath.Join(root, "unproven")
	if err := os.MkdirAll(unproven, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unproven, "payload.bin"), make([]byte, 17), 0o600); err != nil {
		t.Fatal(err)
	}
	store.Close()
	const layout = "2006-01-02 15:04:05"
	for _, item := range items {
		at := now.Add(-item.age).Format(layout)
		setJobTimes(t, home, item.id, at, at)
	}
	store = openCLIJobStore(t, home)
	defer store.Close()

	usage, err := inspectDelegationWorktreeUsage(context.Background(), paths, store, now, 72*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if usage.Stale != 3 || usage.Reclaimable != 1 || usage.Pinned != 1 || usage.Unproven != 1 || usage.RecentTerminal != 1 {
		t.Fatalf("usage = %+v", usage)
	}
	if usage.SizeBytes != 37 { // 7 old + 13 blocked + 17 unproven; fresh excluded
		t.Fatalf("size = %d, want 37", usage.SizeBytes)
	}
	if !strings.Contains(usage.Summary, "3 stale worktrees / 37 B") {
		t.Fatalf("summary = %q", usage.Summary)
	}
}

func TestBuildDelegationWorktreeDoctorCheckThresholds(t *testing.T) {
	ok := buildDelegationWorktreeDoctorCheck(delegationWorktreeUsage{Stale: 1, Pinned: 1, Size: "10 B", Summary: "1 stale worktree / 10 B under /tmp/home/worktrees"})
	if !ok.OK || ok.Required || !strings.Contains(ok.Detail, "1 pinned") {
		t.Fatalf("below-threshold check = %+v", ok)
	}
	warn := buildDelegationWorktreeDoctorCheck(delegationWorktreeUsage{Stale: delegationWorktreeWarnCount, Reclaimable: delegationWorktreeWarnCount, Size: "2.0 GB", SizeBytes: 2_000_000_000, Summary: "10 stale worktrees / 2.0 GB under /tmp/home/worktrees"})
	if warn.OK || warn.Required || !strings.Contains(warn.Detail, "10 stale worktrees / 2.0 GB") {
		t.Fatalf("warning check = %+v", warn)
	}
	quarantined := buildDelegationWorktreeDoctorCheck(delegationWorktreeUsage{Quarantined: 1, Summary: "0 stale worktrees / 0 B under /tmp/home/worktrees"})
	if quarantined.OK || !strings.Contains(quarantined.Detail, "1 cleanup quarantined") {
		t.Fatalf("quarantined check = %+v", quarantined)
	}
}

// Doctor must report both a managed fix clone and every clone moved aside after
// an interrupted allocation/dispatch. These are durable operator work items:
// neither has another automatic deletion path.
func TestInspectDelegationWorktreeUsageAccountsManagedAndSetAsideFixClones(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	store := openCLIJobStore(t, home)
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	clone, err := workflow.FixWorktreePath(paths.Home, "owner/repo", "fix-job")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(clone), 0o755); err != nil {
		t.Fatal(err)
	}
	seedCLIJob(t, store, db.Job{
		ID: "fix-job", Agent: "fixer", Type: "implement", State: string(workflow.JobSucceeded),
		Payload: mustJobPayload(t, workflow.JobPayload{Repo: "owner/repo", WorktreePath: clone, FixWorktree: true}),
	}, string(workflow.JobSucceeded))
	if err := os.MkdirAll(clone, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clone, "managed.bin"), make([]byte, 7), 0o600); err != nil {
		t.Fatal(err)
	}
	setAsideClone, err := workflow.FixWorktreePath(paths.Home, "owner/repo", "fix-set-aside")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(setAsideClone, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(setAsideClone, "set-aside.bin"), make([]byte, 11), 0o600); err != nil {
		t.Fatal(err)
	}
	setAside, err := workflow.SetAsideFixClone(setAsideClone)
	if err != nil {
		t.Fatal(err)
	}
	seedCLIJob(t, store, db.Job{
		ID: "fix-set-aside", Agent: "fixer", Type: "implement", State: string(workflow.JobSucceeded),
		Payload: mustJobPayload(t, workflow.JobPayload{Repo: "owner/repo", WorktreePath: setAsideClone, FixWorktree: true}),
	}, string(workflow.JobSucceeded))
	if setAside == "" {
		t.Fatal("SetAsideFixClone returned no survivor")
	}

	// An interrupted removal's surviving clone, sized so it lands in the byte total.
	survivor := clone + ".ttl-reclaiming-aaaaaaaa"
	if err := os.MkdirAll(survivor, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(survivor, "payload.bin"), make([]byte, 23), 0o600); err != nil {
		t.Fatal(err)
	}
	// Anything else at a quarantine name is unowned content, whatever its shape:
	// with automatic removal disabled nothing here is ever deleted, so doctor's job
	// is to make it visible rather than to classify it as disposable.
	if err := os.WriteFile(clone+".ttl-reclaiming-bbbbbbbb", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// Age the owner past the TTL so its surviving quarantine is stale reclaimable
	// state rather than a recent terminal the size total excludes by design.
	store.Close()
	aged := now.Add(-96 * time.Hour).Format("2006-01-02 15:04:05")
	setJobTimes(t, home, "fix-job", aged, aged)
	setJobTimes(t, home, "fix-set-aside", aged, aged)
	store = openCLIJobStore(t, home)
	defer store.Close()

	usage, err := inspectDelegationWorktreeUsage(context.Background(), paths, store, now, 72*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// Managed clone (7 B), set-aside clone (11 B), and legacy survivor (23 B)
	// are operator work. The two unowned survivors and planted file are unproven.
	if usage.Stale != 4 {
		t.Fatalf("stale = %d, want three clones plus the unproven plant: %+v", usage.Stale, usage)
	}
	if usage.Unproven != 3 {
		t.Fatalf("unproven = %d, want two unowned survivors plus the writer plant: %+v", usage.Unproven, usage)
	}
	if usage.SizeBytes != 41 {
		t.Fatalf("size = %d, want all three fix-clone directories (41 B)", usage.SizeBytes)
	}
}

func TestInspectDelegationWorktreeUsageBoundsLogicalSizeScan(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	store := openCLIJobStore(t, home)
	defer store.Close()
	clone, err := workflow.FixWorktreePath(paths.Home, "owner/repo", "fix-large")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(clone, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < delegationWorktreeSizeEntryLimit; i++ {
		if err := os.WriteFile(filepath.Join(clone, fmt.Sprintf("entry-%04d", i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	seedCLIJob(t, store, db.Job{
		ID: "fix-large", Agent: "fixer", Type: "implement", State: string(workflow.JobBlocked),
		Payload: mustJobPayload(t, workflow.JobPayload{Repo: "owner/repo", WorktreePath: clone, FixWorktree: true}),
	}, string(workflow.JobBlocked))

	usage, err := inspectDelegationWorktreeUsage(context.Background(), paths, store, time.Now().UTC(), 72*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !usage.Truncated || !strings.Contains(usage.Summary, "lower bounds") {
		t.Fatalf("usage = %+v, want explicit bounded-scan truncation", usage)
	}
	if check := buildDelegationWorktreeDoctorCheck(usage); check.OK {
		t.Fatalf("doctor check = %+v, want a truncated scan to warn", check)
	}
}

type recordingWorktreeDirectoryReader struct {
	entry        fs.DirEntry
	remaining    int
	maxRequested int
}

func (r *recordingWorktreeDirectoryReader) ReadDir(n int) ([]fs.DirEntry, error) {
	if n <= 0 {
		return nil, fmt.Errorf("unbounded ReadDir request: %d", n)
	}
	r.maxRequested = max(r.maxRequested, n)
	count := min(n, r.remaining)
	if count == 0 {
		return nil, io.EOF
	}
	entries := make([]fs.DirEntry, count)
	for i := range entries {
		entries[i] = r.entry
	}
	r.remaining -= count
	return entries, nil
}

func TestReadWorktreeDirectoryBatchesNeverEagerlyReadsWholeDirectory(t *testing.T) {
	fixture := t.TempDir()
	if err := os.WriteFile(filepath.Join(fixture, "entry"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(fixture)
	if err != nil {
		t.Fatal(err)
	}
	reader := &recordingWorktreeDirectoryReader{
		entry:     entries[0],
		remaining: delegationWorktreeSizeEntryLimit + 1,
	}
	remaining := delegationWorktreeSizeEntryLimit
	visited := 0
	truncated, stopped, err := readWorktreeDirectoryBatches(context.Background(), reader, &remaining, func(fs.DirEntry) bool {
		visited++
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	if !truncated || stopped {
		t.Fatalf("truncated = %v, stopped = %v, want hard-budget truncation", truncated, stopped)
	}
	if visited != delegationWorktreeSizeEntryLimit {
		t.Fatalf("visited = %d, want exactly %d budgeted entries", visited, delegationWorktreeSizeEntryLimit)
	}
	if reader.maxRequested > delegationWorktreeReadBatchSize {
		t.Fatalf("largest ReadDir request = %d, want at most %d", reader.maxRequested, delegationWorktreeReadBatchSize)
	}
}

func TestHealthEndpointSurfacesDelegationWorktreeUsage(t *testing.T) {
	home := dashboardTestHome(t)
	paths := config.PathsForHome(home)
	path := filepath.Join(paths.Home, "worktrees", "owner--repo", "delegations", "parent", "pinned")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "payload.bin"), []byte("dashboard"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := openCLIJobStore(t, home)
	seedCLIJob(t, store, db.Job{
		ID: "pinned", Agent: "reader", Type: "ask", State: string(workflow.JobBlocked),
		ParentJobID: "parent", DelegationID: "pinned",
		Payload: mustJobPayload(t, workflow.JobPayload{Repo: "owner/repo", DelegationID: "pinned", WorktreePath: path}),
	}, string(workflow.JobBlocked))
	fixClone, err := workflow.FixWorktreePath(paths.Home, "owner/repo", "fix-api")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(fixClone, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixClone, "api.bin"), []byte("set-aside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := workflow.SetAsideFixClone(fixClone); err != nil {
		t.Fatal(err)
	}
	// Deliberately do not create a job row. Interrupted pre-enqueue recovery has
	// exactly this shape, so visibility must come from the fixes directory itself.
	for attempt := 0; attempt < delegationCleanupRetryBudget; attempt++ {
		if _, err := store.RecordCleanupObligationFailure(context.Background(), "quarantined", filepath.Join(paths.Home, "worktrees", "owner--repo", "delegations", "parent", "quarantined"), db.CleanupReasonUnknown, errors.New("stuck"), time.Now().UTC(), time.Now().UTC().Add(time.Minute), delegationCleanupRetryBudget); err != nil {
			t.Fatal(err)
		}
	}
	store.Close()

	stubOnDiskBuild(t, "", "")
	stubUpdateCheck(t, "")
	recorder := httptest.NewRecorder()
	(&webDataSource{home: home}).handleHealth(recorder, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("health status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Worktrees delegationWorktreeUsage `json:"worktrees"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Worktrees.Stale != 2 || payload.Worktrees.Pinned != 1 || payload.Worktrees.Unproven != 1 ||
		payload.Worktrees.Quarantined != 1 || payload.Worktrees.SizeBytes != int64(len("dashboard")+len("set-aside")) {
		t.Fatalf("worktrees = %+v", payload.Worktrees)
	}
	if !strings.Contains(payload.Worktrees.Summary, "2 stale worktrees") {
		t.Fatalf("summary = %q", payload.Worktrees.Summary)
	}
}

package cli

import (
	"context"
	"fmt"
	"io"
	"io/fs"
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

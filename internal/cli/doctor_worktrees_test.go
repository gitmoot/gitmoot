package cli

import (
	"context"
	"encoding/json"
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
	if payload.Worktrees.Stale != 1 || payload.Worktrees.Pinned != 1 || payload.Worktrees.SizeBytes != int64(len("dashboard")) {
		t.Fatalf("worktrees = %+v", payload.Worktrees)
	}
	if !strings.Contains(payload.Worktrees.Summary, "1 stale worktree") {
		t.Fatalf("summary = %q", payload.Worktrees.Summary)
	}
}

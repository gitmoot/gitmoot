package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// TestRunListSnapshotsProcOncePerListing pins #2702: run list used to scan
// all of /proc once per settled job with a worktree, so a large backlog
// printed ~90 rows/s. One snapshot must serve every row, in text and JSON.
func TestRunListSnapshotsProcOncePerListing(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()
	const settled = 25
	for i := range settled {
		seedCLIJob(t, store, db.Job{
			ID:      fmt.Sprintf("settled-%02d", i),
			Agent:   "worker",
			Type:    "ask",
			State:   string(workflow.JobSucceeded),
			Payload: mustJobPayload(t, workflow.JobPayload{Repo: "owner/repo", WorktreePath: fmt.Sprintf("/tmp/wt-%02d", i)}),
		}, "succeeded")
	}

	snapshots, probes := 0, 0
	previous := jobWorktreeLivenessSnapshot
	jobWorktreeLivenessSnapshot = func() worktreeLivenessProbe {
		snapshots++
		return func(path string) (bool, bool) {
			probes++
			return path == "/tmp/wt-07", true
		}
	}
	t.Cleanup(func() { jobWorktreeLivenessSnapshot = previous })

	for _, args := range [][]string{{}, {"--json"}} {
		snapshots, probes = 0, 0
		var stdout, stderr bytes.Buffer
		argv := append([]string{"run", "list", "--home", home}, args...)
		if code := Run(argv, &stdout, &stderr); code != 0 {
			t.Fatalf("%v exit = %d, stderr=%s", argv, code, stderr.String())
		}
		if snapshots != 1 {
			t.Fatalf("%v took %d /proc snapshots for %d settled jobs, want 1", argv, snapshots, settled)
		}
		if probes != settled {
			t.Fatalf("%v probed %d paths, want %d", argv, probes, settled)
		}
		if got := strings.Count(stdout.String(), "settled-07"); got != 1 {
			t.Fatalf("%v output lists settled-07 %d times:\n%s", argv, got, stdout.String())
		}
		if !strings.Contains(stdout.String(), "LIVE PROCESS") && !strings.Contains(stdout.String(), `"process_active": true`) {
			t.Fatalf("%v lost the live badge:\n%s", argv, stdout.String())
		}
	}

	// Lazy: a listing whose rows never need the probe never reads /proc.
	snapshots = 0
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"run", "list", "--home", home, "--state", "running"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run list --state running exit = %d, stderr=%s", code, stderr.String())
	}
	if snapshots != 0 {
		t.Fatalf("listing with no settled worktree rows took %d snapshots, want 0", snapshots)
	}
}

func TestRunListPullRequestAndLimitFilters(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()
	ctx := context.Background()
	// Ids sort opposite to creation time, so "newest" can't be the id order.
	jobs := []struct {
		id, repo, typ, created string
		pr                     int
	}{
		{"a-newest", "owner/repo", "review", "2026-11-04 00:00:00", 7},
		{"b-third", "owner/repo", "review", "2026-11-03 00:00:00", 7},
		{"c-second", "other/repo", "review", "2026-11-02 00:00:00", 7},
		{"d-oldest", "owner/repo", "review", "2026-11-01 00:00:00", 7},
		{"e-other-pr", "owner/repo", "review", "2026-11-05 00:00:00", 8},
		{"f-ask", "owner/repo", "ask", "2026-11-06 00:00:00", 7},
	}
	for _, item := range jobs {
		seedCLIJob(t, store, db.Job{
			ID:      item.id,
			Agent:   "worker",
			Type:    item.typ,
			State:   string(workflow.JobFailed),
			Payload: mustJobPayload(t, workflow.JobPayload{Repo: item.repo, PullRequest: item.pr}),
		}, "failed")
		if err := store.ExecForTest(ctx, `UPDATE jobs SET created_at = ? WHERE id = ?`, item.created, item.id); err != nil {
			t.Fatalf("set created_at: %v", err)
		}
	}

	list := func(args ...string) []string {
		t.Helper()
		argv := append(append([]string{}, args...), "--home", home, "--json")
		var stdout, stderr bytes.Buffer
		if code := Run(argv, &stdout, &stderr); code != 0 {
			t.Fatalf("%v exit = %d, stderr=%s", argv, code, stderr.String())
		}
		var entries []jobListEntry
		if err := json.Unmarshal(stdout.Bytes(), &entries); err != nil {
			t.Fatalf("decode %v: %v\n%s", argv, err, stdout.String())
		}
		ids := make([]string, 0, len(entries))
		for _, entry := range entries {
			ids = append(ids, entry.ID)
		}
		return ids
	}
	cases := []struct {
		args []string
		want []string
	}{
		{[]string{"run", "list", "--pr", "7"}, []string{"a-newest", "b-third", "c-second", "d-oldest", "f-ask"}},
		{[]string{"run", "list", "--pr", "7", "--repo", "owner/repo"}, []string{"a-newest", "b-third", "d-oldest", "f-ask"}},
		{[]string{"review", "list", "--pr", "7", "--repo", "owner/repo"}, []string{"a-newest", "b-third", "d-oldest"}},
		{[]string{"review", "list", "--pr", "7", "--limit", "2"}, []string{"a-newest", "b-third"}},
		{[]string{"run", "list", "--limit", "3"}, []string{"a-newest", "e-other-pr", "f-ask"}},
		{[]string{"run", "list", "--limit", "0"}, []string{"a-newest", "b-third", "c-second", "d-oldest", "e-other-pr", "f-ask"}},
		{[]string{"run", "list", "--limit", "100"}, []string{"a-newest", "b-third", "c-second", "d-oldest", "e-other-pr", "f-ask"}},
	}
	for _, tc := range cases {
		if got := list(tc.args...); !slices.Equal(got, tc.want) {
			t.Fatalf("%v = %v, want %v", tc.args, got, tc.want)
		}
	}

	for _, bad := range [][]string{
		{"run", "list", "--pr", "0"},
		{"run", "list", "--pr", "-3"},
		{"run", "list", "--limit", "-1"},
		{"review", "list", "--pr", "0"},
		{"review", "list", "--limit", "-5"},
		{"run", "list", "--limit", "x"},
	} {
		var stdout, stderr bytes.Buffer
		if code := Run(append(bad, "--home", home), &stdout, &stderr); code != 2 {
			t.Fatalf("%v exit = %d, want 2 (stderr=%s)", bad, code, stderr.String())
		}
		if stdout.Len() != 0 {
			t.Fatalf("%v printed rows on rejection: %s", bad, stdout.String())
		}
	}
}

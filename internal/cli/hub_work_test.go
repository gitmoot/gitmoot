package cli

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/github"
)

// hubWorkClient registers a machine for role and returns a client for it.
func hubWorkClient(t *testing.T, home, role string) (*http.Client, string) {
	t.Helper()
	certs := filepath.Join(t.TempDir(), "m")
	if code, _, diag := hubCLI(t, home, "machine", "add", "m", "--role", role, "--out", certs); code != 0 {
		t.Fatalf("machine add exit=%d: %s", code, diag)
	}
	return hubTestClient(t, certs, "m"), hubTestServer(t, home)
}

func TestHubOrgBriefIsTheCallersRole(t *testing.T) {
	home := messageTestHome(t)
	client, base := hubWorkClient(t, home, "gm-omp-impl")
	status, body := hubCall(t, client, http.MethodGet, base+"/v1/org/brief", "", "")
	if status != http.StatusOK || !strings.Contains(body, `"gm-omp-impl"`) {
		t.Fatalf("brief %d %s", status, body)
	}
}

// Every work endpoint refuses a repository outside the caller's org scope
// before it touches GitHub or the job store.
func TestHubWorkEndpointsRefuseReposOutsideTheRolesScope(t *testing.T) {
	home := messageTestHome(t)
	gh := &reviewLevelFakeGitHub{files: []github.PullRequestFile{{Filename: "README.md", Patch: "+x"}}}
	judge := &reviewLevelCountingJudge{}
	stubReviewLevel(t, gh, judge)
	// gm-omp-impl's scope is gitmoot/implementation (messageTestHome).
	client, base := hubWorkClient(t, home, "gm-omp-impl")
	head := strings.Repeat("a", 40)
	for _, call := range []struct{ method, path, body string }{
		{http.MethodGet, "/v1/tasks/7?repo=other/repo", ""},
		{http.MethodPost, "/v1/reviews/level", `{"repo":"other/repo","pr":7}`},
		{http.MethodPost, "/v1/reviews", `{"repo":"other/repo","pr":7,"head_sha":"` + head + `","message":"m"}`},
	} {
		status, body := hubCall(t, client, call.method, base+call.path, "", call.body)
		if status != http.StatusForbidden && !(status == http.StatusNotFound && call.method == http.MethodGet) {
			t.Errorf("%s %s: %d %s", call.method, call.path, status, body)
		}
	}
	if len(gh.statuses) != 0 {
		t.Fatalf("an out-of-scope level request posted a status: %+v", gh.statuses)
	}
}

func TestHubReviewLevelInScopePostsStatusAndRecordsTheWrite(t *testing.T) {
	home := messageTestHome(t)
	gh := &reviewLevelFakeGitHub{files: []github.PullRequestFile{{Filename: "README.md", Patch: "+x"}}}
	judge := &reviewLevelCountingJudge{}
	stubReviewLevel(t, gh, judge)
	client, base := hubWorkClient(t, home, "gm-omp-impl")
	status, body := hubCall(t, client, http.MethodPost, base+"/v1/reviews/level", "", `{"repo":"gitmoot/implementation","pr":7}`)
	if status != http.StatusOK || !strings.Contains(body, `"level"`) {
		t.Fatalf("level %d %s", status, body)
	}
	if len(gh.statuses) != 1 {
		t.Fatalf("statuses = %d, want 1", len(gh.statuses))
	}
	if n := hubWriteCount(t, home, "review.level"); n != 1 {
		t.Fatalf("review.level writes = %d, want 1", n)
	}
}

func TestHubReviewRequestRefusesAShortHead(t *testing.T) {
	home := messageTestHome(t)
	client, base := hubWorkClient(t, home, "gm-omp-impl")
	status, body := hubCall(t, client, http.MethodPost, base+"/v1/reviews", "",
		`{"repo":"gitmoot/implementation","pr":7,"head_sha":"479dcf53","message":"m"}`)
	if status != http.StatusBadRequest || !strings.Contains(body, "40-character") {
		t.Fatalf("short head: %d %s", status, body)
	}
	status, body = hubCall(t, client, http.MethodPost, base+"/v1/reviews", "",
		`{"repo":"gitmoot/implementation","pr":7,"head_sha":"`+strings.Repeat("a", 40)+`","evidence_manifest":"/etc/passwd"}`)
	if status != http.StatusBadRequest || !strings.Contains(body, "unknown field") {
		t.Fatalf("host-path field: %d %s", status, body)
	}
}

// A remote seat claims its own pending mail through the hub exactly as a local
// turn hook would: once, with the same receipt, and never another role's.
func TestHubNotificationClaimDeliversOwnMailOnceLikeTheLocalHook(t *testing.T) {
	home := messageTestHome(t)
	mine := sendTestMessage(t, home, "gm-omp-nag", "deimos", "Please inspect the failing build.")
	other := sendTestMessage(t, home, "gm-omp-nag", "jarvis", "Private note for jarvis only.")
	client, base := hubWorkClient(t, home, "deimos")

	claim := `{"runtime":"claude","event":"UserPromptSubmit"}`
	status, body := hubCall(t, client, http.MethodPost, base+"/v1/notifications/claim", "", claim)
	if status != http.StatusOK || !strings.Contains(body, fmt.Sprintf("message %d [message] from gm-omp-nag", mine)) {
		t.Fatalf("claim %d %s", status, body)
	}
	if strings.Contains(body, fmt.Sprint(other)) || strings.Contains(body, "Private note") {
		t.Fatalf("deimos's claim showed jarvis's mail: %s", body)
	}
	status, body = hubCall(t, client, http.MethodPost, base+"/v1/notifications/claim", "", claim)
	if status != http.StatusOK || !strings.Contains(body, `"hook_output": null`) {
		t.Fatalf("second claim re-delivered: %d %s", status, body)
	}
	if got := turnHookWakeReceipts(t, home, "deimos")[fmt.Sprint(mine)]; got != "delivered|turn-hook:claude:UserPromptSubmit" {
		t.Fatalf("receipt = %q", got)
	}
	if got := showTestMessage(t, home, "jarvis", other).Status; got != "queued" {
		t.Fatalf("another role's notification status = %q, want queued", got)
	}
	if status, _ := hubCall(t, client, http.MethodPost, base+"/v1/notifications/claim", "", `{"runtime":"bash","event":"Stop"}`); status != http.StatusBadRequest {
		t.Fatalf("unsupported runtime: %d", status)
	}
}

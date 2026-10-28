package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/runtime"
)

// largeReview is a valid changes_requested review well over the old 12 KB repair
// excerpt; extra, when set, adds one more top-level key (#2418).
func largeReview(t *testing.T, extra string) string {
	t.Helper()
	findings := make([]string, 0, 60)
	for i := 0; i < 60; i++ {
		findings = append(findings, fmt.Sprintf("finding %02d: %s", i, strings.Repeat("detail ", 40)))
	}
	encoded, err := json.Marshal(findings)
	if err != nil {
		t.Fatal(err)
	}
	if extra != "" {
		extra = "," + extra
	}
	return `{"gitmoot_result":{"decision":"changes_requested","severity":"P2","summary":"many findings",` +
		`"findings":` + string(encoded) + `,"changes_made":[],"tests_run":[],"needs":[],"delegations":[]` + extra + `}}`
}

// The usmle-app incident (message 366736): a finished review over 12 KB with one
// key outside the contract went to repair, and the repair prompt carried only the
// first 12,000 bytes, so the agent could not reproduce its findings and answered
// blocked. The contract still rejects the key, but the repair prompt now carries
// the whole previous output, so the corrected result keeps every finding.
func TestMailboxRepairPromptCarriesTheWholeLargeResult(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{store: store, resolveDeliveryWorktree: ExcludedDeliveryWorktreeResolver("test_explicit_no_worktree")}
	agent := runtime.Agent{Name: "audit", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "gitmoot/gitmoot", Role: "reviewer"}
	withExtraKey := largeReview(t, `"head_sha":"0123456789abcdef0123456789abcdef01234567"`)
	if len(withExtraKey) <= 12000 {
		t.Fatalf("fixture is %d bytes; it must exceed the old 12 KB excerpt", len(withExtraKey))
	}
	adapter := &fakeDelivery{outputs: []string{withExtraKey, largeReview(t, "")}}
	if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "job-big-repair", Agent: "audit", Action: "review", Repo: "gitmoot/gitmoot"}); err != nil {
		t.Fatal(err)
	}
	result, err := mailbox.Run(ctx, "job-big-repair", agent, adapter)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(adapter.prompts) != 2 || len(result.Findings) != 60 {
		t.Fatalf("deliveries = %d, findings = %d; want one repair and all 60 findings", len(adapter.prompts), len(result.Findings))
	}
	repair := adapter.prompts[1]
	if !strings.Contains(repair, `unsupported gitmoot_result field "head_sha"`) {
		t.Fatal("repair prompt does not name the rejected key")
	}
	if !strings.Contains(repair, strings.TrimSpace(withExtraKey)) || strings.Contains(repair, "[truncated]") {
		t.Fatal("repair prompt did not carry the complete previous output")
	}
}

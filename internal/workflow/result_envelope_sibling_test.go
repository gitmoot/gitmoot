package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/runtime"
)

// herdr513Envelope is the shape jerryfane/herdr#513's reviewer returned (job
// local-review-review-router-18eae2a3837d9713-1): it ran its checks and
// declared evidence=executed BESIDE the gitmoot_result object, where it was
// dropped and the verdict stored the static_only default.
const herdr513Envelope = `{"gitmoot_result":{"decision":"approved","summary":"signature, asset hashes and docs snapshot all match at this head","findings":[],"changes_made":[],"tests_run":["cargo test -p herdr update:: -> 129 passed","python3 -m unittest scripts.test_preview -> Ran 26 tests, OK"],"needs":[]},"evidence":"executed"}`

func TestExtractAgentResultRefusesAResultFieldBesideTheEnvelope(t *testing.T) {
	for name, tc := range map[string]struct{ output, want string }{
		"evidence beside, absent inside":    {herdr513Envelope, `gitmoot_result field "evidence" is beside the gitmoot_result object`},
		"severity beside, absent inside":    {`{"gitmoot_result":{"decision":"approved","summary":"ok at this head"},"severity":"P3"}`, `gitmoot_result field "severity" is beside`},
		"evidence beside, different inside": {`{"gitmoot_result":{"decision":"approved","summary":"ok at this head","evidence":"static_only"},"evidence":"executed"}`, `gitmoot_result field "evidence" is beside`},
		"two fields beside, named in order": {`{"severity":"P3","gitmoot_result":{"decision":"approved","summary":"ok at this head"},"evidence":"executed"}`, `gitmoot_result fields "evidence", "severity" are beside the gitmoot_result object`},
	} {
		t.Run(name, func(t *testing.T) {
			result, err := ExtractAgentResult(tc.output)
			if err == nil {
				t.Fatalf("ExtractAgentResult accepted %s as %+v; a result field beside gitmoot_result must be refused, not dropped", tc.output, result)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// A copy beside the object that repeats the inner value is refused as well:
// telling a true duplicate apart needs a lossless JSON comparison, and the
// repair turn it costs is cheaper than getting that comparison wrong.
func TestExtractAgentResultRefusesADuplicateBesideTheEnvelope(t *testing.T) {
	output := `{"gitmoot_result":{"decision":"approved","summary":"ran go test at this head","tests_run":["go test ./... -> ok"],"evidence":"executed"},"evidence":"executed"}`
	result, err := ExtractAgentResult(output)
	if err == nil {
		t.Fatalf("ExtractAgentResult accepted a duplicate evidence beside gitmoot_result as %+v", result)
	}
	if !strings.Contains(err.Error(), `gitmoot_result field "evidence" is beside the gitmoot_result object`) {
		t.Fatalf("error = %v, want it to name the duplicated field", err)
	}
}

func TestExtractAgentResultIgnoresNonResultEnvelopeSiblings(t *testing.T) {
	// A key that is not a result field was never part of the contract.
	if _, err := ExtractAgentResult(`{"gitmoot_result":{"decision":"approved","summary":"read the diff at this head"},"learnings":["x"]}`); err != nil {
		t.Fatalf("non-result sibling refused: %v", err)
	}
}

// The refusal routes through the existing repair re-ask, so the reviewer's own
// declaration lands in the stored result instead of the static_only default.
func TestMailboxRunRepairsEvidenceDeclaredBesideTheEnvelope(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{store: store}
	agent := runtime.Agent{Name: "audit", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "gitmoot/gitmoot", Role: "reviewer"}
	adapter := &fakeDelivery{outputs: []string{
		herdr513Envelope,
		`{"gitmoot_result":{"decision":"approved","summary":"signature, asset hashes and docs snapshot all match at this head","findings":[],"changes_made":[],"tests_run":["cargo test -p herdr update:: -> 129 passed","python3 -m unittest scripts.test_preview -> Ran 26 tests, OK"],"needs":[],"evidence":"executed"}}`,
	}}
	if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "job-review-evidence-sibling", Agent: "audit", Action: "review", Repo: "gitmoot/gitmoot"}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	result, err := mailbox.Run(ctx, "job-review-evidence-sibling", agent, adapter)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Evidence != EvidenceExecuted || !result.EvidenceDeclared {
		t.Fatalf("stored evidence = %q declared=%v, want the reviewer's executed declaration", result.Evidence, result.EvidenceDeclared)
	}
	if len(adapter.prompts) != 2 {
		t.Fatalf("deliveries = %d, want the first delivery plus one repair", len(adapter.prompts))
	}
	if !strings.Contains(adapter.prompts[1], `gitmoot_result field "evidence" is beside the gitmoot_result object`) {
		t.Fatalf("repair prompt does not name the misplaced field:\n%s", adapter.prompts[1])
	}
}

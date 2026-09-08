package workflow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/runtime"
)

// #1821. A staged review's preflight is decoration unless its finding CONSTRAINS
// the verdict stage. These tests are about the direction of that constraint,
// which is the only interesting thing about it: it moves evidence downward,
// never upward, and it cannot be laundered by adding a hop.

func TestInheritedEvidenceCeilingClampsAnExecutedClaim(t *testing.T) {
	result := AgentResult{Decision: "approved", Evidence: EvidenceExecuted, EvidenceDeclared: true}
	if !ApplyInheritedEvidenceCeiling(&result, EvidenceStaticOnly) {
		t.Fatal("an executed claim under a static_only preflight was not clamped; the preflight would be decoration")
	}
	if result.Evidence != EvidenceStaticOnly {
		t.Fatalf("evidence is %q after clamping, want %q", result.Evidence, EvidenceStaticOnly)
	}
	// #1817's flag must keep meaning "the producer spoke". An overruled producer
	// still spoke, and a consumer that cannot tell it apart from a silent one
	// loses the distinction that field was built for.
	if !EvidenceWasDeclared(result) {
		t.Fatal("clamping cleared evidence_declared; an overruled producer is not a silent one")
	}
	if EvidenceWasExecuted(result) {
		t.Fatal("EvidenceWasExecuted still reports true after a clamp; the gate would read the unclamped claim")
	}
}

// The ceiling must never manufacture a stronger claim than the row itself made.
// The child is the row whose verdict the gate consumes, so its own modesty wins.
func TestInheritedEvidenceCeilingNeverUpgrades(t *testing.T) {
	result := AgentResult{Decision: "approved", Evidence: EvidenceStaticOnly, EvidenceDeclared: true}
	if ApplyInheritedEvidenceCeiling(&result, EvidenceExecuted) {
		t.Fatal("a static_only child was reported as clamped under an executed preflight")
	}
	if result.Evidence != EvidenceStaticOnly {
		t.Fatalf("an executed preflight upgraded its child to %q; the ceiling only moves downward", result.Evidence)
	}
}

// This case exists because a mutation SURVIVED without it. Swapping the clamp
// for "executed becomes static_only, everything else becomes executed" left
// every other test here green, because TestInheritedEvidenceCeilingNeverUpgrades
// passes an EXECUTED ceiling and returns before the mutated branch.
//
// The uncovered shape is the most common one a staged review will produce: an
// honest static_only child under a static_only preflight. Upgrading it would
// manufacture an execution claim out of two admissions that nothing ran, which
// is worse than the gap this whole slice closes.
func TestInheritedEvidenceCeilingLeavesAnAlreadyStaticVerdictAlone(t *testing.T) {
	result := AgentResult{Decision: "changes_requested", Evidence: EvidenceStaticOnly, EvidenceDeclared: true}
	if ApplyInheritedEvidenceCeiling(&result, EvidenceStaticOnly) {
		t.Fatal("a static_only verdict under a static_only ceiling reported a clamp; there was nothing to clamp")
	}
	if result.Evidence != EvidenceStaticOnly {
		t.Fatalf("evidence became %q; two admissions that nothing ran cannot add up to an execution claim", result.Evidence)
	}
	if EvidenceWasExecuted(result) {
		t.Fatal("EvidenceWasExecuted reports true for a static_only verdict under a static_only ceiling")
	}
}

// Every job outside a staged review must be untouched, which is what makes this
// safe to land while nothing dispatches two stages yet.
func TestInheritedEvidenceCeilingIgnoresEveryNonStagedShape(t *testing.T) {
	for _, inherited := range []string{"", "   ", "executed", "nonsense", "STATIC_ONLY"} {
		result := AgentResult{Decision: "approved", Evidence: EvidenceExecuted, EvidenceDeclared: true}
		if ApplyInheritedEvidenceCeiling(&result, inherited) {
			t.Fatalf("inherited %q clamped an executed verdict; only a declared static_only ceiling may", inherited)
		}
		if result.Evidence != EvidenceExecuted {
			t.Fatalf("inherited %q altered evidence to %q", inherited, result.Evidence)
		}
	}
}

func TestInheritedEvidenceCeilingToleratesANilResult(t *testing.T) {
	if ApplyInheritedEvidenceCeiling(nil, EvidenceStaticOnly) {
		t.Fatal("a nil result reported a clamp")
	}
}

// The inheritance resolution, which is where a chain could cheat.
func TestDelegationInheritedEvidencePrefersTheParentsOwnFinding(t *testing.T) {
	payload := JobPayload{Result: &AgentResult{Evidence: EvidenceStaticOnly, EvidenceDeclared: true}}
	if got := delegationInheritedEvidence(payload); got != EvidenceStaticOnly {
		t.Fatalf("child inherited %q, want the parent's declared %q", got, EvidenceStaticOnly)
	}
}

// The laundering case, and the reason the fallback exists: without it a chain
// closes the constraint by inserting one silent hop between the preflight and
// the verdict. That is the same survives-exactly-one-hop defect #1277 fixed for
// the review-fanout intent.
func TestDelegationInheritedEvidenceSurvivesASilentHop(t *testing.T) {
	silentHop := JobPayload{InheritedEvidence: EvidenceStaticOnly}
	if got := delegationInheritedEvidence(silentHop); got != EvidenceStaticOnly {
		t.Fatalf("a hop that declared nothing dropped the ceiling (%q); the constraint would survive exactly one hop", got)
	}
	// And with a result that declared nothing, the same fallback must hold: an
	// undeclared result is silence, not permission.
	undeclared := JobPayload{
		InheritedEvidence: EvidenceStaticOnly,
		Result:            &AgentResult{Evidence: EvidenceExecuted},
	}
	if got := delegationInheritedEvidence(undeclared); got != EvidenceStaticOnly {
		t.Fatalf("an UNDECLARED executed result overrode the inherited ceiling (%q); silence is not permission", got)
	}
}

func TestDelegationInheritedEvidenceIsEmptyForAnOrdinaryParent(t *testing.T) {
	if got := delegationInheritedEvidence(JobPayload{}); got != "" {
		t.Fatalf("an ordinary parent imposed a ceiling of %q", got)
	}
	declaredExecuted := JobPayload{Result: &AgentResult{Evidence: EvidenceExecuted, EvidenceDeclared: true}}
	if got := delegationInheritedEvidence(declaredExecuted); got != EvidenceExecuted {
		t.Fatalf("a parent that executed passed down %q, want %q carried verbatim", got, EvidenceExecuted)
	}
}

// The payload must serialize byte-identically for every job that is not a staged
// review, or this field is a migration rather than an addition.
func TestInheritedEvidenceIsAbsentFromAnOrdinaryPayload(t *testing.T) {
	raw, err := json.Marshal(JobPayload{Repo: "owner/repo", Branch: "main"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "inherited_evidence") {
		t.Fatalf("an ordinary payload carries the field: %s", raw)
	}
	staged, err := json.Marshal(JobPayload{Repo: "owner/repo", InheritedEvidence: EvidenceStaticOnly})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(staged), `"inherited_evidence":"static_only"`) {
		t.Fatalf("a staged payload does not carry the ceiling: %s", staged)
	}
}

// An unrecognised ceiling must not be stored, so a future reader never has to
// interpret a value the validator would reject.
func TestNormalizeInheritedEvidenceDropsUnrecognisedValues(t *testing.T) {
	for _, in := range []string{"", "  ", "EXECUTED", "static-only", "partial", "no"} {
		if got := normalizeInheritedEvidence(in); got != "" {
			t.Fatalf("normalize(%q) = %q, want empty", in, got)
		}
	}
	if got := normalizeInheritedEvidence("  " + EvidenceStaticOnly + " "); got != EvidenceStaticOnly {
		t.Fatalf("normalize did not accept a padded declared value: %q", got)
	}
}

// The PRODUCTION-ENTRY test, and the only one here that proves the wiring
// rather than the arithmetic. The helpers above can all pass while nothing
// calls them, which is the shape that let a pool-seam mutation survive on my
// last slice.
//
// A real Mailbox.Run, a real store, an agent returning a real gitmoot_result
// that claims EXECUTED, under a payload carrying a static_only ceiling.
func TestMailboxRunClampsAVerdictToItsPreflightCeiling(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{store: store, resolveDeliveryWorktree: ExcludedDeliveryWorktreeResolver("test_explicit_no_worktree")}
	agent := runtime.Agent{Name: "verdict", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "owner/repo", Role: "reviewer"}
	adapter := &fakeDelivery{outputs: []string{
		`{"gitmoot_result":{"decision":"approved","summary":"looks fine","evidence":"executed",` +
			`"tests_run":["go test ./..."],"findings":[],"changes_made":[],"needs":[],"delegations":[]}}`,
	}}

	if _, err := mailbox.Enqueue(ctx, JobRequest{
		ID: "verdict-job", Agent: "verdict", Action: "review", Repo: "owner/repo",
		InheritedEvidence: EvidenceStaticOnly,
	}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	result, err := mailbox.Run(ctx, "verdict-job", agent, adapter)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	// The returned value.
	if result.Evidence != EvidenceStaticOnly {
		t.Fatalf("Run returned evidence %q; a verdict claiming execution under a static_only preflight was not clamped", result.Evidence)
	}
	// And the STORED value, which is what every later consumer and the merge gate
	// actually read. Asserting only the return would miss a clamp applied after
	// the payload was written.
	job, err := store.GetJob(ctx, "verdict-job")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	var stored JobPayload
	if err := json.Unmarshal([]byte(job.Payload), &stored); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if stored.Result == nil {
		t.Fatal("no stored result")
	}
	if stored.Result.Evidence != EvidenceStaticOnly {
		t.Fatalf("STORED evidence is %q, want %q", stored.Result.Evidence, EvidenceStaticOnly)
	}
	if EvidenceWasExecuted(*stored.Result) {
		t.Fatal("the stored result still reports executed evidence to the gate")
	}
	if !EvidenceWasDeclared(*stored.Result) {
		t.Fatal("the stored result lost evidence_declared; an overruled producer still spoke")
	}

	// The audit trail. A clamp that leaves no record makes an overruled producer
	// indistinguishable from an honest static_only one, which is the exact
	// distinction #1817 built that flag for.
	events, err := store.ListJobEvents(ctx, "verdict-job")
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	var clamped bool
	for _, e := range events {
		if e.Kind == InheritedEvidenceClampedEvent {
			clamped = true
		}
	}
	if !clamped {
		t.Fatalf("no %s event recorded for a clamped verdict", InheritedEvidenceClampedEvent)
	}
}

// The should-SUCCEED arm: an ordinary review with no ceiling keeps its executed
// claim and records no clamp. A guard that quietly downgrades honest evidence
// would be worse than the gap it closes.
func TestMailboxRunLeavesAnUnstagedVerdictAlone(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{store: store, resolveDeliveryWorktree: ExcludedDeliveryWorktreeResolver("test_explicit_no_worktree")}
	agent := runtime.Agent{Name: "verdict", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "owner/repo", Role: "reviewer"}
	adapter := &fakeDelivery{outputs: []string{
		`{"gitmoot_result":{"decision":"approved","summary":"ran it","evidence":"executed",` +
			`"tests_run":["go test ./..."],"findings":[],"changes_made":[],"needs":[],"delegations":[]}}`,
	}}
	if _, err := mailbox.Enqueue(ctx, JobRequest{
		ID: "plain-job", Agent: "verdict", Action: "review", Repo: "owner/repo",
	}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	result, err := mailbox.Run(ctx, "plain-job", agent, adapter)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Evidence != EvidenceExecuted {
		t.Fatalf("an unstaged verdict was downgraded to %q", result.Evidence)
	}
	events, err := store.ListJobEvents(ctx, "plain-job")
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	for _, e := range events {
		if e.Kind == InheritedEvidenceClampedEvent {
			t.Fatal("an unstaged verdict recorded a clamp event")
		}
	}
}

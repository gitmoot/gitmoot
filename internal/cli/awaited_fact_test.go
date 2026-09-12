package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/db/dbtest"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func insertAwaitedFactWakeForTest(t *testing.T, store *db.Store, payload db.AwaitedFactWakePayload) {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.InsertWakeOutbox(
		context.Background(), db.WakeOutboxSourceAwaitedFact, string(encoded),
		db.WakeOutboxKindFact, []string{payload.WaiterRole},
	); err != nil {
		t.Fatal(err)
	}
}

func TestAwaitedFactExpiryRemainsQueryableAndAddressesParent(t *testing.T) {
	root := t.TempDir()
	paths := config.PathsForHome(root)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(`[org.roles."owner"]
scope=["*"]
[org.roles."lane"]
parent="owner"
scope=["*"]
`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cfg, err := config.LoadOrg(paths)
	if err != nil {
		t.Fatalf("LoadOrg: %v", err)
	}
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	key, err := db.ReviewVerdictSubjectKey("acme/widget", 45, "head-expire")
	if err != nil {
		t.Fatalf("ReviewVerdictSubjectKey: %v", err)
	}
	deadline := time.Now().UTC().Add(time.Minute)
	fact, _, err := store.SubscribeAwaitedFact(context.Background(), db.AwaitedFactSubscription{
		WaiterRole: "lane", SubjectKind: db.AwaitedFactSubjectReviewVerdict,
		SubjectKey: key, Deadline: deadline,
	})
	if err != nil {
		t.Fatalf("SubscribeAwaitedFact: %v", err)
	}
	var log bytes.Buffer
	if err := evaluateAwaitedFactTTLs(context.Background(), store, cfg, &log, deadline.Add(time.Second), awaitedFactTTLDependencies{}); err != nil {
		t.Fatalf("evaluateAwaitedFactTTLs: %v", err)
	}
	expired, err := store.ListAwaitedFacts(context.Background(), "lane", db.AwaitedFactStateExpired)
	if err != nil {
		t.Fatalf("ListAwaitedFacts: %v", err)
	}
	if len(expired) != 1 || expired[0].ID != fact.ID || expired[0].ExpiredAt == "" {
		t.Fatalf("queryable expired facts = %+v, want terminal row %d", expired, fact.ID)
	}
	outbox, err := store.ListWakeOutbox(context.Background(), db.WakeOutboxStatePending)
	if err != nil {
		t.Fatalf("ListWakeOutbox: %v", err)
	}
	if len(outbox) != 1 || outbox[0].TargetRole != "owner" || outbox[0].CoalesceKey != "fact:owner" || !strings.Contains(outbox[0].SourceID, `"waiter_role":"lane"`) {
		t.Fatalf("expiry outbox = %+v, want exact parent delivery", outbox)
	}
}

func TestAwaitedFactOutboxIsAddressedDeliveryWithoutReceiptCeremony(t *testing.T) {
	payload := `{"id":7,"waiter_role":"lane","subject_kind":"review_verdict","subject_key":"acme/widget#46@head","state":"satisfied"}`
	batch := []db.WakeOutboxObligation{{
		ID: 1, SourceKind: db.WakeOutboxSourceAwaitedFact, SourceID: payload,
		TargetRole: "lane", CoalesceKey: "fact:lane", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}}
	event, err := wakeOutboxEvent(batch, time.Now().UTC())
	if err != nil {
		t.Fatalf("wakeOutboxEvent: %v", err)
	}
	matching := matchingWakeRules([]db.EventRule{
		{ID: "other", OnKind: "fact", WakeRole: "other", Scope: db.EventRuleScopeAddressed, Enabled: true},
		{ID: "lane", OnKind: "fact", WakeRole: "lane", Scope: db.EventRuleScopeAddressed, Enabled: true},
	}, event)
	if len(matching) != 1 || matching[0].ID != "lane" {
		t.Fatalf("matching fact rules = %+v, want exact lane rule", matching)
	}
	if event.JobID != "awaited-fact:7" || event.RootID != "awaited-fact:7" || strings.Contains(event.JobID, "{") || strings.Contains(event.RootID, "{") {
		t.Fatalf("fact job_id/root_id = %q/%q, want short stable fact ids without serialized payloads", event.JobID, event.RootID)
	}
	prompt := eventRuleWakePrompt("fact", event)
	if !strings.Contains(prompt, "awaited fact for lane") || strings.Contains(prompt, " ack ") || strings.Contains(prompt, " done ") {
		t.Fatalf("fact prompt = %q, want delivery-only prompt without receipt ceremony", prompt)
	}
}

func TestFailedReviewFactWakeRequiresRetryOrExplicitBlocker(t *testing.T) {
	payload := `{"id":8,"waiter_role":"lane","subject_kind":"review_verdict","subject_key":"acme/widget#46@head","state":"review_failed","job_id":"review-8","detail":"review job review-8 failed without an exact-head verdict"}`
	event, err := wakeOutboxEvent([]db.WakeOutboxObligation{{
		ID: 2, SourceKind: db.WakeOutboxSourceAwaitedFact, SourceID: payload,
		TargetRole: "lane", CoalesceKey: "fact:lane", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	prompt := eventRuleWakePrompt("fact", event)
	for _, want := range []string{"review-8 failed", "org await list --role lane --state waiting", "Retry the exact-head review", "record a blocker naming its owner and next trigger"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("failed-review prompt = %q, want %q", prompt, want)
		}
	}
}

func TestFactWakeBatchPrioritizesReviewFailureOverOlderCompletion(t *testing.T) {
	now := time.Now().UTC()
	event, err := wakeOutboxEvent([]db.WakeOutboxObligation{
		{
			ID: 2, SourceKind: db.WakeOutboxSourceAwaitedFact,
			SourceID:   `{"id":8,"waiter_role":"lane","subject_kind":"review_verdict","subject_key":"acme/widget#45@old","state":"satisfied"}`,
			TargetRole: "lane", CoalesceKey: "fact:lane", CreatedAt: now.Add(-time.Second).Format(time.RFC3339Nano),
		},
		{
			ID: 3, SourceKind: db.WakeOutboxSourceAwaitedFact,
			SourceID:   `{"id":9,"waiter_role":"lane","subject_kind":"review_verdict","subject_key":"acme/widget#46@head","state":"review_failed","job_id":"review-9","detail":"review job review-9 failed without an exact-head verdict"}`,
			TargetRole: "lane", CoalesceKey: "fact:lane", CreatedAt: now.Format(time.RFC3339Nano),
		},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if event.JobID != "awaited-fact:9" || event.Cause != "awaited_fact_review_failed" {
		t.Fatalf("fact event identity = %q cause=%q, want actionable failed fact", event.JobID, event.Cause)
	}
	prompt := eventRuleWakePrompt("fact", event)
	for _, want := range []string{"review-9 failed", "is satisfied", "Retry the exact-head review"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("coalesced fact prompt = %q, want %q", prompt, want)
		}
	}
}

func TestFactWakeBatchSuppressesFailureSupersededBySuccess(t *testing.T) {
	now := time.Now().UTC()
	event, err := wakeOutboxEvent([]db.WakeOutboxObligation{
		{
			ID: 2, SourceKind: db.WakeOutboxSourceAwaitedFact,
			SourceID:   `{"id":9,"waiter_role":"lane","subject_kind":"review_verdict","subject_key":"acme/widget#46@head","state":"review_failed","job_id":"review-9","lifecycle_generation":0,"detail":"review job review-9 failed without an exact-head verdict"}`,
			TargetRole: "lane", CoalesceKey: "fact:lane", CreatedAt: now.Add(-time.Second).Format(time.RFC3339Nano),
		},
		{
			ID: 3, SourceKind: db.WakeOutboxSourceAwaitedFact,
			SourceID:   `{"id":9,"waiter_role":"lane","subject_kind":"review_verdict","subject_key":"acme/widget#46@head","state":"satisfied"}`,
			TargetRole: "lane", CoalesceKey: "fact:lane", CreatedAt: now.Format(time.RFC3339Nano),
		},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if event.JobID != "awaited-fact:9" || event.Cause != "awaited_fact_satisfied" {
		t.Fatalf("fact event identity = %q cause=%q, want latest satisfied fact", event.JobID, event.Cause)
	}
	prompt := eventRuleWakePrompt("fact", event)
	if !strings.Contains(prompt, "is satisfied") || strings.Contains(prompt, "Retry the exact-head review") ||
		strings.Contains(prompt, "review-9 failed") {
		t.Fatalf("coalesced fact prompt = %q, want only the latest satisfied state", prompt)
	}
}

func TestFactWakeDrainReducesOneFactAcrossBatchLimit(t *testing.T) {
	store, sink, wake, _ := replyWakeTestHarness(t, []replyWakeTestRole{{"owner", "w1:p0"}, {"lane", "w1:p1"}})
	ctx := context.Background()
	if err := store.AddEventRule(ctx, db.EventRule{
		ID: "fact-lane", OnKind: db.WakeOutboxKindFact, WakeRole: "lane",
		Scope: db.EventRuleScopeAddressed, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	insertAwaitedFactWakeForTest(t, store, db.AwaitedFactWakePayload{
		ID: 9, WaiterRole: "lane", SubjectKind: db.AwaitedFactSubjectReviewVerdict,
		SubjectKey: "acme/widget#46@head", State: "review_failed", JobID: "review-9",
		Detail: "review job review-9 failed without an exact-head verdict",
	})
	for index := range replyWakeMaxCoalescedItems - 1 {
		insertAwaitedFactWakeForTest(t, store, db.AwaitedFactWakePayload{
			ID: int64(100 + index), WaiterRole: "lane", SubjectKind: db.AwaitedFactSubjectReviewVerdict,
			SubjectKey: fmt.Sprintf("acme/widget#%d@head", 100+index), State: db.AwaitedFactStateSatisfied,
		})
	}
	insertAwaitedFactWakeForTest(t, store, db.AwaitedFactWakePayload{
		ID: 9, WaiterRole: "lane", SubjectKind: db.AwaitedFactSubjectReviewVerdict,
		SubjectKey: "acme/widget#46@head", State: db.AwaitedFactStateSatisfied,
	})

	drainReplyWakeAfterAllRowsAreDue(t, store, sink)
	if wake.promptCalls != replyWakeMaxCoalescedItems {
		t.Fatalf("prompt calls = %d, want one per current fact (%d)", wake.promptCalls, replyWakeMaxCoalescedItems)
	}
	for _, prompt := range wake.prompts {
		if strings.Contains(prompt, "Retry the exact-head review") || strings.Contains(prompt, "review-9 failed") {
			t.Fatalf("stale failure escaped cross-batch reduction: %q", prompt)
		}
	}
	pending, err := store.ListWakeOutbox(ctx, db.WakeOutboxStatePending)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending fact wakes = %+v err=%v, want none", pending, err)
	}
	delivered, err := store.ListWakeOutbox(ctx, db.WakeOutboxStateDelivered)
	if err != nil {
		t.Fatal(err)
	}
	currentDelivered := false
	for _, row := range delivered {
		var payload db.AwaitedFactWakePayload
		if err := json.Unmarshal([]byte(row.SourceID), &payload); err != nil {
			t.Fatal(err)
		}
		if payload.ID == 9 {
			currentDelivered = payload.State == db.AwaitedFactStateSatisfied
		}
	}
	if !currentDelivered {
		t.Fatalf("delivered fact rows = %+v, want fact 9's current satisfied lifecycle as survivor", delivered)
	}
}

func TestFactWakeDrainDeliversDistinctFailuresSeparately(t *testing.T) {
	store, sink, wake, _ := replyWakeTestHarness(t, []replyWakeTestRole{{"owner", "w1:p0"}, {"lane", "w1:p1"}})
	ctx := context.Background()
	if err := store.AddEventRule(ctx, db.EventRule{
		ID: "fact-lane", OnKind: db.WakeOutboxKindFact, WakeRole: "lane",
		Scope: db.EventRuleScopeAddressed, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	for index, jobID := range []string{"review-alpha", "review-beta"} {
		insertAwaitedFactWakeForTest(t, store, db.AwaitedFactWakePayload{
			ID: int64(index + 1), WaiterRole: "lane", SubjectKind: db.AwaitedFactSubjectReviewVerdict,
			SubjectKey: fmt.Sprintf("acme/widget#%d@head", index+1), State: "review_failed", JobID: jobID,
			Detail: fmt.Sprintf("review job %s failed without an exact-head verdict; %s", jobID, strings.Repeat("detail ", 60)),
		})
	}

	drainReplyWakeAfterAllRowsAreDue(t, store, sink)
	if wake.promptCalls != 2 {
		t.Fatalf("prompt calls = %d, want one per unresolved failure", wake.promptCalls)
	}
	for _, jobID := range []string{"review-alpha", "review-beta"} {
		found := false
		for _, prompt := range wake.prompts {
			found = found || strings.Contains(prompt, jobID)
		}
		if !found {
			t.Fatalf("prompts = %+v, want distinct failure %s", wake.prompts, jobID)
		}
	}
}

func TestAwaitedFactExpiryTerminatesWhenWaiterRoleWasRemoved(t *testing.T) {
	root := t.TempDir()
	paths := config.PathsForHome(root)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte("[org.roles.\"owner\"]\nscope=[\"*\"]\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cfg, err := config.LoadOrg(paths)
	if err != nil {
		t.Fatalf("LoadOrg: %v", err)
	}
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	key, err := db.ReviewVerdictSubjectKey("acme/widget", 48, "head-removed-role")
	if err != nil {
		t.Fatalf("ReviewVerdictSubjectKey: %v", err)
	}
	deadline := time.Now().UTC().Add(time.Minute)
	fact, _, err := store.SubscribeAwaitedFact(context.Background(), db.AwaitedFactSubscription{
		WaiterRole: "removed-lane", SubjectKind: db.AwaitedFactSubjectReviewVerdict,
		SubjectKey: key, Deadline: deadline,
	})
	if err != nil {
		t.Fatalf("SubscribeAwaitedFact: %v", err)
	}
	var log bytes.Buffer
	for tick := 1; tick <= 3; tick++ {
		if err := evaluateAwaitedFactTTLs(context.Background(), store, cfg, &log, deadline.Add(time.Duration(tick)*time.Minute), awaitedFactTTLDependencies{}); err != nil {
			t.Fatalf("evaluateAwaitedFactTTLs tick %d: %v", tick, err)
		}
	}
	if got := strings.Count(log.String(), "is no longer configured"); got != 1 {
		t.Fatalf("removed-role expiry log count = %d, want one terminal transition; log=%q", got, log.String())
	}
	expired, err := store.ListAwaitedFacts(context.Background(), "removed-lane", db.AwaitedFactStateExpired)
	if err != nil {
		t.Fatalf("ListAwaitedFacts expired: %v", err)
	}
	waiting, err := store.ListAwaitedFacts(context.Background(), "removed-lane", db.AwaitedFactStateWaiting)
	if err != nil {
		t.Fatalf("ListAwaitedFacts waiting: %v", err)
	}
	if len(expired) != 1 || expired[0].ID != fact.ID || len(waiting) != 0 {
		t.Fatalf("after three ticks expired=%+v waiting=%+v, want one queryable terminal row and no immortal wait", expired, waiting)
	}
	outbox, err := store.ListWakeOutbox(context.Background(), db.WakeOutboxStatePending)
	if err != nil {
		t.Fatalf("ListWakeOutbox: %v", err)
	}
	if len(outbox) != 1 || outbox[0].TargetRole != "removed-lane" {
		t.Fatalf("removed-role expiry outbox = %+v, want one exact unroutable address", outbox)
	}
}

func TestOrgAwaitReviewAndList(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte("[org.roles.\"owner\"]\nscope=[\"*\"]\n[org.roles.\"lane\"]\nparent=\"owner\"\nscope=[\"*\"]\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	var stdout, stderr bytes.Buffer
	code := runOrg([]string{"await", "review", "--home", home, "--role", "lane", "--repo", "Acme/Widget", "--pr", "47", "--head", "Head-CLI", "--ttl", "10m"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("org await review code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = runOrg([]string{"await", "list", "--home", home, "--role", "lane", "--state", "waiting", "--json"}, &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), `"subject_key": "acme/widget#47@head-cli"`) || !strings.Contains(stdout.String(), `"state": "waiting"`) {
		t.Fatalf("org await list code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = runOrg([]string{"events", "rule", "add", "--home", home, "--on", "fact", "--wake", "lane"}, &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), "added event-rule-") {
		t.Fatalf("org events fact rule code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
}

// TestOrgAwaitReviewRecordsAHeadlessExclusion closes the end-to-end boundary of
// #2008's awaited-fact consumer: db reports the skipped row RAW, and this is the
// side that turns it into a stated reason.
//
// Without this the slice's coverage stops at the store, and the recording loop -
// the only part that knows the reason vocabulary - would be untested.
func TestOrgAwaitReviewRecordsAHeadlessExclusion(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte("[org.roles.\"owner\"]\nscope=[\"*\"]\n[org.roles.\"lane\"]\nparent=\"owner\"\nscope=[\"*\"]\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()
	// A session review: no head, externally driven, which is the 41-of-41 class.
	if err := store.CreateExternallyDrivenJobWithEvent(ctx, db.Job{
		ID: "session-await-review", Agent: "reviewer", Type: "review", State: "succeeded",
		Payload: `{"repo":"acme/widget","pull_request":47,"head_sha":"","result":{"decision":"approved","summary":"session"}}`,
	}, db.JobEvent{Kind: "succeeded", Message: "approved"}); err != nil {
		t.Fatalf("CreateExternallyDrivenJobWithEvent: %v", err)
	}
	store.Close()

	var stdout, stderr bytes.Buffer
	if code := runOrg([]string{"await", "review", "--home", home, "--role", "lane", "--repo", "acme/widget", "--pr", "47", "--head", "head-cli", "--ttl", "10m"}, &stdout, &stderr); code != 0 {
		t.Fatalf("org await review code=%d err=%q", code, stderr.String())
	}

	verify, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer verify.Close()
	events, err := verify.ListJobEvents(ctx, "session-await-review")
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	var recorded []string
	for _, event := range events {
		if event.Kind == workflow.HeadBoundExclusionEventKind {
			recorded = append(recorded, event.Message)
		}
	}
	if len(recorded) != 1 {
		t.Fatalf("exclusion events = %d (%v), want 1: the headless row was passed over silently", len(recorded), recorded)
	}
	if !strings.Contains(recorded[0], workflow.HeadBoundExclusionSessionRow) {
		t.Errorf("message = %q, want the session reason", recorded[0])
	}
	if !strings.Contains(recorded[0], "awaited_facts.reviewVerdict") {
		t.Errorf("message = %q, want it to name the consumer", recorded[0])
	}
}

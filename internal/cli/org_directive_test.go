package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/db/dbtest"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func directiveTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatal(err)
	}
	configBody := `[org.roles."owner"]
scope=["*"]
pane="owner-pane"
[org.roles."worker"]
parent="owner"
scope=["*"]
pane="worker-pane"
[org.roles."peer"]
parent="owner"
scope=["*"]
pane="peer-pane"
[org.roles."helper"]
parent="worker"
scope=["*"]
pane="helper-pane"
`
	if err := os.WriteFile(paths.ConfigFile, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestOrgDirectiveSendBodySourcesAndDirectionPolicy(t *testing.T) {
	t.Setenv("GITMOOT_ORG_ROLE", "owner")
	tests := []struct {
		name string
		args func(string) []string
		body string
	}{
		{"text", func(home string) []string {
			return []string{"directive", "send", "--home", home, "--to", "worker", "--workflow", "release/text", "use `a=b` [now]"}
		}, "use `a=b` [now]"},
		{"file", func(home string) []string {
			path := filepath.Join(home, "directive.txt")
			if err := os.WriteFile(path, []byte("line one\n`$HOME` ] line two"), 0o600); err != nil {
				t.Fatal(err)
			}
			return []string{"directive", "send", "--home", home, "--to", "worker", "--workflow", "release/file", "-F", path}
		}, "line one\n`$HOME` ] line two"},
		{"stdin", func(home string) []string {
			return []string{"directive", "send", "--home", home, "--to", "worker", "--workflow", "release/stdin", "--stdin"}
		}, "$(printf unsafe)\nnext"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			home := directiveTestHome(t)
			oldStdin := orgDirectiveStdin
			orgDirectiveStdin = strings.NewReader(test.body)
			t.Cleanup(func() { orgDirectiveStdin = oldStdin })
			var stdout, stderr bytes.Buffer
			if code := runOrg(test.args(home), &stdout, &stderr); code != 0 {
				t.Fatalf("send code=%d out=%q err=%q", code, stdout.String(), stderr.String())
			}
			store, err := dbtest.Open(t, config.PathsForHome(home).Database)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			notes, err := store.ListWorkflowNotes(context.Background(), "release/"+test.name, 0)
			if err != nil || len(notes) != 1 {
				t.Fatalf("notes=%+v err=%v", notes, err)
			}
			_, to, _, body, ok := workflow.ParseOrgDirectiveNote(notes[0].Body)
			if !ok || to != "worker" || body != test.body {
				t.Fatalf("directive parse=(to=%q body=%q ok=%v), want body %q", to, body, ok, test.body)
			}
		})
	}

	// MUTANT: allowing a non-ancestor sender would make this peer send succeed.
	home := directiveTestHome(t)
	t.Setenv("GITMOOT_ORG_ROLE", "peer")
	var stdout, stderr bytes.Buffer
	code := runOrg([]string{"directive", "send", "--home", home, "--to", "worker", "--workflow", "release/peer", "must fail"}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "peer and upward directives are refused") {
		t.Fatalf("peer send code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
}

func TestOrgDirectiveSendRefusesUpward(t *testing.T) {
	home := directiveTestHome(t)
	t.Setenv("GITMOOT_ORG_ROLE", "worker")
	var stdout, stderr bytes.Buffer
	code := runOrg([]string{"directive", "send", "--home", home, "--to", "owner", "--workflow", "release/upward", "must fail"}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "peer and upward directives are refused") {
		t.Fatalf("upward send code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
}

func TestOrgDirectiveAckAuthorizationAndUnackedQuery(t *testing.T) {
	home := directiveTestHome(t)
	t.Setenv("GITMOOT_ORG_ROLE", "owner")
	var stdout, stderr bytes.Buffer
	if code := runOrg([]string{"directive", "send", "--home", home, "--to", "worker", "--workflow", "release/ack", "inspect the result"}, &stdout, &stderr); code != 0 {
		t.Fatalf("send code=%d err=%q", code, stderr.String())
	}
	fields := strings.Fields(stdout.String())
	directiveID, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil {
		t.Fatal(err)
	}

	// MUTANT: trusting --by without checking the addressed target would accept peer.
	stdout.Reset()
	stderr.Reset()
	if code := runOrg([]string{"directive", "ack", fmt.Sprint(directiveID), "--home", home, "--by", "peer"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "cannot acknowledge") {
		t.Fatalf("unauthorized ack code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
	store, err := dbtest.Open(t, config.PathsForHome(home).Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	unacked, err := store.ListUnacknowledgedOrgDirectives(context.Background(), "worker")
	if err != nil || len(unacked) != 1 || unacked[0].ID != directiveID {
		t.Fatalf("unacked=%+v err=%v", unacked, err)
	}

	stdout.Reset()
	stderr.Reset()
	if code := runOrg([]string{"directive", "ack", fmt.Sprint(directiveID), "--home", home, "--by", "worker"}, &stdout, &stderr); code != 0 {
		t.Fatalf("ack code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
	unacked, err = store.ListUnacknowledgedOrgDirectives(context.Background(), "worker")
	if err != nil || len(unacked) != 0 {
		t.Fatalf("unacked after ack=%+v err=%v", unacked, err)
	}

	stdout.Reset()
	stderr.Reset()
	if code := runOrg([]string{"directive", "send", "--home", home, "--to", "worker", "--workflow", "release/ancestor-ack", "inspect another result"}, &stdout, &stderr); code != 0 {
		t.Fatalf("second send code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
	ancestorDirectiveID := strings.Fields(stdout.String())[2]
	stdout.Reset()
	stderr.Reset()
	if code := runOrg([]string{"directive", "ack", ancestorDirectiveID, "--home", home, "--by", "owner"}, &stdout, &stderr); code != 0 {
		t.Fatalf("ancestor ack code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
}

func TestOrgDirectiveReceiptRequiresActingRole(t *testing.T) {
	home := directiveTestHome(t)
	t.Setenv("GITMOOT_ORG_ROLE", "owner")
	var stdout, stderr bytes.Buffer
	if code := runOrg([]string{"directive", "send", "--home", home, "--to", "worker", "--workflow", "release/actorless", "inspect the result"}, &stdout, &stderr); code != 0 {
		t.Fatalf("send code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
	directiveID := strings.Fields(stdout.String())[2]
	t.Setenv("GITMOOT_ORG_ROLE", "")

	for _, kind := range []string{"ack", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			stdout.Reset()
			stderr.Reset()
			code := runOrg([]string{"directive", kind, directiveID, "--home", home}, &stdout, &stderr)
			if code != 1 || !strings.Contains(stderr.String(), "acting org role is required") {
				t.Fatalf("actorless %s code=%d out=%q err=%q", kind, code, stdout.String(), stderr.String())
			}
		})
	}
}

func TestOrgDirectiveCancelIsRestrictedToSender(t *testing.T) {
	home := directiveTestHome(t)
	t.Setenv("GITMOOT_ORG_ROLE", "owner")
	var stdout, stderr bytes.Buffer
	if code := runOrg([]string{"directive", "send", "--home", home, "--to", "worker", "--workflow", "release/cancel", "stop if obsolete"}, &stdout, &stderr); code != 0 {
		t.Fatalf("send code=%d err=%q", code, stderr.String())
	}
	id := strings.Fields(stdout.String())[2]
	stdout.Reset()
	stderr.Reset()
	if code := runOrg([]string{"directive", "cancel", id, "--home", home, "--by", "worker"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "cannot cancel") {
		t.Fatalf("target cancel code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := runOrg([]string{"directive", "cancel", id, "--home", home, "--by", "owner"}, &stdout, &stderr); code != 0 {
		t.Fatalf("sender cancel code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
}

func TestDirectiveWakeOutboxIsConfigInert(t *testing.T) {
	home := directiveTestHome(t)
	t.Setenv("GITMOOT_ORG_ROLE", "owner")
	var stdout, stderr bytes.Buffer
	if code := runOrg([]string{"directive", "send", "--home", home, "--to", "worker", "--workflow", "release/inert", "act"}, &stdout, &stderr); code != 0 {
		t.Fatalf("send code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
	store, err := dbtest.Open(t, config.PathsForHome(home).Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	notes, err := store.ListWorkflowNotes(context.Background(), "release/inert", 0)
	if err != nil || len(notes) != 1 {
		t.Fatalf("stored directives=%+v err=%v", notes, err)
	}
	directive := notes[0]
	wake := &fakeEventWake{}
	deliverySink := synchronousEventRuleTestSink{sink: &eventRuleSink{store: store, home: home, wake: wake}}
	pending, err := store.ListWakeOutbox(context.Background(), db.WakeOutboxStatePending)
	if err != nil || len(pending) != 1 ||
		pending[0].SourceKind != db.WakeOutboxSourceWorkflowNote ||
		pending[0].SourceID != fmt.Sprint(directive.ID) ||
		pending[0].TargetRole != "worker" ||
		pending[0].CoalesceKey != db.WakeOutboxDirectiveCoalescePrefix+"worker" {
		t.Fatalf("stored directive=%+v pending=%+v err=%v", directive, pending, err)
	}
	createdAt, _ := time.Parse(time.RFC3339Nano, pending[0].CreatedAt)

	// MUTANT F3: treating an unmatched directive as an unhealthy obligation
	// makes this zero-directive-rule drain return a permanent error.
	if err := drainReplyWakeOutbox(context.Background(), store, createdAt.Add(replyWakeCoalescingWindow+time.Second), replyWakeTestDeliveryResolver(deliverySink)); err != nil {
		t.Fatalf("config-inert drain for directive %d: %v", directive.ID, err)
	}
	health, err := wakeOutboxObligationHealth(
		context.Background(),
		store,
		createdAt.Add(-time.Minute),
		replyWakeTestDeliveryResolver(deliverySink),
	)
	if err != nil || health.pending != 0 || health.inert != 1 {
		t.Fatalf("config-inert directive health: %s err=%v", health, err)
	}
	stillPending, err := store.ListWakeOutbox(context.Background(), db.WakeOutboxStatePending)
	if err != nil || len(stillPending) != 1 || wake.promptCalls != 0 {
		t.Fatalf("pending=%+v prompts=%d err=%v", stillPending, wake.promptCalls, err)
	}
}

func TestOrgEventRuleAddAcceptsDirective(t *testing.T) {
	home := directiveTestHome(t)
	var stdout, stderr bytes.Buffer
	if code := runOrg([]string{"events", "rule", "add", "--home", home, "--on", "directive", "--wake", "worker"}, &stdout, &stderr); code != 0 {
		t.Fatalf("rule add code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
}

func TestDirectiveWakeOutboxUsesSeparateCoalesceNamespace(t *testing.T) {
	store, deliverySink, wake, _ := replyWakeTestHarness(t, []replyWakeTestRole{{name: "owner", pane: "w1:p1"}})
	ctx := context.Background()
	if err := store.AddEventRule(ctx, db.EventRule{ID: "directive-owner", OnKind: "directive", WakeRole: "owner", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	thread, err := store.CreateChatThread(ctx, db.ChatThread{Slug: "directive-coalesce", Repo: "owner/repo"})
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 2; index++ {
		if _, err := store.AddChatMessage(ctx, db.ChatMessage{
			ThreadID: thread.ID, AuthorName: "worker", Kind: db.ChatKindChat,
			Body: fmt.Sprintf("@owner reply %d", index), Mentions: []string{"owner"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	directive, err := store.InsertWorkflowNote(ctx, db.WorkflowNote{
		WorkflowID: "release/coalesce", Author: "worker", Body: workflow.FormatOrgDirectiveNote("worker", "owner", "release/coalesce", "act"),
		AddressedTarget: "owner", AddressedWakeKind: db.WakeOutboxKindDirective,
	})
	if err != nil {
		t.Fatal(err)
	}
	pending, err := store.ListWakeOutbox(ctx, db.WakeOutboxStatePending)
	if err != nil || len(pending) != 3 {
		t.Fatalf("pending=%+v err=%v", pending, err)
	}
	keys := map[string]int{}
	for _, row := range pending {
		keys[row.CoalesceKey]++
	}
	if keys["reply:owner"] != 2 || keys["directive:owner"] != 1 {
		t.Fatalf("coalesce namespaces=%v", keys)
	}

	latest, _ := time.Parse(time.RFC3339Nano, pending[len(pending)-1].CreatedAt)
	if err := drainReplyWakeOutbox(ctx, store, latest.Add(replyWakeCoalescingWindow+time.Second), replyWakeTestDeliveryResolver(deliverySink)); err != nil {
		t.Fatalf("coalesced drain: %v", err)
	}
	directivePrompt := ""
	for _, prompt := range wake.prompts {
		if strings.Contains(prompt, "gitmoot org directive ack") {
			directivePrompt = prompt
		}
	}
	if wake.promptCalls != 2 || !strings.Contains(directivePrompt, fmt.Sprintf("directive %d", directive.ID)) || !strings.Contains(directivePrompt, fmt.Sprintf("gitmoot org directive ack %d --by owner", directive.ID)) {
		t.Fatalf("directive wake calls=%d prompts=%q", wake.promptCalls, wake.prompts)
	}
}

// #1352 SITE 1 of 2: the `done` VERB and its authorization. Completion previously
// had no CLI surface at all — the persistence layer (FormatOrgDirectiveDoneNote,
// the sweep's NOT EXISTS on [org:directive-done) was already built and simply
// unexposed, so completing a directive meant hand-writing a raw marker note.
//
// `done` carries ACK's discipline, not cancel's: the role that owes the work, or
// an ancestor overseeing it, declares it finished. The sender may cancel but may
// not mark another role's obligation complete on its behalf.
func TestOrgDirectiveDoneVerbAuthorizationAndCompletion(t *testing.T) {
	home := directiveTestHome(t)
	t.Setenv("GITMOOT_ORG_ROLE", "owner")
	var stdout, stderr bytes.Buffer
	if code := runOrg([]string{"directive", "send", "--home", home, "--to", "worker", "--workflow", "release/done", "finish the thing"}, &stdout, &stderr); code != 0 {
		t.Fatalf("send code=%d err=%q", code, stderr.String())
	}
	directiveID := strings.Fields(stdout.String())[2]

	// A peer with no relationship to the target cannot complete it.
	stdout.Reset()
	stderr.Reset()
	if code := runOrg([]string{"directive", "done", directiveID, "--home", home, "--by", "peer"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "cannot record done") {
		t.Fatalf("unauthorized done code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}

	// The target may. The confirmation must read "completed", not "doneed".
	stdout.Reset()
	stderr.Reset()
	if code := runOrg([]string{"directive", "done", directiveID, "--home", home, "--by", "worker"}, &stdout, &stderr); code != 0 {
		t.Fatalf("done code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, "completed directive") || strings.Contains(got, "doneed") {
		t.Fatalf("confirmation wording = %q, want \"completed directive\"", got)
	}

	// COMPLETION SEMANTICS, not just the exit code. Rewriting the done branch to
	// write FormatOrgDirectiveAckNote left the earlier version of this test green:
	// the CLI would report "completed" while persisting only RECEIPT, leaving the
	// obligation open. Assert the persisted marker is a done marker naming this
	// directive, and that no ack marker was written in its place.
	store, err := dbtest.Open(t, config.PathsForHome(home).Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	id64, err := strconv.ParseInt(directiveID, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	notes, err := store.ListWorkflowNotes(context.Background(), "release/done", 100)
	if err != nil {
		t.Fatalf("ListWorkflowNotes returned error: %v", err)
	}
	doneMarkers, ackMarkers := 0, 0
	for _, note := range notes {
		if parsed, by, ok := workflow.ParseOrgDirectiveDoneNote(note.Body); ok && parsed == id64 {
			if by != "worker" {
				t.Fatalf("done marker recorded by %q, want worker", by)
			}
			doneMarkers++
		}
		if parsed, _, ok := workflow.ParseOrgDirectiveAckNote(note.Body); ok && parsed == id64 {
			ackMarkers++
		}
	}
	if doneMarkers != 1 || ackMarkers != 0 {
		t.Fatalf("persisted markers: done=%d ack=%d — `done` must record COMPLETION, not receipt", doneMarkers, ackMarkers)
	}

	// A role BELOW the target may complete — someone who plausibly did the work.
	stdout.Reset()
	stderr.Reset()
	if code := runOrg([]string{"directive", "send", "--home", home, "--to", "worker", "--workflow", "release/done-subtree", "and this one"}, &stdout, &stderr); code != 0 {
		t.Fatalf("second send code=%d err=%q", code, stderr.String())
	}
	secondID := strings.Fields(stdout.String())[2]
	stdout.Reset()
	stderr.Reset()
	if code := runOrg([]string{"directive", "done", secondID, "--home", home, "--by", "helper"}, &stdout, &stderr); code != 0 {
		t.Fatalf("subtree done code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
}

// #1352, the finding that survived round 1 and is the reason the predicate
// changed: SENDER COMPLETION MUST BE REFUSED.
//
// MUTANT = the previous behaviour (ancestor-permission). In a tree, `send`
// requires the sender to be an ancestor of the target, so permitting ancestors
// IS permitting the sender under another name — a role could issue a directive
// and then certify its own work as done. That is the self-approved merge.
//
// Ancestors keep `cancel`, which asserts something different: not that the work
// happened, but that it is no longer needed.
func TestOrgDirectiveDoneRefusesSenderAndAncestors(t *testing.T) {
	home := directiveTestHome(t)
	t.Setenv("GITMOOT_ORG_ROLE", "owner")
	var stdout, stderr bytes.Buffer
	if code := runOrg([]string{"directive", "send", "--home", home, "--to", "worker", "--workflow", "release/no-self-complete", "do the work"}, &stdout, &stderr); code != 0 {
		t.Fatalf("send code=%d err=%q", code, stderr.String())
	}
	directiveID := strings.Fields(stdout.String())[2]

	// The SENDER — necessarily an ancestor — must not be able to complete it.
	stdout.Reset()
	stderr.Reset()
	code := runOrg([]string{"directive", "done", directiveID, "--home", home, "--by", "owner"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("the SENDER completed its own directive: self-certification is exactly what this predicate forbids (out=%q)", stdout.String())
	}
	if !strings.Contains(stderr.String(), "cannot record done") || !strings.Contains(stderr.String(), "an ancestor may cancel instead") {
		t.Fatalf("refusal must name the alternative verb; err=%q", stderr.String())
	}

	// And no completion marker may have been persisted by the refused attempt.
	store, err := dbtest.Open(t, config.PathsForHome(home).Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	id64, err := strconv.ParseInt(directiveID, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	notes, err := store.ListWorkflowNotes(context.Background(), "release/no-self-complete", 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, note := range notes {
		if parsed, _, ok := workflow.ParseOrgDirectiveDoneNote(note.Body); ok && parsed == id64 {
			t.Fatalf("a refused sender completion still persisted a done marker: %q", note.Body)
		}
	}

	// The ancestor's real recourse still works: it may CANCEL.
	stdout.Reset()
	stderr.Reset()
	if code := runOrg([]string{"directive", "cancel", directiveID, "--home", home, "--by", "owner"}, &stdout, &stderr); code != 0 {
		t.Fatalf("ancestor cancel code=%d err=%q; excluding ancestors from done must not strand them", code, stderr.String())
	}
}

// #1352 SITE 2 of 2: the lister gap the verb EXPOSES. `done` terminates the
// obligation even with NO prior ack, so a completed-but-never-acked directive
// must stop reading as outstanding. Before the verb shipped this was unreachable
// in practice, which is why it survived.
//
// Deliberately a SEPARATE guard from the verb test: mutating the CLI verb must
// not fail this one, and mutating the lister must not fail that one — otherwise
// the two are one aggregate wearing two names.
func TestOrgDirectiveDoneClearsUnackedListWithoutAck(t *testing.T) {
	home := directiveTestHome(t)
	t.Setenv("GITMOOT_ORG_ROLE", "owner")
	var stdout, stderr bytes.Buffer
	if code := runOrg([]string{"directive", "send", "--home", home, "--to", "worker", "--workflow", "release/done-noack", "do it"}, &stdout, &stderr); code != 0 {
		t.Fatalf("send code=%d err=%q", code, stderr.String())
	}
	directiveID, err := strconv.ParseInt(strings.Fields(stdout.String())[2], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	store, err := dbtest.Open(t, config.PathsForHome(home).Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// PRECONDITION. Without this the guard is vacuous: `AND 0` on the lister makes
	// it semantically dead, every ID is absent, and an absence-only assertion stays
	// green. Prove the directive IS listed before completion, so the test measures
	// a TRANSITION rather than a permanent emptiness.
	before, err := store.ListUnacknowledgedOrgDirectives(context.Background(), "worker")
	if err != nil {
		t.Fatalf("ListUnacknowledgedOrgDirectives (before) returned error: %v", err)
	}
	listedBefore := false
	for _, note := range before {
		if note.ID == directiveID {
			listedBefore = true
		}
	}
	if !listedBefore {
		t.Fatalf("precondition failed: directive %d is not outstanding before completion; the lister is dead, not filtering (before=%+v)", directiveID, before)
	}

	// Write the completion marker directly: this guard is about the LISTER, so it
	// must not depend on the CLI verb's behaviour.
	if _, err := store.InsertWorkflowNote(context.Background(), db.WorkflowNote{
		WorkflowID: "release/done-noack",
		Author:     "worker",
		Body:       workflow.FormatOrgDirectiveDoneNote(directiveID, "worker"),
	}); err != nil {
		t.Fatalf("insert done marker: %v", err)
	}

	unacked, err := store.ListUnacknowledgedOrgDirectives(context.Background(), "worker")
	if err != nil {
		t.Fatalf("ListUnacknowledgedOrgDirectives returned error: %v", err)
	}
	for _, note := range unacked {
		if note.ID == directiveID {
			t.Fatalf("a completed directive still reads as outstanding with no ack recorded: %+v", note)
		}
	}
}

// #1352 SITE 3 of 3 (g7-review finding 2): the atomic nudge claim is a third
// obligation-state site. ListOpenOrgDirectiveObligations can return an open row,
// a terminator can commit in the gap, and MarkOrgDirectiveNudged would still
// claim it — after which the evaluator emits a nudge for an obligation that has
// already ended, so `done` did not reliably end TTL nudges as documented.
//
// This reproduces the exact race the reviewer probed: list -> insert terminator
// -> claim. The claim must refuse.
func TestOrgDirectiveNudgeClaimRefusesTerminatedObligation(t *testing.T) {
	ctx := context.Background()
	home := directiveTestHome(t)
	t.Setenv("GITMOOT_ORG_ROLE", "owner")
	var stdout, stderr bytes.Buffer
	if code := runOrg([]string{"directive", "send", "--home", home, "--to", "worker", "--workflow", "release/race", "race me"}, &stdout, &stderr); code != 0 {
		t.Fatalf("send code=%d err=%q", code, stderr.String())
	}
	directiveID, err := strconv.ParseInt(strings.Fields(stdout.String())[2], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	store, err := dbtest.Open(t, config.PathsForHome(home).Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// LIST: the evaluator observes the obligation as open.
	open0, err := store.ListOpenOrgDirectiveObligations(ctx, 200)
	if err != nil {
		t.Fatalf("ListOpenOrgDirectiveObligations returned error: %v", err)
	}
	var item db.OrgDirectiveObligation
	found := false
	for _, o := range open0 {
		if o.ID == directiveID {
			item, found = o, true
		}
	}
	if !found {
		t.Fatalf("precondition failed: directive %d not returned as open", directiveID)
	}

	// The terminator commits in the gap between list and claim.
	if _, err := store.InsertWorkflowNote(ctx, db.WorkflowNote{
		WorkflowID: "release/race",
		Author:     "worker",
		Body:       workflow.FormatOrgDirectiveDoneNote(directiveID, "worker"),
	}); err != nil {
		t.Fatalf("insert done marker: %v", err)
	}

	// CLAIM: must refuse, or a nudge fires for a completed obligation.
	_, claimed, err := store.MarkOrgDirectiveNudged(ctx, item.ID, item.NudgeCount, item.LastNudgedAt, time.Now().UTC())
	if err != nil {
		t.Fatalf("MarkOrgDirectiveNudged returned error: %v", err)
	}
	if claimed {
		t.Fatal("nudge claim succeeded on a COMPLETED directive; `done` does not end TTL nudges")
	}

	// And the claim must still work for an obligation that is genuinely open.
	stdout.Reset()
	stderr.Reset()
	if code := runOrg([]string{"directive", "send", "--home", home, "--to", "worker", "--workflow", "release/race2", "still open"}, &stdout, &stderr); code != 0 {
		t.Fatalf("second send code=%d err=%q", code, stderr.String())
	}
	openID, err := strconv.ParseInt(strings.Fields(stdout.String())[2], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	open1, err := store.ListOpenOrgDirectiveObligations(ctx, 200)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range open1 {
		if o.ID != openID {
			continue
		}
		_, ok, err := store.MarkOrgDirectiveNudged(ctx, o.ID, o.NudgeCount, o.LastNudgedAt, time.Now().UTC())
		if err != nil {
			t.Fatalf("MarkOrgDirectiveNudged (open) returned error: %v", err)
		}
		if !ok {
			t.Fatal("terminator check refused a genuinely OPEN obligation; nudging is now dead")
		}
	}
}

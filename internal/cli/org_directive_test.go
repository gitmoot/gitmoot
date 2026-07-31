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
			store, err := db.Open(config.PathsForHome(home).Database)
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
	store, err := db.Open(config.PathsForHome(home).Database)
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
	store, err := db.Open(config.PathsForHome(home).Database)
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
	if err := wakeOutboxObligationHealth(context.Background(), store, createdAt.Add(-time.Minute)); err != nil {
		t.Fatalf("config-inert directive health: %v", err)
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

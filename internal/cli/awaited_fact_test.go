package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
)

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
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	key, err := db.ReviewVerdictSubjectKey("acme/widget", 45, "head-expire")
	if err != nil {
		t.Fatalf("ReviewVerdictSubjectKey: %v", err)
	}
	deadline := time.Now().UTC().Add(time.Minute)
	fact, err := store.SubscribeAwaitedFact(context.Background(), db.AwaitedFactSubscription{
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
	prompt := eventRuleWakePrompt("fact", event)
	if !strings.Contains(prompt, "awaited fact for lane") || strings.Contains(prompt, " ack ") || strings.Contains(prompt, " done ") {
		t.Fatalf("fact prompt = %q, want delivery-only prompt without receipt ceremony", prompt)
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

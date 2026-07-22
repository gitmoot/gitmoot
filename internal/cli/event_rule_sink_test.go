package cli

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/events"
)

type fakeEventWake struct {
	availableCalls int
	promptCalls    int
	pane           string
	prompt         string
	until          string
	labelToPane    map[string]string
}

func (f *fakeEventWake) Available(context.Context) bool {
	f.availableCalls++
	return true
}

func (f *fakeEventWake) AgentPrompt(_ context.Context, pane, prompt, until string) (bool, bool, error) {
	f.promptCalls++
	f.pane, f.prompt, f.until = pane, prompt, until
	return true, false, nil
}

func (f *fakeEventWake) ResolvePaneByLabel(_ context.Context, label string) (string, bool) {
	pane, ok := f.labelToPane[label]
	return pane, ok
}

func TestClassifyEventRuleKinds(t *testing.T) {
	tests := []struct {
		name  string
		event events.Event
		want  []string
	}{
		{name: "escalation", event: events.Event{Type: events.EventJobNeedsAttention, Cause: "escalation"}, want: []string{"escalation"}},
		{name: "attention", event: events.Event{Type: events.EventJobNeedsAttention, Cause: "ask_gate"}, want: []string{"attention"}},
		{name: "merge guard", event: events.Event{Type: events.EventJobBlocked, Cause: "merge_guard"}, want: []string{"guard"}},
		{name: "permission guard", event: events.Event{Type: events.EventJobBlocked, Cause: "permission_guard"}, want: []string{"guard"}},
		{name: "blocked since only", event: events.Event{Type: events.EventJobBlocked, Cause: "blocked_since"}, want: []string{"blocked"}},
		{name: "finished terminal", event: events.Event{Type: events.EventJobFinished}, want: []string{"job-terminal"}},
		{name: "failed terminal", event: events.Event{Type: events.EventJobFailed, Cause: "unrelated"}, want: []string{"job-terminal"}},
		{name: "plain blocked terminal and blocked", event: events.Event{Type: events.EventJobBlocked}, want: []string{"job-terminal", "blocked"}},
		{name: "unknown blocked cause", event: events.Event{Type: events.EventJobBlocked, Cause: "other"}},
		{name: "unknown attention cause", event: events.Event{Type: events.EventJobNeedsAttention, Cause: "other"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyEventRuleKinds(tt.event); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %v want %v", got, tt.want)
			}
		})
	}
}

func TestEventRuleMatchV1(t *testing.T) {
	event := events.Event{Repo: "Acme/Widget", JobID: "Job-AbC"}
	for _, filter := range []string{"", "acme/w", "WIDGET", "job-a", "ABC"} {
		if !eventRuleMatches(filter, event) {
			t.Fatalf("filter %q did not match", filter)
		}
	}
	if eventRuleMatches("missing", event) {
		t.Fatal("unexpected match")
	}
}

func TestEventRuleEvaluatorResolvesPaneAndWakes(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte("[org.roles.\"owner\"]\nscope=[\"*\"]\npane=\"w1:p1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.AddEventRule(context.Background(), db.EventRule{ID: "rule-1", OnKind: "attention", MatchFilter: "WIDGET", WakeRole: "owner", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	wake := &fakeEventWake{}
	sink := &eventRuleSink{store: store, home: home, wake: wake}
	sink.evaluate(context.Background(), events.Event{Type: events.EventJobNeedsAttention, Cause: "ask_gate", Repo: "acme/widget", JobID: "job-1", Detail: "Please choose"})
	if wake.availableCalls != 1 || wake.pane != "w1:p1" || wake.until != "" {
		t.Fatalf("wake=%+v", wake)
	}
	if want := "gitmoot attention event for job job-1: Please choose"; wake.prompt != want {
		t.Fatalf("prompt=%q want=%q", wake.prompt, want)
	}
}

func TestEventRuleEvaluatorResolvesPaneLabel(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatal(err)
	}
	// A binding without ':' is a herdr pane LABEL, resolved to the live id at wake time.
	if err := os.WriteFile(paths.ConfigFile, []byte("[org.roles.\"owner\"]\nscope=[\"*\"]\npane=\"coordinator-a\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.AddEventRule(context.Background(), db.EventRule{ID: "rule-lbl", OnKind: "guard", WakeRole: "owner", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	wake := &fakeEventWake{labelToPane: map[string]string{"coordinator-a": "w2:p5"}}
	sink := &eventRuleSink{store: store, home: home, wake: wake}
	sink.evaluate(context.Background(), events.Event{Type: events.EventJobBlocked, Cause: "merge_guard", JobID: "job-9"})
	if wake.pane != "w2:p5" {
		t.Fatalf("label did not resolve to live pane id: %+v", wake)
	}
}

func TestEventRuleEvaluatorZeroRulesDoesNotProbeHerdr(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	wake := &fakeEventWake{}
	(&eventRuleSink{store: store, wake: wake}).evaluate(context.Background(), events.Event{Type: events.EventJobFinished, JobID: "job-1"})
	if wake.availableCalls != 0 {
		t.Fatalf("availability probed %d times with zero rules", wake.availableCalls)
	}
}

func TestDaemonEventSinkRuleOnlyActivationAndRemoval(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := os.MkdirAll(paths.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte("[org.roles.\"owner\"]\nscope=[\"*\"]\npane=\"w1:p1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if sink := daemonEventSink(store, paths.Home); sink != nil {
		t.Fatal("zero rules and no webhook must produce a nil sink")
	}
	if err := store.AddEventRule(context.Background(), db.EventRule{ID: "rule-activate", OnKind: "guard", WakeRole: "owner", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if sink := daemonEventSink(store, paths.Home); sink == nil {
		t.Fatal("enabled rule must activate the sink without a webhook")
	}
	if err := store.DeleteEventRule(context.Background(), "rule-activate"); err != nil {
		t.Fatal(err)
	}
	if sink := daemonEventSink(store, paths.Home); sink != nil {
		t.Fatal("removing the last rule must restore the nil off path")
	}
}

// TestEventRuleWakeFiresEachMatchingRule guards the multi-rule fan-out: a plain
// job.blocked event classifies to BOTH job-terminal and blocked, so two rules —
// one per kind — must each produce a wake. (The per-rule wake context in evaluate
// is what keeps a slow earlier wake from starving the later one; see #1060.)
func TestEventRuleWakeFiresEachMatchingRule(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte("[org.roles.\"owner\"]\nscope=[\"*\"]\npane=\"w1:p1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, r := range []db.EventRule{
		{ID: "r-term", OnKind: "job-terminal", WakeRole: "owner", Enabled: true},
		{ID: "r-blk", OnKind: "blocked", WakeRole: "owner", Enabled: true},
	} {
		if err := store.AddEventRule(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
	wake := &fakeEventWake{}
	sink := &eventRuleSink{store: store, home: home, wake: wake}
	sink.evaluate(context.Background(), events.Event{Type: events.EventJobBlocked, JobID: "job-1"})
	if wake.promptCalls != 2 {
		t.Fatalf("want a wake for each of the 2 matching rules, got %d", wake.promptCalls)
	}
}

func TestTruncateForWakeRuneSafe(t *testing.T) {
	// A multibyte run whose byte length exceeds the cap; a naive detail[:max] would
	// split a rune and emit invalid UTF-8 into the herdr prompt.
	long := strings.Repeat("é", 400) // 800 bytes, 400 runes
	out := truncateForWake(long, 320)
	if !utf8.ValidString(out) {
		t.Fatalf("truncated prompt is not valid UTF-8: %q", out)
	}
	if !strings.HasSuffix(out, "…") {
		t.Fatalf("expected ellipsis on truncation, got %q", out)
	}
	// A short ASCII string is returned unchanged (no ellipsis).
	if got := truncateForWake("ok", 320); got != "ok" {
		t.Fatalf("short string altered: %q", got)
	}
	// An invalid byte EARLY in the string must not collapse the tail: we shave
	// only the partial rune at the cut boundary, not react to bytes elsewhere.
	bad := "\xff" + strings.Repeat("b", 400)
	if got := truncateForWake(bad, 320); len(got) < 300 {
		t.Fatalf("early invalid byte over-trimmed the detail to %d bytes: %q", len(got), got)
	}
}

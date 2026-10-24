package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/org"
)

func TestMessageNotificationTimingFollowsRecipientState(t *testing.T) {
	for _, scenario := range []string{"working deadline", "idle immediately", "becomes idle"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			store, sink, wake, home := replyWakeTestHarness(t, []replyWakeTestRole{{"owner", "w1:p1"}, {"worker", "agent:worker"}})
			snapshot := org.Snapshot{
				States:       map[string]org.RoleLiveState{"worker": {State: org.StateWorking}},
				PaneBindings: map[string]org.PaneBinding{"worker": {PaneID: "w1:p2"}},
				Sessions:     map[string]org.SessionActivity{"worker": {Agent: "omp"}},
			}
			withOrgProvider(t, orgFixtureProvider{snapshot: snapshot})
			worker := defaultJobWorker(store, io.Discard, home)
			installReplyWakeProductionSink(t, worker, sink.sink)
			loop := newReplyWakeDrainLoop(store, worker, nil, io.Discard)
			base := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
			first, err := store.CreateMessage(ctx, db.Message{Sender: "owner", Recipient: "worker", Body: "first"})
			if err != nil {
				t.Fatal(err)
			}
			setWakeOutboxCreatedAt(t, store.DatabasePath(), fmt.Sprint(first.ID), base)
			if scenario == "idle immediately" {
				snapshot.States["worker"] = org.RoleLiveState{State: org.StateIdle}
				loop.tick(ctx, base)
			} else {
				loop.tick(ctx, base.Add(119*time.Second))
				if wake.promptCalls != 0 {
					t.Fatalf("working recipient interrupted before 120 seconds: %d", wake.promptCalls)
				}
				if scenario == "becomes idle" {
					snapshot.States["worker"] = org.RoleLiveState{State: org.StateIdle}
					// No insert, no watermark change and no thirty-second wait.
					loop.tick(ctx, base.Add(119*time.Second+time.Millisecond))
				} else {
					second, err := store.CreateMessage(ctx, db.Message{Sender: "owner", Recipient: "worker", Body: "later arrival"})
					if err != nil {
						t.Fatal(err)
					}
					setWakeOutboxCreatedAt(t, store.DatabasePath(), fmt.Sprint(second.ID), base.Add(119*time.Second))
					loop.tick(ctx, base.Add(120*time.Second))
					m, err := store.GetMessage(ctx, second.ID, "worker")
					if err != nil || m.Status != "submitted" {
						t.Fatalf("later arrival was not carried by due batch: %+v %v", m, err)
					}
				}
			}
			if wake.promptCalls != 1 {
				t.Fatalf("due notification submitted %d times", wake.promptCalls)
			}
			m, err := store.GetMessage(ctx, first.ID, "worker")
			if err != nil || m.Status != "submitted" {
				t.Fatalf("first message: %+v %v", m, err)
			}
			loop.tick(ctx, base.Add(5*time.Minute))
			if wake.promptCalls != 1 {
				t.Fatal("completed notification replayed")
			}
		})
	}
}

func TestWorkingTurnHookWaitsButStopReleasesMail(t *testing.T) {
	home := messageTestHome(t)
	messageID := sendTestMessage(t, home, "gm-omp-nag", "deimos", "Please inspect the failing build.")
	originalNow := turnHookNow
	now := time.Now().UTC()
	turnHookNow = func() time.Time { return now }
	t.Cleanup(func() { turnHookNow = originalNow })
	output, diagnostic := runTurnHook(t, home, "deimos", "claude", "PostToolUse", `{}`)
	if output != "" {
		t.Fatalf("working hook delivered early: %q %q", output, diagnostic)
	}
	m := showTestMessage(t, home, "gm-omp-nag", messageID)
	if m.Status != "queued" {
		t.Fatalf("held notification=%s", m.Status)
	}
	output, diagnostic = runTurnHook(t, home, "deimos", "claude", "Stop", `{}`)
	delivered := decodeTurnHookOutput(t, output)
	if delivered.HookSpecificOutput == nil || !strings.Contains(delivered.HookSpecificOutput.AdditionalContext, "Please inspect the failing build.") {
		t.Fatalf("idle Stop did not deliver the message: %q %q", output, diagnostic)
	}
	m = showTestMessage(t, home, "gm-omp-nag", messageID)
	if m.Status != "submitted" {
		t.Fatalf("released notification=%s", m.Status)
	}
}

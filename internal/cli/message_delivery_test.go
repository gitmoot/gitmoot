package cli

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/cockpit"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/org"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// Offline, unbound and non-OMP recipients keep mail pending without spending
// an attempt; a working OMP recipient is notified (its add-on decides), and an
// unknown outcome is never replayed.
func TestMessageDaemonDefersUnreachableRecipientsAndNeverReplaysUnknown(t *testing.T) {
	for _, kind := range []string{"message", "directive"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			store, sink, wake, home := replyWakeTestHarness(t, []replyWakeTestRole{{"owner", "w1:p1"}, {"worker", "agent:worker"}})
			snapshot := org.Snapshot{
				States:       map[string]org.RoleLiveState{"worker": {State: org.StateIdle}},
				PaneBindings: map[string]org.PaneBinding{"worker": {PaneID: "w1:p2"}},
				Sessions:     map[string]org.SessionActivity{},
			}
			withOrgProvider(t, orgFixtureProvider{snapshot: snapshot})
			body := "[org:message to=worker from=owner wf=safety] Queued conversation"
			if kind == "directive" {
				body = workflow.FormatOrgDirectiveNote("owner", "worker", "safety", "Queued assignment")
			}
			note, err := store.InsertWorkflowNote(ctx, db.WorkflowNote{WorkflowID: "safety", Author: "owner", Body: body, AddressedTarget: "worker"})
			if err != nil {
				t.Fatal(err)
			}
			worker := defaultJobWorker(store, io.Discard, home)
			installReplyWakeProductionSink(t, worker, sink.sink)
			var diagnostic bytes.Buffer
			tick := func(now time.Time) {
				t.Helper()
				runReplyWakeOutboxDrainOnce(ctx, store, worker, &diagnostic, now, nil)
			}
			now := time.Now().UTC().Add(time.Hour)
			for _, held := range []struct {
				name, agent, reason string
				binding             org.PaneBinding
			}{
				{"absent", "omp", "recipient is offline or its binding is unresolved", org.PaneBinding{}},
				{"ambiguous", "omp", "recipient is offline or its binding is unresolved", org.PaneBinding{PaneID: "w1:p2", Ambiguous: true}},
				{"claude", "claude", cockpit.NotificationAwaitingNextTurn, org.PaneBinding{PaneID: "w1:p2"}},
				{"codex", "codex", cockpit.NotificationAwaitingNextTurn, org.PaneBinding{PaneID: "w1:p2"}},
				{"unsupported runtime", "pi", cockpit.NotificationCapabilityUnavailable, org.PaneBinding{PaneID: "w1:p2"}},
				{"undetected runtime", "", cockpit.NotificationCapabilityUnavailable, org.PaneBinding{PaneID: "w1:p2"}},
			} {
				snapshot.PaneBindings["worker"] = held.binding
				snapshot.Sessions["worker"] = org.SessionActivity{PaneID: held.binding.PaneID, Agent: held.agent}
				for range 2 {
					tick(now)
				}
				rows, err := store.ListWakeOutbox(ctx, "")
				if err != nil {
					t.Fatal(err)
				}
				if len(rows) != 1 || rows[0].State != db.WakeOutboxStatePending || rows[0].AttemptCount != 0 || rows[0].LastError != held.reason || wake.promptCalls != 0 {
					t.Fatalf("%s was not held pending with reason %q: rows=%+v calls=%d log=%s", held.name, held.reason, rows, wake.promptCalls, diagnostic.String())
				}
				if _, err := store.GetMessage(ctx, note.ID, "worker"); err != nil {
					t.Fatalf("%s lost durable inbox: %v", held.name, err)
				}
			}
			// A working OMP recipient is notified now, not at the end of its turn.
			snapshot.States["worker"] = org.RoleLiveState{State: org.StateWorking}
			snapshot.PaneBindings["worker"] = org.PaneBinding{PaneID: "w1:p3"}
			snapshot.Sessions["worker"] = org.SessionActivity{PaneID: "w1:p3", Agent: "omp"}
			wake.labelToPane = map[string]string{"agent:worker": "w1:p3"}
			wake.onPrompt = func() error { panic("transport crashed after input may have been written") }
			tick(now)
			rows, err := store.ListWakeOutbox(ctx, "")
			if err != nil {
				t.Fatal(err)
			}
			if wake.promptCalls != 1 || wake.pane != "w1:p3" || len(rows) != 1 || rows[0].State != db.WakeOutboxStateDeliveryUnknown {
				t.Fatalf("recreated recipient/unknown outcome: rows=%+v calls=%d pane=%s", rows, wake.promptCalls, wake.pane)
			}
			wake.onPrompt = nil
			tick(now.Add(time.Hour))
			if wake.promptCalls != 1 {
				t.Fatal("unknown input was blindly replayed")
			}
		})
	}
}

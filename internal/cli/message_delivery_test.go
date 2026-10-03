package cli

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/org"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestMessageDaemonDefersUnsafeRecipientsAndNeverReplaysUnknown(t *testing.T) {
	for _, kind := range []string{"message", "directive"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			store, sink, wake, home := replyWakeTestHarness(t, []replyWakeTestRole{{"owner", "w1:p1"}, {"worker", "agent:worker"}})
			snapshot := org.Snapshot{States: map[string]org.RoleLiveState{"worker": {State: org.StateWorking}}, PaneBindings: map[string]org.PaneBinding{"worker": {PaneID: "w1:p2"}}}
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
				if err := runEnabledRepoWorkerTicksTracked(ctx, store, worker, 0, "", &diagnostic, now, nil, nil); err != nil {
					t.Fatal(err)
				}
			}
			now := time.Now().UTC().Add(time.Hour)
			for _, unsafe := range []struct {
				name    string
				state   org.LifecycleState
				binding org.PaneBinding
			}{
				{"busy", org.StateWorking, org.PaneBinding{PaneID: "w1:p2"}},
				{"dialog", org.StateBlocked, org.PaneBinding{PaneID: "w1:p2"}},
				{"draft", org.StateInputPending, org.PaneBinding{PaneID: "w1:p2"}},
				{"absent", org.StateIdle, org.PaneBinding{}},
				{"ambiguous", org.StateIdle, org.PaneBinding{PaneID: "w1:p2", Ambiguous: true}},
			} {
				snapshot.States["worker"] = org.RoleLiveState{State: unsafe.state}
				snapshot.PaneBindings["worker"] = unsafe.binding
				tick(now)
				rows, err := store.ListWakeOutbox(ctx, "")
				if err != nil {
					t.Fatal(err)
				}
				if len(rows) != 1 || rows[0].State != db.WakeOutboxStatePending || rows[0].AttemptCount != 0 || rows[0].LastError == "" || wake.promptCalls != 0 {
					t.Fatalf("%s did not defer safely: rows=%+v calls=%d log=%s", unsafe.name, rows, wake.promptCalls, diagnostic.String())
				}
				if _, err := store.GetMessage(ctx, note.ID, "worker"); err != nil {
					t.Fatalf("%s lost durable inbox: %v", unsafe.name, err)
				}
			}
			snapshot.States["worker"] = org.RoleLiveState{State: org.StateIdle}
			snapshot.PaneBindings["worker"] = org.PaneBinding{PaneID: "w1:p3"}
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

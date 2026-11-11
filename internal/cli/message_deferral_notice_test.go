package cli

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/cockpit"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/org"
)

// Spelled out rather than taken from the production constants, so the test
// also pins the wording a sender and a recipient's status show.
const (
	deferralNoticeOfflineReason      = "recipient is offline or its binding is unresolved"
	deferralNoticeUnregisteredReason = "recipient role is not registered; no destination guessed"
	deferralNoticeNoSeatReason       = "recipient has no current seat; message remains in inbox"
)

// deferralNoticeFixture is the production drain (runReplyWakeOutboxDrainOnce
// with the daemon's own delivery resolver) over a registry of owner and
// worker. Each step returns the snapshot so a case can bind, unbind or busy
// the worker; the owner stays unbound so its notice wakes stay pending.
type deferralNoticeFixture struct {
	t        *testing.T
	store    *db.Store
	wake     *fakeEventWake
	snapshot org.Snapshot
	tick     func(time.Time)
}

func newDeferralNoticeFixture(t *testing.T, workerPane string) deferralNoticeFixture {
	t.Helper()
	store, sink, wake, home := replyWakeTestHarness(t, []replyWakeTestRole{{"owner", "w1:p1"}, {"worker", workerPane}})
	snapshot := org.Snapshot{
		States:       map[string]org.RoleLiveState{"worker": {State: org.StateIdle}},
		PaneBindings: map[string]org.PaneBinding{},
		Sessions:     map[string]org.SessionActivity{},
	}
	withOrgProvider(t, orgFixtureProvider{snapshot: snapshot})
	worker := defaultJobWorker(store, io.Discard, home)
	installReplyWakeProductionSink(t, worker, sink.sink)
	return deferralNoticeFixture{t: t, store: store, wake: wake, snapshot: snapshot, tick: func(now time.Time) {
		t.Helper()
		runReplyWakeOutboxDrainOnce(context.Background(), store, worker, io.Discard, now, nil)
	}}
}

// bindWorker makes the worker an idle, bound OMP seat that the fake transport
// reaches, so its wake is delivered unless the add-on defers it.
func (f deferralNoticeFixture) bindWorker() {
	f.snapshot.PaneBindings["worker"] = org.PaneBinding{PaneID: "w1:p2"}
	f.snapshot.Sessions["worker"] = org.SessionActivity{PaneID: "w1:p2", Agent: "omp"}
	f.wake.labelToPane = map[string]string{"agent:worker": "w1:p2"}
}

// send stores a message and returns it with its wake row's creation time.
func (f deferralNoticeFixture) send(sender, recipient string) (db.Message, time.Time) {
	f.t.Helper()
	ctx := context.Background()
	message, err := f.store.CreateMessage(ctx, db.Message{Sender: sender, Recipient: recipient, Body: "please rebase onto main"})
	if err != nil {
		f.t.Fatal(err)
	}
	return message, f.wakeRow(message).createdAt
}

type deferralNoticeWakeRow struct {
	state, lastError string
	createdAt        time.Time
}

func (f deferralNoticeFixture) wakeRow(message db.Message) deferralNoticeWakeRow {
	f.t.Helper()
	rows, err := f.store.ListWakeOutbox(context.Background(), "")
	if err != nil {
		f.t.Fatal(err)
	}
	for _, row := range rows {
		if row.SourceID == fmt.Sprint(message.ID) && row.TargetRole == message.Recipient {
			createdAt, err := time.Parse(time.RFC3339Nano, row.CreatedAt)
			if err != nil {
				f.t.Fatal(err)
			}
			return deferralNoticeWakeRow{state: row.State, lastError: row.LastError, createdAt: createdAt}
		}
	}
	f.t.Fatalf("no wake row for message %d in %+v", message.ID, rows)
	return deferralNoticeWakeRow{}
}

// notices returns every deferral notice in the store, whoever it went to.
func (f deferralNoticeFixture) notices() []db.Message {
	f.t.Helper()
	raw, err := sql.Open("sqlite", f.store.DatabasePath())
	if err != nil {
		f.t.Fatal(err)
	}
	defer raw.Close()
	rows, err := raw.Query(`SELECT recipient, body FROM messages WHERE sender = ? AND body LIKE 'Your message %' ORDER BY id`, db.MessageSystemSender)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []db.Message
	for rows.Next() {
		var message db.Message
		if err := rows.Scan(&message.Recipient, &message.Body); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, message)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatal(err)
	}
	return out
}

// #2574: a message whose wake stays undelivered past the threshold earns its
// sender exactly one system notice naming the message, recipient, wait and
// current reason; nothing before the threshold and nothing on later passes.
// "runtime busy" is the 11 Nov incident: an idle OMP add-on deferring as busy.
func TestMessageDeferralNoticeTellsTheSenderOnce(t *testing.T) {
	for _, test := range []struct {
		name   string
		setup  func(deferralNoticeFixture)
		reason string
	}{
		{
			name: "runtime busy",
			setup: func(f deferralNoticeFixture) {
				f.bindWorker()
				f.wake.promptErr = &cockpit.NotificationDeferred{Reason: "busy"}
			},
			reason: "notification deferred: busy",
		},
		{name: "recipient offline", setup: func(deferralNoticeFixture) {}, reason: deferralNoticeOfflineReason},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newDeferralNoticeFixture(t, "agent:worker")
			test.setup(f)
			message, createdAt := f.send("owner", "worker")

			f.tick(createdAt.Add(5 * time.Minute))
			if row := f.wakeRow(message); row.state != db.WakeOutboxStatePending || row.lastError != test.reason {
				t.Fatalf("wake row after 5m = %+v, want pending with %q", row, test.reason)
			}
			if notices := f.notices(); len(notices) != 0 {
				t.Fatalf("notices before the threshold = %+v, want none", notices)
			}

			f.tick(createdAt.Add(16 * time.Minute))
			notices := f.notices()
			if len(notices) != 1 || notices[0].Recipient != "owner" {
				t.Fatalf("notices after 16m = %+v, want one to the sender", notices)
			}
			for _, want := range []string{
				fmt.Sprintf("Your message %d to worker", message.ID),
				"waited 16m undelivered",
				"Current reason: " + test.reason + ".",
				"stays queued and will be delivered when worker can take it",
				"no need to type it into worker's pane",
			} {
				if !strings.Contains(notices[0].Body, want) {
					t.Fatalf("notice %q does not contain %q", notices[0].Body, want)
				}
			}

			f.tick(createdAt.Add(17 * time.Minute))
			f.tick(createdAt.Add(30 * time.Minute))
			if notices := f.notices(); len(notices) != 1 {
				t.Fatalf("notices after later passes = %+v, want still one", notices)
			}
		})
	}
}

// #2574: who is never told. Each case drives the same production drain past
// the threshold and first proves the row reached the state the case names, so
// a passing case cannot be a fixture that never deferred.
func TestMessageDeferralNoticeSkips(t *testing.T) {
	for _, test := range []struct {
		name       string
		workerPane string
		setup      func(*testing.T, deferralNoticeFixture) (db.Message, time.Time)
		after      time.Duration
		wantState  string
		wantReason string
	}{
		{
			// Held offline first, so the row carries a reason the drain's opening
			// projection still shows when the same pass delivers it.
			name: "delivered", workerPane: "agent:worker",
			setup: func(t *testing.T, f deferralNoticeFixture) (db.Message, time.Time) {
				message, createdAt := f.send("owner", "worker")
				f.tick(createdAt.Add(5 * time.Minute))
				if row := f.wakeRow(message); row.lastError != deferralNoticeOfflineReason {
					t.Fatalf("wake row before binding = %+v, want held offline", row)
				}
				f.bindWorker()
				return message, createdAt
			},
			after: 16 * time.Minute, wantState: db.WakeOutboxStateDelivered,
		},
		{
			name: "system sender", workerPane: "agent:worker",
			setup: func(_ *testing.T, f deferralNoticeFixture) (db.Message, time.Time) {
				return f.send(db.MessageSystemSender, "worker")
			},
			after: 16 * time.Minute, wantState: db.WakeOutboxStatePending, wantReason: deferralNoticeOfflineReason,
		},
		{
			name: "self message", workerPane: "agent:worker",
			setup: func(t *testing.T, f deferralNoticeFixture) (db.Message, time.Time) {
				message, createdAt := f.send("owner", "worker")
				// The schema refuses sender = recipient exactly, but role names
				// are case-insensitive, so "WORKER" to "worker" is still the
				// recipient writing to itself, and a registered role besides.
				raw, err := sql.Open("sqlite", f.store.DatabasePath())
				if err != nil {
					t.Fatal(err)
				}
				defer raw.Close()
				if _, err := raw.Exec(`UPDATE messages SET sender = upper(recipient) WHERE id = ?`, message.ID); err != nil {
					t.Fatal(err)
				}
				return message, createdAt
			},
			after: 16 * time.Minute, wantState: db.WakeOutboxStatePending, wantReason: deferralNoticeOfflineReason,
		},
		{
			name: "unregistered sender", workerPane: "agent:worker",
			setup: func(_ *testing.T, f deferralNoticeFixture) (db.Message, time.Time) {
				return f.send("ghost", "worker")
			},
			after: 16 * time.Minute, wantState: db.WakeOutboxStatePending, wantReason: deferralNoticeOfflineReason,
		},
		{
			name: "recipient not registered", workerPane: "agent:worker",
			setup: func(t *testing.T, f deferralNoticeFixture) (db.Message, time.Time) {
				if err := f.store.AddEventRule(context.Background(), db.EventRule{ID: "reply-retired", OnKind: "reply", WakeRole: "retired", Enabled: true}); err != nil {
					t.Fatal(err)
				}
				return f.send("owner", "retired")
			},
			after: 16 * time.Minute, wantState: db.WakeOutboxStatePending, wantReason: deferralNoticeUnregisteredReason,
		},
		{
			name: "recipient has no current seat", workerPane: "",
			setup: func(_ *testing.T, f deferralNoticeFixture) (db.Message, time.Time) {
				return f.send("owner", "worker")
			},
			after: 16 * time.Minute, wantState: db.WakeOutboxStatePending, wantReason: deferralNoticeNoSeatReason,
		},
		{
			// The deploy-day cutoff: 24h is messageDeferralNoticeMaxAge.
			name: "older than the notice window", workerPane: "agent:worker",
			setup: func(_ *testing.T, f deferralNoticeFixture) (db.Message, time.Time) {
				return f.send("owner", "worker")
			},
			after: 24*time.Hour + time.Minute, wantState: db.WakeOutboxStatePending, wantReason: deferralNoticeOfflineReason,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newDeferralNoticeFixture(t, test.workerPane)
			message, createdAt := test.setup(t, f)
			for _, after := range []time.Duration{test.after, test.after + time.Minute} {
				f.tick(createdAt.Add(after))
			}
			if row := f.wakeRow(message); row.state != test.wantState || row.lastError != test.wantReason {
				t.Fatalf("wake row = %+v, want state %q reason %q", row, test.wantState, test.wantReason)
			}
			if notices := f.notices(); len(notices) != 0 {
				t.Fatalf("notices = %+v, want none", notices)
			}
		})
	}
}

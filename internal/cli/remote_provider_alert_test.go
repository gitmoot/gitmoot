package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/db/dbtest"
)

// #2680: when E2B refuses new sandboxes, every remote review fails the same
// way within minutes. The operator gets ONE message for the cluster, not one
// per job, and not again within the hour, even after a daemon restart.
func TestRemoteProviderRefusalClusterAlertsOperatorOncePerHour(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gitmoot.db")
	store, err := dbtest.Open(t, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	refuse := func(store *db.Store, jobID, provider string) {
		t.Helper()
		if err := store.AddJobEvent(ctx, db.JobEvent{JobID: jobID, Kind: remoteProviderRefusedEventKind, Message: "refused", Provider: provider}); err != nil {
			t.Fatal(err)
		}
	}
	alert := func(store *db.Store, now time.Time) bool {
		t.Helper()
		sent, err := alertRemoteProviderRefusalCluster(ctx, store, "e2b", now)
		if err != nil {
			t.Fatal(err)
		}
		return sent
	}
	operatorMessages := func(store *db.Store) []db.Message {
		t.Helper()
		messages, err := store.ListMessages(ctx, remoteProviderAlertRecipient, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		return messages
	}

	now := time.Now().UTC()
	refuse(store, "review-1", "e2b")
	refuse(store, "review-2", "e2b")
	// Another provider's refusal and a second event for the same job are not
	// separate E2B failures.
	refuse(store, "review-3", "sandboxd")
	refuse(store, "review-2", "e2b")
	if alert(store, now) {
		t.Fatal("alerted for two failed jobs; the threshold is three")
	}

	refuse(store, "review-3", "e2b")
	if !alert(store, now) {
		t.Fatal("three failed jobs within ten minutes raised no alert")
	}
	messages := operatorMessages(store)
	if len(messages) != 1 {
		t.Fatalf("operator has %d messages, want exactly one alert: %+v", len(messages), messages)
	}
	body := messages[0].Body
	for _, want := range []string{"3 remote jobs", "review-3, review-2, review-1", "spending limit", "E2B dashboard"} {
		if !strings.Contains(body, want) {
			t.Fatalf("alert %q does not contain %q", body, want)
		}
	}
	if messages[0].Sender != db.MessageSystemSender {
		t.Fatalf("alert sender = %q, want %q", messages[0].Sender, db.MessageSystemSender)
	}
	// The window reads job_events.created_at, which SQLite stamps with the real
	// clock, so time passing is simulated by aging the throttle's last-alert
	// stamp, never by advancing now: every check below still sees a cluster of
	// at least three, and only the throttle can hold the alert back.
	ageThrottle := func(store *db.Store, by time.Duration) {
		t.Helper()
		stamp := time.Now().UTC().Add(-by).Format(db.BlockedEpisodeTimeLayout)
		if err := store.ExecForTest(ctx, fmt.Sprintf(`UPDATE org_blocked_episodes SET emitted_at = '%s' WHERE subject = 'provider-refusal:e2b'`, stamp)); err != nil {
			t.Fatal(err)
		}
	}
	refuse(store, "review-4", "e2b")
	if alert(store, time.Now().UTC()) {
		t.Fatal("alerted again right after the first alert")
	}
	ageThrottle(store, 50*time.Minute)

	// A daemon restart reopens the store; the throttle is durable.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	refuse(reopened, "review-5", "e2b")
	if alert(reopened, time.Now().UTC()) {
		t.Fatal("alerted again 50 minutes after the last alert, once the store was reopened")
	}
	if got := len(operatorMessages(reopened)); got != 1 {
		t.Fatalf("operator has %d messages after the reopen, want still one", got)
	}

	// An hour after the last alert, a cluster still in progress alerts again.
	ageThrottle(reopened, 61*time.Minute)
	if !alert(reopened, time.Now().UTC()) {
		t.Fatal("no alert an hour after the last one, with the cluster still in progress")
	}
	if got := len(operatorMessages(reopened)); got != 2 {
		t.Fatalf("operator has %d messages, want the second alert", got)
	}
}

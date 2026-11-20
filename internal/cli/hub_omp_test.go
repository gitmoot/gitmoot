package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
)

type hubOMPClaimResponse struct {
	Claimed []hubOMPClaimed `json:"claimed"`
}

func hubOMPClaim(t *testing.T, client *http.Client, base string) []hubOMPClaimed {
	t.Helper()
	status, body := hubCall(t, client, http.MethodPost, base+"/v1/notifications/omp/claim", "", `{}`)
	if status != http.StatusOK {
		t.Fatalf("omp claim %d %s", status, body)
	}
	var response hubOMPClaimResponse
	if err := json.Unmarshal([]byte(body), &response); err != nil {
		t.Fatal(err)
	}
	return response.Claimed
}

// A remote omp seat's client claims its mail, and each outcome lands as the
// daemon's own would: delivered with the machine's receipt, deferred back to
// pending with the add-on's reason, and no outcome at all leaving the row
// attempted so it is never resent blindly.
func TestHubOMPTwoPhaseDeliveryOutcomes(t *testing.T) {
	home := messageTestHome(t)
	first := sendTestMessage(t, home, "gm-omp-nag", "deimos", "First: please look at the queue.")
	other := sendTestMessage(t, home, "gm-omp-nag", "jarvis", "Not for deimos.")
	client, base := hubWorkClient(t, home, "deimos")

	claimed := hubOMPClaim(t, client, base)
	if len(claimed) != 1 || claimed[0].MessageID != first || !strings.Contains(claimed[0].Prompt, fmt.Sprintf("gitmoot message show %d", first)) {
		t.Fatalf("claimed = %+v", claimed)
	}
	if again := hubOMPClaim(t, client, base); len(again) != 0 {
		t.Fatalf("an attempted row was claimed twice: %+v", again)
	}
	if got := turnHookWakeReceipts(t, home, "deimos")[fmt.Sprint(first)]; !strings.HasPrefix(got, "attempted|") {
		t.Fatalf("after claim: %q, want attempted", got)
	}

	// Another role's notification cannot be finished by this machine, even
	// with this machine's own current attempt token.
	jarvisRow := wakeOutboxIDForMessage(t, home, "jarvis", other)
	if status, body := hubCall(t, client, http.MethodPost, base+"/v1/notifications/omp/finish", "",
		fmt.Sprintf(`{"id":%d,"attempt":%q,"outcome":"delivered"}`, jarvisRow, claimed[0].Attempt)); status != http.StatusConflict {
		t.Fatalf("finishing jarvis's row: %d %s", status, body)
	}

	// Deferred: back to pending with the reason, and claimable again.
	if status, body := hubCall(t, client, http.MethodPost, base+"/v1/notifications/omp/finish", "",
		fmt.Sprintf(`{"id":%d,"attempt":%q,"outcome":"deferred","reason":"omp inbox add-on not listening"}`, claimed[0].ID, claimed[0].Attempt)); status != http.StatusOK {
		t.Fatalf("defer %d %s", status, body)
	}
	if got := turnHookWakeReceipts(t, home, "deimos")[fmt.Sprint(first)]; !strings.HasPrefix(got, "pending|") {
		t.Fatalf("after defer: %q, want pending", got)
	}
	claimed = hubOMPClaim(t, client, base)
	if len(claimed) != 1 || claimed[0].MessageID != first {
		t.Fatalf("reclaim after defer = %+v", claimed)
	}

	// Delivered: the machine's receipt, and the row is finished.
	if status, body := hubCall(t, client, http.MethodPost, base+"/v1/notifications/omp/finish", "",
		fmt.Sprintf(`{"id":%d,"attempt":%q,"outcome":"delivered"}`, claimed[0].ID, claimed[0].Attempt)); status != http.StatusOK {
		t.Fatalf("deliver %d %s", status, body)
	}
	if got := turnHookWakeReceipts(t, home, "deimos")[fmt.Sprint(first)]; got != "delivered|omp-client:m" {
		t.Fatalf("after deliver: %q", got)
	}
	if status, _ := hubCall(t, client, http.MethodPost, base+"/v1/notifications/omp/finish", "",
		fmt.Sprintf(`{"id":%d,"attempt":%q,"outcome":"delivered"}`, claimed[0].ID, claimed[0].Attempt)); status != http.StatusConflict {
		t.Fatalf("finishing a delivered row again: %d", status)
	}
	if status, _ := hubCall(t, client, http.MethodPost, base+"/v1/notifications/omp/finish", "",
		fmt.Sprintf(`{"id":%d,"attempt":%q,"outcome":"uncertain"}`, claimed[0].ID, claimed[0].Attempt)); status != http.StatusBadRequest {
		t.Fatalf("an uncertain outcome was accepted: %d", status)
	}
	if got := showTestMessage(t, home, "jarvis", other).Status; got != "queued" {
		t.Fatalf("jarvis's notification = %q, want queued", got)
	}
}

// wakeOutboxIDForMessage returns the wake_outbox row id delivering message to
// role.
func wakeOutboxIDForMessage(t *testing.T, home, role string, message int64) int64 {
	t.Helper()
	store, err := db.Open(config.PathsForHome(home).Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	entries, err := store.ListWakeOutbox(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.TargetRole == role && entry.SourceID == fmt.Sprint(message) {
			return entry.ID
		}
	}
	t.Fatalf("no wake row for message %d to %s", message, role)
	return 0
}

// A late or replayed outcome for an earlier attempt cannot change a newer
// attempt of the same message (#2717 review).
func TestHubOMPStaleFinishCannotReopenANewerAttempt(t *testing.T) {
	home := messageTestHome(t)
	message := sendTestMessage(t, home, "gm-omp-nag", "deimos", "Only once, please.")
	client, base := hubWorkClient(t, home, "deimos")
	first := hubOMPClaim(t, client, base)
	if len(first) != 1 {
		t.Fatalf("first claim = %+v", first)
	}
	deferral := fmt.Sprintf(`{"id":%d,"attempt":%q,"outcome":"deferred","reason":"add-on busy"}`, first[0].ID, first[0].Attempt)
	if status, body := hubCall(t, client, http.MethodPost, base+"/v1/notifications/omp/finish", "", deferral); status != http.StatusOK {
		t.Fatalf("defer %d %s", status, body)
	}
	second := hubOMPClaim(t, client, base)
	if len(second) != 1 || second[0].Attempt == first[0].Attempt {
		t.Fatalf("second claim = %+v (first attempt %s)", second, first[0].Attempt)
	}
	if status, body := hubCall(t, client, http.MethodPost, base+"/v1/notifications/omp/finish", "", deferral); status != http.StatusConflict {
		t.Fatalf("replayed deferral of the first attempt: %d %s", status, body)
	}
	if got := turnHookWakeReceipts(t, home, "deimos")[fmt.Sprint(message)]; !strings.HasPrefix(got, "attempted|") {
		t.Fatalf("the newer attempt was changed: %q", got)
	}
	if again := hubOMPClaim(t, client, base); len(again) != 0 {
		t.Fatalf("the message was claimed a third time: %+v", again)
	}
}

// A claim is all or nothing: when one row of the batch cannot be claimed,
// none is, and nothing is left attempted without its prompt reaching the
// client (#2717 review).
func TestHubOMPClaimIsAllOrNothing(t *testing.T) {
	home := messageTestHome(t)
	first := sendTestMessage(t, home, "gm-omp-nag", "deimos", "First of two.")
	second := sendTestMessage(t, home, "jarvis", "deimos", "Second of two.")
	client, base := hubWorkClient(t, home, "deimos")
	secondRow := wakeOutboxIDForMessage(t, home, "deimos", second)
	raw, err := sql.Open("sqlite", config.PathsForHome(home).Database)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(fmt.Sprintf(`CREATE TRIGGER fail_second BEFORE UPDATE OF state ON wake_outbox
		WHEN OLD.id = %d AND NEW.state = 'attempted' BEGIN SELECT RAISE(ABORT, 'injected'); END`, secondRow)); err != nil {
		t.Fatal(err)
	}
	if status, _ := hubCall(t, client, http.MethodPost, base+"/v1/notifications/omp/claim", "", `{}`); status != http.StatusInternalServerError {
		t.Fatalf("claim with an injected failure: %d", status)
	}
	receipts := turnHookWakeReceipts(t, home, "deimos")
	for _, id := range []int64{first, second} {
		if got := receipts[fmt.Sprint(id)]; !strings.HasPrefix(got, "pending|") {
			t.Fatalf("message %d after a failed claim: %q, want pending", id, got)
		}
	}
	if _, err := raw.Exec(`DROP TRIGGER fail_second`); err != nil {
		t.Fatal(err)
	}
	if claimed := hubOMPClaim(t, client, base); len(claimed) != 2 {
		t.Fatalf("claim after the failure cleared = %+v", claimed)
	}
}

// Binding a role retires the notifications that have waited longer than the
// threshold: the new agent's client claims nothing stale, the messages stay
// in the inbox, and mail sent after the bind is delivered as usual (#2721:
// the mac-studio bind pushed weeks of stale instructions into a fresh agent).
func TestBindRetiresOlderNotificationsButKeepsTheMessages(t *testing.T) {
	home := messageTestHome(t)
	old := sendTestMessage(t, home, "gm-omp-nag", "deimos", "Weeks old: owner authorized disk cleanup.")
	client, base := hubWorkClient(t, home, "deimos")
	// A bind two days later, with the default threshold: the message has
	// waited longer than 24h, so it is retired.
	retired, kept, err := retireBacklogOnBind(config.PathsForHome(home), "deimos", "agent:mac/deimos", time.Now().UTC().Add(48*time.Hour), defaultBindRetireAge)
	if err != nil || retired != 1 || kept != 0 {
		t.Fatalf("retired, kept = %d, %d, %v; want 1, 0", retired, kept, err)
	}
	fresh := sendTestMessage(t, home, "gm-omp-nag", "deimos", "Sent after the bind.")
	claimed := hubOMPClaim(t, client, base)
	if len(claimed) != 1 || claimed[0].MessageID != fresh {
		t.Fatalf("claimed = %+v, want only message %d", claimed, fresh)
	}
	if got := turnHookWakeReceipts(t, home, "deimos")[fmt.Sprint(old)]; !strings.HasPrefix(got, "superseded|retired when role deimos was bound") {
		t.Fatalf("old notification = %q", got)
	}
	var inbox []messageTestResult
	if err := json.Unmarshal(messageCLI(t, home, "deimos", "inbox"), &inbox); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, message := range inbox {
		found = found || (message.ID == old && strings.Contains(message.Body, "Weeks old"))
	}
	if !found {
		t.Fatalf("the retired message is no longer readable: %+v", inbox)
	}
	// Another role's waiting mail is not touched by deimos's bind, even one
	// that retires everything.
	other := sendTestMessage(t, home, "gm-omp-nag", "jarvis", "For jarvis.")
	if _, _, err := retireBacklogOnBind(config.PathsForHome(home), "deimos", "agent:mac/deimos", time.Now().UTC().Add(time.Second), 0); err != nil {
		t.Fatal(err)
	}
	if got := turnHookWakeReceipts(t, home, "jarvis")[fmt.Sprint(other)]; !strings.HasPrefix(got, "pending|") {
		t.Fatalf("jarvis's notification after deimos's bind: %q", got)
	}
}

// A routine re-bind (a seat's pane changed) keeps work that arrived minutes
// earlier pending and delivers it; only --retire-older-than 0 retires it
// (#2721 review by the gitmoot seat).
func TestRebindKeepsRecentNotificationsPending(t *testing.T) {
	home := messageTestHome(t)
	recent := sendTestMessage(t, home, "gm-omp-nag", "deimos", "Five minutes old: please look.")
	client, base := hubWorkClient(t, home, "deimos")
	boundAt := time.Now().UTC().Add(5 * time.Minute)
	retired, kept, err := retireBacklogOnBind(config.PathsForHome(home), "deimos", "agent:deimos", boundAt, defaultBindRetireAge)
	if err != nil || retired != 0 || kept != 1 {
		t.Fatalf("re-bind: retired, kept = %d, %d, %v; want 0, 1", retired, kept, err)
	}
	if claimed := hubOMPClaim(t, client, base); len(claimed) != 1 || claimed[0].MessageID != recent {
		t.Fatalf("the recent message was not delivered after a re-bind: %+v", claimed)
	}
	second := sendTestMessage(t, home, "gm-omp-nag", "deimos", "Also recent.")
	retired, kept, err = retireBacklogOnBind(config.PathsForHome(home), "deimos", "agent:deimos", time.Now().UTC().Add(5*time.Minute), 0)
	if err != nil || retired != 1 || kept != 0 {
		t.Fatalf("--retire-older-than 0: retired, kept = %d, %d, %v; want 1, 0", retired, kept, err)
	}
	if got := turnHookWakeReceipts(t, home, "deimos")[fmt.Sprint(second)]; !strings.HasPrefix(got, "superseded|") {
		t.Fatalf("with threshold 0: %q", got)
	}
}

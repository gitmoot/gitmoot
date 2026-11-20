package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

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

	// Another role's notification cannot be finished by this machine.
	jarvisRow := wakeOutboxIDForMessage(t, home, "jarvis", other)
	if status, body := hubCall(t, client, http.MethodPost, base+"/v1/notifications/omp/finish", "",
		fmt.Sprintf(`{"ids":[%d],"outcome":"delivered"}`, jarvisRow)); status != http.StatusForbidden {
		t.Fatalf("finishing jarvis's row: %d %s", status, body)
	}

	// Deferred: back to pending with the reason, and claimable again.
	if status, body := hubCall(t, client, http.MethodPost, base+"/v1/notifications/omp/finish", "",
		fmt.Sprintf(`{"ids":[%d],"outcome":"deferred","reason":"omp inbox add-on not listening"}`, claimed[0].ID)); status != http.StatusOK {
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
		fmt.Sprintf(`{"ids":[%d],"outcome":"delivered"}`, claimed[0].ID)); status != http.StatusOK {
		t.Fatalf("deliver %d %s", status, body)
	}
	if got := turnHookWakeReceipts(t, home, "deimos")[fmt.Sprint(first)]; got != "delivered|omp-client:m" {
		t.Fatalf("after deliver: %q", got)
	}
	if status, _ := hubCall(t, client, http.MethodPost, base+"/v1/notifications/omp/finish", "",
		fmt.Sprintf(`{"ids":[%d],"outcome":"delivered"}`, claimed[0].ID)); status != http.StatusConflict {
		t.Fatalf("finishing a delivered row again: %d", status)
	}
	if status, _ := hubCall(t, client, http.MethodPost, base+"/v1/notifications/omp/finish", "",
		fmt.Sprintf(`{"ids":[%d],"outcome":"uncertain"}`, claimed[0].ID)); status != http.StatusBadRequest {
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

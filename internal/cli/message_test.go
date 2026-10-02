package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/db/dbtest"
)

type messageTestResult struct {
	ID         int64  `json:"id"`
	ThreadID   int64  `json:"thread_id"`
	ReplyTo    int64  `json:"reply_to"`
	Sender     string `json:"from"`
	Recipient  string `json:"to"`
	Body       string `json:"message"`
	WorkflowID string `json:"workflow"`
	Status     string `json:"notification_status"`
}

func messageTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0700); err != nil {
		t.Fatal(err)
	}
	body := `[org.roles."owner"]
scope=["*"]
[org.roles."gitmoot"]
parent="owner"
scope=["*"]
[org.roles."jarvis"]
parent="owner"
scope=["*"]
[org.roles."deimos"]
parent="owner"
scope=["*"]
[org.roles."gm-omp-nag"]
parent="gitmoot"
scope=["gitmoot/nag"]
[org.roles."gm-omp-impl"]
parent="gitmoot"
scope=["gitmoot/implementation"]
`
	if err := os.WriteFile(paths.ConfigFile, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return home
}

func messageCLI(t *testing.T, home, role string, args ...string) []byte {
	t.Helper()
	var out, diag bytes.Buffer
	command := append([]string{"message"}, args...)
	command = append(command, "--role", role, "--home", home, "--json")
	if code := Run(command, &out, &diag); code != 0 {
		t.Fatalf("%v exit=%d: %s", args, code, diag.String())
	}
	return out.Bytes()
}

func TestMessageConversationCrossTreeWithoutWorkflow(t *testing.T) {
	home := messageTestHome(t)
	var sent, replied messageTestResult
	if err := json.Unmarshal(messageCLI(t, home, "gm-omp-nag", "send", "deimos", "Please inspect this."), &sent); err != nil {
		t.Fatal(err)
	}
	if sent.WorkflowID != "" || sent.Sender != "gm-omp-nag" || sent.Recipient != "deimos" || sent.ThreadID != sent.ID || sent.Status != "queued" {
		t.Fatalf("sent=%+v", sent)
	}
	var inbox []messageTestResult
	if err := json.Unmarshal(messageCLI(t, home, "deimos", "inbox"), &inbox); err != nil {
		t.Fatal(err)
	}
	if len(inbox) != 1 || inbox[0].ID != sent.ID || inbox[0].Body != "Please inspect this." {
		t.Fatalf("inbox=%+v", inbox)
	}
	if err := json.Unmarshal(messageCLI(t, home, "deimos", "reply", fmt.Sprint(sent.ID), "I will check."), &replied); err != nil {
		t.Fatal(err)
	}
	if replied.ReplyTo != sent.ID || replied.ThreadID != sent.ID || replied.Recipient != sent.Sender {
		t.Fatalf("reply=%+v", replied)
	}
	if err := json.Unmarshal(messageCLI(t, home, "gm-omp-nag", "inbox"), &inbox); err != nil {
		t.Fatal(err)
	}
	if len(inbox) != 1 || inbox[0].ID != replied.ID {
		t.Fatalf("sender inbox=%+v", inbox)
	}
	var shown messageTestResult
	if err := json.Unmarshal(messageCLI(t, home, "deimos", "show", fmt.Sprint(sent.ID)), &shown); err != nil {
		t.Fatal(err)
	}
	if shown.Status != "queued" {
		t.Fatalf("reading changed transport status: %+v", shown)
	}
	store, err := dbtest.Open(t, config.PathsForHome(home).Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	jobs, err := store.CountJobsByWorkflow(context.Background(), "")
	if err != nil || jobs != 0 {
		t.Fatalf("fabricated jobs: %d %v", jobs, err)
	}
	wakes, err := store.ListWakeOutbox(context.Background(), db.WakeOutboxStatePending)
	if err != nil || len(wakes) != 2 || wakes[0].TargetRole != "deimos" || wakes[1].TargetRole != "gm-omp-nag" {
		t.Fatalf("wakes=%+v err=%v", wakes, err)
	}
}

func TestMessageParticipantIsolationAndStableInboxPagination(t *testing.T) {
	home := messageTestHome(t)
	var sent [3]messageTestResult
	for i := range sent {
		if err := json.Unmarshal(messageCLI(t, home, "owner", "send", "deimos", fmt.Sprint(i)), &sent[i]); err != nil {
			t.Fatal(err)
		}
	}
	for _, action := range []string{"show", "reply"} {
		args := []string{"message", action, fmt.Sprint(sent[0].ID)}
		if action == "reply" {
			args = append(args, "hijack")
		}
		args = append(args, "--role", "jarvis", "--home", home)
		var out, diag bytes.Buffer
		if code := Run(args, &out, &diag); code == 0 {
			t.Fatalf("outsider %s accepted: %s", action, out.String())
		}
	}
	var page []messageTestResult
	if err := json.Unmarshal(messageCLI(t, home, "deimos", "inbox", "--limit", "2"), &page); err != nil {
		t.Fatal(err)
	}
	if len(page) != 2 || page[0].ID != sent[2].ID || page[1].ID != sent[1].ID {
		t.Fatalf("page=%+v", page)
	}
	messageCLI(t, home, "owner", "send", "deimos", "arrives while paging")
	if err := json.Unmarshal(messageCLI(t, home, "deimos", "inbox", "--before", fmt.Sprint(page[1].ID), "--limit", "2"), &page); err != nil {
		t.Fatal(err)
	}
	if len(page) != 1 || page[0].ID != sent[0].ID {
		t.Fatalf("cursor skipped/duplicated mail: %+v", page)
	}
}

func TestMessageInvalidRecipientsDoNotCreateMail(t *testing.T) {
	home := messageTestHome(t)
	for _, recipient := range []string{"missing", "owner"} {
		var out, diag bytes.Buffer
		if code := Run([]string{"message", "send", recipient, "hello", "--role", "owner", "--home", home}, &out, &diag); code == 0 {
			t.Fatalf("accepted %q", recipient)
		}
	}
	var inbox []messageTestResult
	if err := json.Unmarshal(messageCLI(t, home, "owner", "inbox"), &inbox); err != nil {
		t.Fatal(err)
	}
	if len(inbox) != 0 {
		t.Fatalf("invalid send created mail: %+v", inbox)
	}
}

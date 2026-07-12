package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
)

func TestWorkflowIDOmittedPayloadIsByteIdentical(t *testing.T) {
	payload := JobPayload{Repo: "acme/widget"}
	got, err := marshalPayload(payload)
	if err != nil {
		t.Fatalf("marshalPayload: %v", err)
	}
	want := `{"repo":"acme/widget","branch":"","pull_request":0,"task_id":"","task_title":"","sender":"","instructions":"","constraints":null}`
	if got != want {
		t.Fatalf("unlabelled payload changed:\n got %s\nwant %s", got, want)
	}
	if strings.Contains(got, "workflow_id") {
		t.Fatalf("unlabelled payload contains workflow_id: %s", got)
	}
}

func TestValidateWorkflowID(t *testing.T) {
	tests := []struct {
		label string
		valid bool
	}{
		{label: "", valid: true},
		{label: "release", valid: true},
		{label: "release-42", valid: true},
		{label: "fable/dashboard-redesign", valid: true},
		{label: strings.Repeat("a", 64), valid: true},
		{label: "Release"},
		{label: "release_42"},
		{label: "-release"},
		{label: "release-"},
		{label: "release--42"},
		{label: "/x"},
		{label: "x/"},
		{label: "a//b"},
		{label: "a/b/c"},
		{label: strings.Repeat("a", 65)},
	}
	for _, test := range tests {
		err := ValidateWorkflowID(test.label)
		if test.valid && err != nil {
			t.Errorf("ValidateWorkflowID(%q): %v", test.label, err)
		}
		if !test.valid && err == nil {
			t.Errorf("ValidateWorkflowID(%q) accepted invalid label", test.label)
		}
	}
}

func TestWorkflowIDInheritedByDelegationAndComparedForIdempotency(t *testing.T) {
	request := (Engine{}).delegationRequest(
		db.Job{ID: "parent", Agent: "coord"},
		JobPayload{Repo: "acme/widget", WorkflowID: "release-42"},
		Delegation{ID: "child", Agent: "worker", Action: "ask", Prompt: "check"},
	)
	if request.WorkflowID != "release-42" {
		t.Fatalf("delegation WorkflowID = %q", request.WorkflowID)
	}
	payload := JobPayload{Repo: "acme/widget", WorkflowID: "release-42"}
	base := JobRequest{Repo: "acme/widget", WorkflowID: "release-42"}
	if !payloadMatchesRequest(payload, base) {
		t.Fatal("same workflow id did not match")
	}
	base.WorkflowID = "release-43"
	if payloadMatchesRequest(payload, base) {
		t.Fatal("different workflow ids were treated as idempotently equivalent")
	}
}

func TestMailboxRejectsInvalidWorkflowIDAndExternalJobPersistsValidID(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "acme/widget")
	mailbox := Mailbox{Store: store}
	_, err := mailbox.Enqueue(ctx, JobRequest{ID: "bad", Agent: "coord", Action: "ask", Repo: "acme/widget", WorkflowID: "Bad_ID"})
	if err == nil || !strings.Contains(err.Error(), "invalid workflow id") {
		t.Fatalf("invalid enqueue error = %v", err)
	}
	job, err := mailbox.OpenExternalJob(ctx, JobRequest{ID: "session", Agent: "coord", Action: "ask", Repo: "acme/widget", WorkflowID: "release-42"})
	if err != nil {
		t.Fatalf("OpenExternalJob: %v", err)
	}
	payload, err := unmarshalPayload(job.Payload)
	if err != nil || payload.WorkflowID != "release-42" {
		t.Fatalf("payload=%+v err=%v", payload, err)
	}
}

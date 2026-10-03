package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
)

func TestMessageFormalThreadsWithoutWorkflow(t *testing.T) {
	type formalMessage struct {
		ID         int64  `json:"id"`
		ThreadID   int64  `json:"thread_id"`
		Recipient  string `json:"to"`
		Kind       string `json:"kind"`
		WorkflowID string `json:"workflow"`
		Lifecycle  string `json:"lifecycle"`
	}
	home := directiveTestHome(t)
	invoke := func(role string, args ...string) (formalMessage, int) {
		t.Helper()
		flags := []string{"--home", home, "--role", role, "--json"}
		split := len(args)
		if args[0] == "escalate" {
			split = 1
		}
		if args[0] == "directive" && args[1] == "send" {
			split = 2
		}
		command := append([]string{}, args[:split]...)
		command = append(command, flags...)
		command = append(command, args[split:]...)
		var out, errOut bytes.Buffer
		code := runMessage(command, &out, &errOut)
		var message formalMessage
		if code == 0 {
			if err := json.Unmarshal(out.Bytes(), &message); err != nil {
				t.Fatalf("%v: %v output=%q", command, err, out.String())
			}
		}
		return message, code
	}
	must := func(role string, args ...string) formalMessage {
		t.Helper()
		m, code := invoke(role, args...)
		if code != 0 {
			t.Fatalf("%s %v failed: %d", role, args, code)
		}
		return m
	}
	ordinary := must("helper", "send", "peer", "A cross-tree conversation")
	if ordinary.Kind != "message" || ordinary.WorkflowID != "" || ordinary.Lifecycle != "" {
		t.Fatalf("ordinary message acquired formal duties: %+v", ordinary)
	}
	escalation := must("helper", "escalate", "Need a decision")
	if escalation.Recipient != "worker" || escalation.Kind != "escalation" || escalation.Lifecycle != "open" || escalation.WorkflowID != "" {
		t.Fatalf("escalation: %+v", escalation)
	}
	id := fmt.Sprint(escalation.ID)
	reply := must("worker", "reply", id, fmt.Sprintf("[org:escalate-resolved id=%s by=worker] resolved", id))
	if reply.ThreadID != escalation.ID {
		t.Fatalf("reply left original thread: %+v", reply)
	}
	if root := must("helper", "show", id); root.Lifecycle != "open" {
		t.Fatalf("conversation resolved a blocker: %+v", root)
	}
	if _, code := invoke("peer", "resolve", id, "--answer", "Unauthorized answer"); code == 0 {
		t.Fatal("unrelated role resolved escalation")
	}
	if _, code := invoke("worker", "resolve", id, "--note", fmt.Sprint(ordinary.ID)); code == 0 {
		t.Fatal("workflowless escalation cited an unrelated private conversation")
	}
	receipt := must("worker", "resolve", id, "--answer", "Proceed with the assigned change")
	repeated := must("worker", "resolve", id, "--answer", "Proceed with the assigned change")
	if receipt.ID != repeated.ID || receipt.ThreadID != escalation.ID || receipt.Recipient != "helper" {
		t.Fatalf("resolution replay or routing changed: first=%+v repeat=%+v", receipt, repeated)
	}
	if _, code := invoke("worker", "resolve", id, "--answer", "Different decision"); code == 0 {
		t.Fatal("conflicting resolution reported success")
	}
	if root := must("helper", "show", id); root.Lifecycle != "resolved" {
		t.Fatalf("resolution not visible after reopen: %+v", root)
	}
	directive := must("owner", "directive", "send", "--to", "worker", "Complete this assignment")
	did := fmt.Sprint(directive.ID)
	must("worker", "reply", did, fmt.Sprintf("[org:directive-done id=%s by=worker] completed", did))
	if root := must("owner", "show", did); root.Lifecycle != "pending" {
		t.Fatalf("reply acquired directive authority: %+v", root)
	}
	if _, code := invoke("owner", "directive", "done", did); code == 0 {
		t.Fatal("issuer self-certified completion")
	}
	if _, code := invoke("peer", "directive", "ack", did); code == 0 {
		t.Fatal("peer accepted another branch's directive")
	}
	must("worker", "directive", "ack", did)
	if root := must("owner", "show", did); root.Lifecycle != "accepted" {
		t.Fatalf("acceptance missing: %+v", root)
	}
	must("worker", "directive", "done", did)
	if root := must("owner", "show", did); root.Lifecycle != "completed" {
		t.Fatalf("completion missing: %+v", root)
	}
}

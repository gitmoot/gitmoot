package workflow

import (
	"strings"
	"testing"
)

// TestJobPayloadChatFieldsOmittedWhenEmpty proves the #534 additive fields
// (ThreadID / ChatMessageID) are omitempty: a non-chat job's marshaled payload
// carries NEITHER key, so every existing (non-chat) job serializes
// byte-identically to before the fields were added.
func TestJobPayloadChatFieldsOmittedWhenEmpty(t *testing.T) {
	encoded, err := marshalPayload(JobPayload{
		Repo:         "o/r",
		TaskID:       "t1",
		TaskTitle:    "title",
		Sender:       "session",
		Instructions: "do the thing",
	})
	if err != nil {
		t.Fatalf("marshalPayload returned error: %v", err)
	}
	if strings.Contains(encoded, "thread_id") {
		t.Fatalf("payload without a thread leaked a thread_id key: %s", encoded)
	}
	if strings.Contains(encoded, "chat_message_id") {
		t.Fatalf("payload without a chat message leaked a chat_message_id key: %s", encoded)
	}
}

// TestJobPayloadChatFieldsRoundTrip proves the fields ARE carried when a chat
// promotion sets them, and survive a marshal/parse round trip.
func TestJobPayloadChatFieldsRoundTrip(t *testing.T) {
	encoded, err := marshalPayload(JobPayload{Repo: "o/r", ThreadID: "chat-1", ChatMessageID: "msg-9"})
	if err != nil {
		t.Fatalf("marshalPayload returned error: %v", err)
	}
	if !strings.Contains(encoded, `"thread_id":"chat-1"`) || !strings.Contains(encoded, `"chat_message_id":"msg-9"`) {
		t.Fatalf("chat fields not marshaled: %s", encoded)
	}
	parsed, err := ParseJobPayload(encoded)
	if err != nil {
		t.Fatalf("ParseJobPayload returned error: %v", err)
	}
	if parsed.ThreadID != "chat-1" || parsed.ChatMessageID != "msg-9" {
		t.Fatalf("round trip lost chat fields: %+v", parsed)
	}
}

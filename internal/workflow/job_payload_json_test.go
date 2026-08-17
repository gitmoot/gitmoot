package workflow

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestJobPayloadJSONRoundTripPreservesUnknownFields(t *testing.T) {
	input := `{"repo":"owner/repo","exec_backend":"local","future_dispatch":{"mode":"isolated","limits":[1,2,3]}}`
	var payload JobPayload
	if err := json.Unmarshal([]byte(input), &payload); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("decode round trip: %v", err)
	}
	var want map[string]json.RawMessage
	if err := json.Unmarshal([]byte(input), &want); err != nil {
		t.Fatalf("decode input: %v", err)
	}
	if !reflect.DeepEqual(got["future_dispatch"], want["future_dispatch"]) {
		t.Fatalf("future_dispatch after round trip = %s, want %s; payload=%s", got["future_dispatch"], want["future_dispatch"], encoded)
	}
}

func TestJobPayloadJSONPreservesExplicitBlankExecBackendPresence(t *testing.T) {
	var payload JobPayload
	if err := json.Unmarshal([]byte(`{"repo":"owner/repo","exec_backend":""}`), &payload); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	value, present := payload.ExecBackendOverride()
	if !present || value != "" {
		t.Fatalf("ExecBackendOverride = %q, %v; want explicit blank", value, present)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &envelope); err != nil {
		t.Fatalf("decode round trip: %v", err)
	}
	if _, present := envelope["exec_backend"]; !present {
		t.Fatalf("explicit blank exec_backend disappeared on round trip: %s", encoded)
	}
}

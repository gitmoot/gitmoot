package workflow

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
)

type jobPayloadJSON JobPayload

var jobPayloadKnownJSONFields = func() map[string]struct{} {
	fields := make(map[string]struct{})
	typ := reflect.TypeOf(JobPayload{})
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if !field.IsExported() {
			continue
		}
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if name == "" {
			name = field.Name
		}
		if name != "-" {
			fields[name] = struct{}{}
		}
	}
	// unmarshalPayload recognizes these legacy aliases and rewrites them to the
	// canonical template_* fields. They are known inputs, not future members.
	for _, name := range []string{"preset_id", "preset_resolved_commit", "preset_content"} {
		fields[name] = struct{}{}
	}
	return fields
}()

// ExecBackendOverride returns the job-scoped selector and whether the payload
// explicitly supplied it. Presence is separate from value so `"exec_backend":""`
// cannot be mistaken for the absent-key local default.
func (p JobPayload) ExecBackendOverride() (string, bool) {
	return p.ExecBackend, p.execBackendPresent || p.ExecBackend != ""
}

// UnmarshalJSON retains unknown members so a newer payload can pass through an
// older daemon without losing fields that daemon does not understand.
func (p *JobPayload) UnmarshalJSON(data []byte) error {
	var decoded jobPayloadJSON
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*p = JobPayload(decoded)

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(data, &envelope); err != nil {
		return err
	}
	if envelope == nil {
		return nil
	}
	_, p.execBackendPresent = envelope["exec_backend"]
	for name := range jobPayloadKnownJSONFields {
		delete(envelope, name)
	}
	if len(envelope) != 0 {
		p.unknownJSONFields = envelope
	}
	return nil
}

// MarshalJSON appends retained unknown members to the normal struct encoding.
// Payloads without unknown fields use the alias bytes verbatim, preserving the
// established field order and byte-for-byte serialization.
func (p JobPayload) MarshalJSON() ([]byte, error) {
	encoded, err := json.Marshal(jobPayloadJSON(p))
	if err != nil {
		return nil, err
	}

	extra := make(map[string]json.RawMessage, len(p.unknownJSONFields)+1)
	for name, value := range p.unknownJSONFields {
		extra[name] = value
	}
	if p.execBackendPresent && p.ExecBackend == "" {
		extra["exec_backend"] = json.RawMessage(`""`)
	}
	if len(extra) == 0 {
		return encoded, nil
	}

	keys := make([]string, 0, len(extra))
	for name := range extra {
		keys = append(keys, name)
	}
	sort.Strings(keys)

	out := append([]byte(nil), encoded[:len(encoded)-1]...)
	needComma := len(encoded) > 2
	for _, name := range keys {
		if needComma {
			out = append(out, ',')
		}
		needComma = true
		encodedName, err := json.Marshal(name)
		if err != nil {
			return nil, err
		}
		out = append(out, encodedName...)
		out = append(out, ':')
		value := extra[name]
		if len(value) == 0 {
			value = json.RawMessage("null")
		}
		out = append(out, value...)
	}
	out = append(out, '}')
	return out, nil
}

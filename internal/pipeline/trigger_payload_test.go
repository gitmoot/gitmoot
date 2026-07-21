package pipeline

import (
	"strings"
	"testing"
)

func TestValidateAndEncodeTriggerPayload(t *testing.T) {
	tooMany := make(map[string]string)
	for i := 0; i < 33; i++ {
		tooMany["k"+string(rune('a'+i%26))+strings.Repeat("x", i/26)] = "v"
	}
	tests := []struct {
		name    string
		payload map[string]string
		want    string
		wantErr string
	}{
		{name: "valid canonical JSON", payload: map[string]string{"z": "last", "a": "first"}, want: `{"a":"first","z":"last"}`},
		{name: "bad key", payload: map[string]string{"Bad-Key": "v"}, wantErr: `payload key "Bad-Key" must be 1-64 bytes`},
		{name: "key too long", payload: map[string]string{"a" + strings.Repeat("x", 64): "v"}, wantErr: "must be 1-64 bytes"},
		{name: "value too big", payload: map[string]string{"body": strings.Repeat("x", (32<<10)+1)}, wantErr: `payload value for key "body" exceeds the 32 KiB limit`},
		{name: "too many entries", payload: tooMany, wantErr: "maximum is 32"},
		{name: "decoded total too big", payload: map[string]string{"a": strings.Repeat("a", 24<<10), "b": strings.Repeat("b", 24<<10)}, wantErr: "decoded key/value total exceeds the 48 KiB limit"},
		{name: "nul", payload: map[string]string{"body": "a\x00b"}, wantErr: "must not contain U+0000"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidateAndEncodeTriggerPayload(tc.payload)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %q, err=%v; want %q", got, err, tc.want)
			}
		})
	}
}

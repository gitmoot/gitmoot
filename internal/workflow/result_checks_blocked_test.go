package workflow

import (
	"encoding/json"
	"strings"
	"testing"
)

// #2314 asked for a machine-readable reason when a review could not run the
// project's checks, so the blocker table (toolchain, dependencies, sandbox, ...)
// can be counted from stored results instead of searched for in summaries. The
// result must ACCEPT the field, STORE it, and REJECT a value outside the enum,
// because a misspelling would otherwise be an uncountable row.
//
// It reads the stored value back through the JSON a job row persists, which is
// the surface anyone counting these reasons reads.
func TestChecksBlockedIsAcceptedStoredAndValidated(t *testing.T) {
	for _, tt := range []struct {
		name    string
		field   string
		want    string
		wantErr bool
	}{
		{name: "a declared reason is stored", field: `,"checks_blocked":"toolchain"`, want: `"checks_blocked":"toolchain"`},
		{name: "surrounding whitespace is trimmed", field: `,"checks_blocked":" dependencies "`, want: `"checks_blocked":"dependencies"`},
		{name: "omitting it stores nothing", field: ``, want: ``},
		{name: "a value outside the enum is rejected", field: `,"checks_blocked":"pytest"`, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			output := `{"gitmoot_result":{"decision":"approved","summary":"read the diff","evidence":"static_only"` + tt.field + `}}`
			result, err := extractAgentResultForAction(output, "review")
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "checks_blocked") {
					t.Fatalf("err = %v, want a checks_blocked validation error", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("extract: %v", err)
			}
			stored, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			if tt.want == "" {
				if strings.Contains(string(stored), "checks_blocked") {
					t.Fatalf("stored result %s carries checks_blocked the producer never sent", stored)
				}
				return
			}
			if !strings.Contains(string(stored), tt.want) {
				t.Fatalf("stored result %s does not carry %s", stored, tt.want)
			}
		})
	}
}

package workflow

import "testing"

func TestOrgDirectiveNoteSchemas(t *testing.T) {
	body := FormatOrgDirectiveNote("owner", "worker", "release/one", "run `x=y` ] then report")
	from, to, wf, directive, ok := ParseOrgDirectiveNote(body)
	if !ok || from != "owner" || to != "worker" || wf != "release/one" || directive != "run `x=y` ] then report" {
		t.Fatalf("ParseOrgDirectiveNote(%q) = (%q, %q, %q, %q, %v)", body, from, to, wf, directive, ok)
	}
	tests := []struct {
		name  string
		body  string
		parse func(string) (int64, string, bool)
	}{
		{"ack", FormatOrgDirectiveAckNote(42, "worker"), ParseOrgDirectiveAckNote},
		{"cancel", FormatOrgDirectiveCancelNote(42, "owner"), ParseOrgDirectiveCancelNote},
		{"done", FormatOrgDirectiveDoneNote(42, "worker"), ParseOrgDirectiveDoneNote},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			id, by, ok := test.parse(test.body)
			if !ok || id != 42 || by == "" {
				t.Fatalf("parse(%q) = (%d, %q, %v)", test.body, id, by, ok)
			}
		})
	}
}

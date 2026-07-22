package workflow

import "testing"

func TestFormatOrgHandoffNote(t *testing.T) {
	if got, want := FormatOrgHandoffNote("owner", "Finished release ] with follow-up"), "[org:handoff role=owner] Finished release ] with follow-up"; got != want {
		t.Fatalf("FormatOrgHandoffNote() = %q, want %q", got, want)
	}
	for _, test := range []struct {
		role string
		note string
	}{
		{note: "handoff"},
		{role: "owner", note: "  "},
		{role: "owner]", note: "handoff"},
		{role: "owner name", note: "handoff"},
	} {
		if got := FormatOrgHandoffNote(test.role, test.note); got != "" {
			t.Fatalf("FormatOrgHandoffNote(%q, %q) = %q, want empty", test.role, test.note, got)
		}
	}
}

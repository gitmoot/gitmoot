package workflow

import "testing"

func TestFormatParseOrgHandoffNote(t *testing.T) {
	body := FormatOrgHandoffNote("owner", "Finished release ] with follow-up")
	if got, want := body, "[org:handoff role=owner] Finished release ] with follow-up"; got != want {
		t.Fatalf("FormatOrgHandoffNote() = %q, want %q", got, want)
	}
	role, handoff, ok := ParseOrgHandoffNote(body)
	if !ok || role != "owner" || handoff != "Finished release ] with follow-up" {
		t.Fatalf("ParseOrgHandoffNote() = (%q, %q, %v)", role, handoff, ok)
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

func TestParseOrgHandoffNoteRejectsMalformedHeaders(t *testing.T) {
	for _, body := range []string{
		"[org:handoff role=owner]",
		"[org:handoff role=owner] ",
		"[org:handoff role=owner]handoff",
		"[org:handoff owner] handoff",
		"[org:handoff role=owner extra=x] handoff",
		"[org:handoff role=owner handoff",
		"[org:handoff role=owner name] handoff",
		"[org:escalate role=owner] handoff",
	} {
		if _, _, ok := ParseOrgHandoffNote(body); ok {
			t.Fatalf("ParseOrgHandoffNote(%q) unexpectedly succeeded", body)
		}
	}
}

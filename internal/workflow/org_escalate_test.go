package workflow

import "testing"

func TestFormatParseOrgEscalateNoteRoundTrip(t *testing.T) {
	body := FormatOrgEscalateNote("operator", "owner", "release/one", "Can this include ] and x=y?")
	if body != "[org:escalate to=owner from=operator wf=release/one] Can this include ] and x=y?" {
		t.Fatalf("FormatOrgEscalateNote = %q", body)
	}
	from, to, wf, question, ok := ParseOrgEscalateNote(body)
	if !ok || from != "operator" || to != "owner" || wf != "release/one" || question != "Can this include ] and x=y?" {
		t.Fatalf("ParseOrgEscalateNote = (%q, %q, %q, %q, %v)", from, to, wf, question, ok)
	}
}

func TestParseOrgEscalateNoteRejectsMalformedOrDuplicateKeys(t *testing.T) {
	for _, body := range []string{
		"[org:escalate to=owner from=operator wf=release/one]",
		"[org:escalate to=owner from=operator wf=release/one] ",
		"[org:escalate to=owner to=lead wf=release/one] question",
		"[org:escalate to=owner from=operator] question",
		"[org:escalate to=owner from=operator wf=release/one extra=x] question",
		"[org:escalate to=owner from=operator wf=release/one question",
		"[org:escalate to=owner from=operator wf=release/one]question",
	} {
		if _, _, _, _, ok := ParseOrgEscalateNote(body); ok {
			t.Fatalf("ParseOrgEscalateNote(%q) unexpectedly succeeded", body)
		}
	}
	if got := FormatOrgEscalateNote("operator]", "owner", "release/one", "question"); got != "" {
		t.Fatalf("FormatOrgEscalateNote accepted delimiter-bearing field: %q", got)
	}
}

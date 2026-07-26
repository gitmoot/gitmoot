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

func TestFormatParseOrgEscalateResolvedNoteRoundTrip(t *testing.T) {
	for _, test := range []struct {
		name         string
		answerNoteID int64
		want         string
	}{
		{name: "without answer note", want: "[org:escalate-resolved id=42 by=owner] resolved"},
		{name: "with answer note", answerNoteID: 77, want: "[org:escalate-resolved id=42 by=owner note=77] resolved"},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := FormatOrgEscalateResolvedNote(42, "owner", test.answerNoteID)
			if body != test.want {
				t.Fatalf("FormatOrgEscalateResolvedNote = %q, want %q", body, test.want)
			}
			escalationNoteID, resolvedBy, answerNoteID, ok := ParseOrgEscalateResolvedNote(body)
			if !ok || escalationNoteID != 42 || resolvedBy != "owner" || answerNoteID != test.answerNoteID {
				t.Fatalf("ParseOrgEscalateResolvedNote = (%d, %q, %d, %v)", escalationNoteID, resolvedBy, answerNoteID, ok)
			}
		})
	}
}

func TestParseOrgEscalateResolvedNoteRejectsMalformedOrDuplicateKeys(t *testing.T) {
	for _, body := range []string{
		"[org:escalate-resolved id=42 by=owner]",
		"[org:escalate-resolved id=42 by=owner] pending",
		"[org:escalate-resolved by=owner] resolved",
		"[org:escalate-resolved id=42] resolved",
		"[org:escalate-resolved id=42 id=43 by=owner] resolved",
		"[org:escalate-resolved id=42 by=owner by=review] resolved",
		"[org:escalate-resolved id=42 by=owner note=77 note=78] resolved",
		"[org:escalate-resolved id=nope by=owner] resolved",
		"[org:escalate-resolved id=0 by=owner] resolved",
		"[org:escalate-resolved id=42 by=owner note=nope] resolved",
		"[org:escalate-resolved id=42 by=owner note=0] resolved",
		"[org:escalate-resolved id=42 by=owner extra=value] resolved",
		"[org:escalate-resolved id=42 by=owner note=77 extra=value] resolved",
		"[org:escalate-resolved id=42 by=owner resolved",
	} {
		if _, _, _, ok := ParseOrgEscalateResolvedNote(body); ok {
			t.Fatalf("ParseOrgEscalateResolvedNote(%q) unexpectedly succeeded", body)
		}
	}
	for _, test := range []struct {
		id   int64
		by   string
		note int64
	}{
		{id: 0, by: "owner"},
		{id: -1, by: "owner"},
		{id: 42, by: ""},
		{id: 42, by: "bad role"},
	} {
		if got := FormatOrgEscalateResolvedNote(test.id, test.by, test.note); got != "" {
			t.Fatalf("FormatOrgEscalateResolvedNote(%d, %q, %d) = %q, want empty", test.id, test.by, test.note, got)
		}
	}
}

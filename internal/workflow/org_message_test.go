package workflow

import "testing"

func TestOrgMessageNoteRoundTrip(t *testing.T) {
	body := FormatOrgMessageNote("gm-omp-nag", "gm-omp-impl", "gitmoot/1692", "heads up: [internal/cli/org.go] overlaps")
	from, to, workflowID, message, ok := ParseOrgMessageNote(body)
	if !ok || from != "gm-omp-nag" || to != "gm-omp-impl" || workflowID != "gitmoot/1692" || message != "heads up: [internal/cli/org.go] overlaps" {
		t.Fatalf("parsed message=(from=%q to=%q workflow=%q message=%q ok=%v)", from, to, workflowID, message, ok)
	}
	for _, invalid := range []string{
		FormatOrgMessageNote("", "gm-omp-impl", "gitmoot/1692", "message"),
		FormatOrgMessageNote("gm-omp-nag", "", "gitmoot/1692", "message"),
		FormatOrgMessageNote("gm-omp-nag", "gm-omp-impl", "", "message"),
		FormatOrgMessageNote("gm-omp-nag", "gm-omp-impl", "gitmoot/1692", "   "),
	} {
		if invalid != "" {
			t.Fatalf("invalid message formatted as %q", invalid)
		}
	}
}

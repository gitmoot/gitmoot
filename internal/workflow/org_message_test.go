package workflow

import "testing"

func TestHistoricalOrgMessageRemainsReadable(t *testing.T) {
	body := "[org:message to=gm-omp-impl from=gm-omp-nag wf=gitmoot/1692] heads up: [internal/cli/org.go] overlaps"
	from, to, workflowID, message, ok := ParseOrgMessageNote(body)
	if !ok || from != "gm-omp-nag" || to != "gm-omp-impl" || workflowID != "gitmoot/1692" || message != "heads up: [internal/cli/org.go] overlaps" {
		t.Fatalf("parsed message=(from=%q to=%q workflow=%q message=%q ok=%v)", from, to, workflowID, message, ok)
	}
}

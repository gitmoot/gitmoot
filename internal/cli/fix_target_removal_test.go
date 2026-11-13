package cli

import (
	"bytes"
	"strings"
	"testing"
)

// #2586: fix-target routing is gone, so `agent review` no longer knows --lead,
// while --no-fix-target is still accepted (and ignored) for old callers.
func TestAgentReviewRejectsLeadAndAcceptsNoFixTarget(t *testing.T) {
	var stderr bytes.Buffer
	if _, ok := parseAgentRunOptions("review", []string{"reviewer", "Review it.", "--lead", "implementer"}, &stderr); ok {
		t.Fatal("agent review accepted --lead")
	}
	if !strings.Contains(stderr.String(), `unknown agent review flag "--lead"`) {
		t.Fatalf("stderr = %q, want unknown agent review flag \"--lead\"", stderr.String())
	}

	stderr.Reset()
	options, ok := parseAgentRunOptions("review", []string{"reviewer", "Review it.", "--no-fix-target", "--pr", "7"}, &stderr)
	if !ok {
		t.Fatalf("agent review rejected --no-fix-target: %q", stderr.String())
	}
	if options.prNumber != 7 {
		t.Fatalf("prNumber = %d, want 7: --no-fix-target must not consume the next argument", options.prNumber)
	}
}

func TestReviewRequestRejectsLead(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runReviewRequest([]string{"--pr", "7", "--lead", "implementer"}, &stdout, &stderr); code == 0 {
		t.Fatalf("review request accepted --lead: stdout=%q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "flag provided but not defined: -lead") {
		t.Fatalf("stderr = %q, want flag provided but not defined: -lead", stderr.String())
	}
}

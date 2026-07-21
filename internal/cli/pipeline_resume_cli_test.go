package cli

import (
	"bytes"
	"strings"
	"testing"
)

// TestPipelineResumeCancelCLI smoke-tests the resume/cancel CLI wrappers through
// Run(): argument validation and the error/exit-code contract.
func TestPipelineResumeCancelCLI(t *testing.T) {
	home := t.TempDir()
	run := func(args ...string) (string, string, int) {
		var stdout, stderr bytes.Buffer
		code := Run(append(args, "--home", home), &stdout, &stderr)
		return stdout.String(), stderr.String(), code
	}
	// Missing run id: a usage error BEFORE any store touch, so it is safe to invoke
	// without --home (the arg guard returns before withStore).
	bare := func(args ...string) (string, int) {
		var stdout, stderr bytes.Buffer
		code := Run(args, &stdout, &stderr)
		return stderr.String(), code
	}

	if errOut, code := bare("pipeline", "resume"); code != 2 || !strings.Contains(errOut, "requires a run id") {
		t.Fatalf("resume no-arg: code=%d stderr=%q", code, errOut)
	}
	if errOut, code := bare("pipeline", "cancel"); code != 2 || !strings.Contains(errOut, "requires a run id") {
		t.Fatalf("cancel no-arg: code=%d stderr=%q", code, errOut)
	}
	// Unknown run: a friendly not-found with exit 1.
	if _, errOut, code := run("pipeline", "resume", "prun-nope"); code != 1 || !strings.Contains(errOut, "not found") {
		t.Fatalf("resume unknown: code=%d stderr=%q", code, errOut)
	}
	if _, errOut, code := run("pipeline", "cancel", "prun-nope"); code != 1 || !strings.Contains(errOut, "not found") {
		t.Fatalf("cancel unknown: code=%d stderr=%q", code, errOut)
	}
}

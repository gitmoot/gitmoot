package cli

import (
	"bytes"
	"strings"
	"testing"
)

// TestPipelineRunRequiresRepo proves `pipeline run` refuses a pipeline with no
// repo: its stage jobs need a managed repo for a worker to claim them.
func TestPipelineRunRequiresRepo(t *testing.T) {
	home := t.TempDir()
	run := func(args ...string) (string, string, int) {
		var stdout, stderr bytes.Buffer
		code := Run(append(args, "--home", home), &stdout, &stderr)
		return stdout.String(), stderr.String(), code
	}
	spec := writeSpec(t, "name: norepo\nstages:\n  - id: a\n    cmd: echo a\n")
	if _, errOut, code := run("pipeline", "add", spec); code != 0 {
		t.Fatalf("add exit=%d stderr=%s", code, errOut)
	}
	_, errOut, code := run("pipeline", "run", "norepo")
	if code != 1 || !strings.Contains(errOut, "has no repo") {
		t.Fatalf("run no-repo: code=%d stderr=%q", code, errOut)
	}
}

// TestPipelineRunOverlapGuardAndShow proves the manual run path: a run prints its
// id, a second run while the first is active is refused (one active run per
// pipeline), and `pipeline show <run-id>` renders the run funnel while
// `show <name>` still renders the registry view.
func TestPipelineRunOverlapGuardAndShow(t *testing.T) {
	home := t.TempDir()
	run := func(args ...string) (string, string, int) {
		var stdout, stderr bytes.Buffer
		code := Run(append(args, "--home", home), &stdout, &stderr)
		return stdout.String(), stderr.String(), code
	}
	spec := writeSpec(t, "name: flow\nrepo: owner/repo\nstages:\n  - id: a\n    cmd: echo a\n")
	if _, errOut, code := run("pipeline", "add", spec); code != 0 {
		t.Fatalf("add exit=%d stderr=%s", code, errOut)
	}

	out, errOut, code := run("pipeline", "run", "flow")
	if code != 0 {
		t.Fatalf("run exit=%d stderr=%s", code, errOut)
	}
	runID := strings.TrimSpace(out)
	if !strings.HasPrefix(runID, "prun-flow-") {
		t.Fatalf("run id = %q, want a prun-flow- id", runID)
	}

	// Overlap guard: the first run is still running, so a second is refused.
	if _, errOut, code := run("pipeline", "run", "flow"); code != 1 || !strings.Contains(errOut, "already has an active run") {
		t.Fatalf("overlap run: code=%d stderr=%q", code, errOut)
	}

	// show <run-id>: the run funnel.
	funnel, errOut, code := run("pipeline", "show", runID)
	if code != 0 {
		t.Fatalf("show run exit=%d stderr=%s", code, errOut)
	}
	if !strings.Contains(funnel, "run: "+runID) || !strings.Contains(funnel, "state: running") || !strings.Contains(funnel, "a QUEUED") {
		t.Fatalf("run funnel = %q", funnel)
	}

	// show <name>: the registry view still wins for a pipeline name.
	regView, _, code := run("pipeline", "show", "flow")
	if code != 0 || !strings.Contains(regView, "name: flow") {
		t.Fatalf("show name view = %q code=%d", regView, code)
	}

	// A totally unknown identifier is a friendly not-found.
	if _, errOut, code := run("pipeline", "show", "prun-nope-1"); code != 1 || !strings.Contains(errOut, "not found") {
		t.Fatalf("show unknown: code=%d stderr=%q", code, errOut)
	}
}

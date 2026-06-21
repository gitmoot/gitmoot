package subprocess

import (
	"bytes"
	"context"
	"runtime"
	"strings"
	"testing"
)

func TestRunCapturesStdout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell command differs on windows")
	}

	result, err := Run(context.Background(), "", "sh", "-c", "printf gitmoot")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if strings.TrimSpace(result.Stdout) != "gitmoot" {
		t.Fatalf("stdout = %q", result.Stdout)
	}
}

func TestRunStreamTeesAndBuffers(t *testing.T) {
	var tee bytes.Buffer
	result, err := RunStream(context.Background(), "", &tee, "sh", "-c", "echo out; echo err >&2")
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	if result.Stdout != "out\n" || result.Stderr != "err\n" {
		t.Fatalf("buffered result = %+v", result)
	}
	teed := tee.String()
	if !strings.Contains(teed, "out\n") || !strings.Contains(teed, "err\n") {
		t.Fatalf("tee missing streams: %q", teed)
	}
}

func TestRunStreamNilWriterDegradesToRun(t *testing.T) {
	result, err := RunStream(context.Background(), "", nil, "sh", "-c", "echo ok")
	if err != nil {
		t.Fatalf("RunStream nil: %v", err)
	}
	if result.Stdout != "ok\n" {
		t.Fatalf("result = %+v", result)
	}
}

func TestRunStreamInterleavesAtLineBoundaries(t *testing.T) {
	var tee bytes.Buffer
	_, err := RunStream(context.Background(), "", &tee, "sh", "-c", "printf 'partial'; sleep 0.05; printf ' line\\n'")
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	if got := tee.String(); got != "partial line\n" {
		t.Fatalf("line not assembled before forwarding: %q", got)
	}
}

// TestTeeRunnerTeesAndReturnsResult: a TeeRunner's plain Run tees the child's
// output to Out while returning the same buffered Result — so an adapter that
// only calls .Run() streams live into the log with no change to result capture.
func TestTeeRunnerTeesAndReturnsResult(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell command differs on windows")
	}
	var tee bytes.Buffer
	runner := TeeRunner{Inner: GroupRunner{}, Out: &tee}
	result, err := runner.Run(context.Background(), "", "sh", "-c", "echo out; echo err >&2")
	if err != nil {
		t.Fatalf("TeeRunner.Run: %v", err)
	}
	if result.Stdout != "out\n" || result.Stderr != "err\n" {
		t.Fatalf("buffered result = %+v", result)
	}
	teed := tee.String()
	if !strings.Contains(teed, "out\n") || !strings.Contains(teed, "err\n") {
		t.Fatalf("tee missing streams: %q", teed)
	}
}

// TestTeeRunnerNilOutDegradesToRun: a nil Out leaves behavior byte-identical to
// the inner runner's plain Run — the tee is opt-in via a non-nil writer.
func TestTeeRunnerNilOutDegradesToRun(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell command differs on windows")
	}
	runner := TeeRunner{Inner: GroupRunner{}}
	result, err := runner.Run(context.Background(), "", "sh", "-c", "echo ok")
	if err != nil {
		t.Fatalf("TeeRunner.Run nil out: %v", err)
	}
	if result.Stdout != "ok\n" {
		t.Fatalf("result = %+v", result)
	}
}

// TestTeeRunnerDefaultsToGroupRunner: a zero Inner defaults to GroupRunner{}, so
// the tee keeps the process-group kill semantics — proven by a nil-inner runner
// streaming output correctly via the group stream path.
func TestTeeRunnerDefaultsToGroupRunner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell command differs on windows")
	}
	var tee bytes.Buffer
	runner := TeeRunner{Out: &tee} // Inner nil -> GroupRunner{}
	result, err := runner.Run(context.Background(), "", "sh", "-c", "echo grouped")
	if err != nil {
		t.Fatalf("TeeRunner.Run default inner: %v", err)
	}
	if strings.TrimSpace(result.Stdout) != "grouped" {
		t.Fatalf("result = %+v", result)
	}
	if !strings.Contains(tee.String(), "grouped") {
		t.Fatalf("tee missing output: %q", tee.String())
	}
}

// TestTeeRunnerLookPathDelegates: LookPath passes through to the inner runner so
// runtime resolution (and the GroupRunner default) behaves identically.
func TestTeeRunnerLookPathDelegates(t *testing.T) {
	runner := TeeRunner{Inner: GroupRunner{}}
	got, err := runner.LookPath("sh")
	if err != nil {
		t.Fatalf("LookPath: %v", err)
	}
	want, err := GroupRunner{}.LookPath("sh")
	if err != nil {
		t.Fatalf("GroupRunner.LookPath: %v", err)
	}
	if got != want {
		t.Fatalf("LookPath = %q, want %q", got, want)
	}
}

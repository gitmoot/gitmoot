package subprocess

import (
	"bytes"
	"context"
	"runtime"
	"strconv"
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

func TestExecRunnerRunExactEnvDoesNotInheritHost(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell command differs on windows")
	}

	t.Setenv("GITMOOT_EXACT_ENV_AMBIENT", "must-not-leak")
	var stdout, stderr bytes.Buffer
	err := (ExecRunner{}).RunExactEnv(
		context.Background(),
		"",
		[]string{"GITMOOT_EXACT_ENV_VALUE=exact"},
		&stdout,
		&stderr,
		"sh",
		"-c",
		`printf '%s|%s' "$GITMOOT_EXACT_ENV_VALUE" "${GITMOOT_EXACT_ENV_AMBIENT-unset}"`,
	)
	if err != nil {
		t.Fatalf("RunExactEnv returned error: %v", err)
	}
	if stdout.String() != "exact|unset" {
		t.Fatalf("stdout = %q, want exact environment without ambient variables", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestGroupRunnerPIDCaptureReportsStartedProcess(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("PID assertion uses a POSIX shell")
	}
	var captured int
	runner := GroupRunner{}
	result, err := runner.RunWithPID(context.Background(), "", func(pid int) {
		captured = pid
	}, "sh", "-c", `printf %s "$$"`)
	if err != nil {
		t.Fatalf("RunWithPID returned error: %v", err)
	}
	reported, err := strconv.Atoi(strings.TrimSpace(result.Stdout))
	if err != nil {
		t.Fatalf("parse child PID from stdout %q: %v", result.Stdout, err)
	}
	if captured <= 0 || captured != reported {
		t.Fatalf("captured PID = %d, child reported %d", captured, reported)
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

func TestTeeRunnerRunEnvTeesAndInjects(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell command differs on windows")
	}
	var tee bytes.Buffer
	runner := TeeRunner{Inner: GroupRunner{}, Out: &tee}
	result, err := runner.RunEnv(context.Background(), "", []string{"GITMOOT_TRIGGER_BODY=first\n第二"}, "sh", "-c", `printf '%s' "$GITMOOT_TRIGGER_BODY"`)
	if err != nil {
		t.Fatalf("TeeRunner.RunEnv: %v", err)
	}
	if result.Stdout != "first\n第二" || !strings.Contains(tee.String(), "first\n第二") {
		t.Fatalf("result=%+v tee=%q", result, tee.String())
	}
}

func TestTeeRunnerEnvInjectingRunnerStreamsEnvironmentAndPID(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell command differs on windows")
	}

	newRunner := func(out *bytes.Buffer) TeeRunner {
		return TeeRunner{
			Inner: EnvInjectingRunner{
				Inner: GroupRunner{},
				Env: []string{
					"GITMOOT_INJECTED=wrapper",
					"GITMOOT_SHARED=wrapper",
				},
			},
			Out: out,
		}
	}
	callEnv := []string{
		"GITMOOT_CALL_ENV=call",
		"GITMOOT_SHARED=call",
	}

	t.Run("RunEnv", func(t *testing.T) {
		var tee bytes.Buffer
		result, err := newRunner(&tee).RunEnv(
			context.Background(),
			"",
			callEnv,
			"sh",
			"-c",
			`printf '%s' "$GITMOOT_INJECTED:$GITMOOT_CALL_ENV:$GITMOOT_SHARED"`,
		)
		if err != nil {
			t.Fatalf("TeeRunner.RunEnv: %v", err)
		}
		const want = "wrapper:call:call"
		if result.Stdout != want || strings.TrimSuffix(tee.String(), "\n") != want {
			t.Fatalf("stdout=%q tee=%q, want %q", result.Stdout, tee.String(), want)
		}
	})

	t.Run("RunEnvWithPID", func(t *testing.T) {
		var (
			tee      bytes.Buffer
			captured int
		)
		result, err := newRunner(&tee).RunEnvWithPID(
			context.Background(),
			"",
			callEnv,
			func(pid int) { captured = pid },
			"sh",
			"-c",
			`printf '%s' "$GITMOOT_INJECTED:$GITMOOT_CALL_ENV:$GITMOOT_SHARED:$$"`,
		)
		if err != nil {
			t.Fatalf("TeeRunner.RunEnvWithPID: %v", err)
		}
		fields := strings.Split(result.Stdout, ":")
		if len(fields) != 4 {
			t.Fatalf("stdout=%q, want injected env and child PID", result.Stdout)
		}
		if got := strings.Join(fields[:3], ":"); got != "wrapper:call:call" {
			t.Fatalf("injected env = %q, want %q", got, "wrapper:call:call")
		}
		reported, err := strconv.Atoi(fields[3])
		if err != nil {
			t.Fatalf("parse child PID from stdout %q: %v", result.Stdout, err)
		}
		if captured <= 0 || captured != reported {
			t.Fatalf("captured PID = %d, child reported %d", captured, reported)
		}
		if strings.TrimSuffix(tee.String(), "\n") != result.Stdout {
			t.Fatalf("tee=%q, want stdout %q", tee.String(), result.Stdout)
		}
	})
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

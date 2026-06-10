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

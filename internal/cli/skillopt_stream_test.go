package cli

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/subprocess"
)

// streamRecordingRunner implements subprocess.StreamRunner and records which
// entry point the optimizer launch used.
type streamRecordingRunner struct {
	streamed bool
	ran      bool
	wroteTo  io.Writer
	result   subprocess.Result
}

func (r *streamRecordingRunner) Run(ctx context.Context, dir string, command string, args ...string) (subprocess.Result, error) {
	r.ran = true
	return r.result, nil
}

func (r *streamRecordingRunner) RunStream(ctx context.Context, dir string, out io.Writer, command string, args ...string) (subprocess.Result, error) {
	r.streamed = true
	r.wroteTo = out
	if out != nil {
		_, _ = out.Write([]byte("[1/6 FEEDBACK-DIRECT] live line\n"))
	}
	return r.result, nil
}

func (r *streamRecordingRunner) LookPath(file string) (string, error) { return file, nil }

func TestRunSkillOptTrainOptimizerStreamsWhenSupported(t *testing.T) {
	previous := skillOptTrainOptimizerRunner
	runner := &streamRecordingRunner{result: subprocess.Result{Stdout: "done"}}
	skillOptTrainOptimizerRunner = runner
	defer func() { skillOptTrainOptimizerRunner = previous }()

	var progress bytes.Buffer
	paths := skillOptTrainOptimizerPaths{OutRoot: t.TempDir()}
	result, err := runSkillOptTrainOptimizer(context.Background(), &progress, paths, skillOptTrainOptimizerRequest{}, "gitmoot-skillopt", []string{"optimize"})
	if err != nil {
		t.Fatalf("runSkillOptTrainOptimizer: %v", err)
	}
	if !runner.streamed || runner.ran {
		t.Fatalf("expected the streaming entry point; streamed=%v ran=%v", runner.streamed, runner.ran)
	}
	if !strings.Contains(progress.String(), "live line") {
		t.Fatalf("progress writer did not receive the stream: %q", progress.String())
	}
	if result.Stdout != "done" {
		t.Fatalf("buffered result lost: %+v", result)
	}

	// A nil progress writer (or a Runner without streaming) uses Run.
	runner.streamed, runner.ran = false, false
	if _, err := runSkillOptTrainOptimizer(context.Background(), nil, paths, skillOptTrainOptimizerRequest{}, "gitmoot-skillopt", []string{"optimize"}); err != nil {
		t.Fatalf("nil-progress run: %v", err)
	}
	if runner.streamed || !runner.ran {
		t.Fatalf("nil progress must use Run; streamed=%v ran=%v", runner.streamed, runner.ran)
	}
}

func TestOptimizerMetadataRecordsBackendAndModel(t *testing.T) {
	metadata := map[string]any{}
	addSkillOptTrainOptimizerConfigMetadata(metadata, skillOptTrainOptimizerRequest{
		OptimizerBackend: "claude",
		OptimizerModel:   "claude-sonnet-4-6",
	})
	if metadata["run_optimizer_backend"] != "claude" || metadata["run_optimizer_model"] != "claude-sonnet-4-6" {
		t.Fatalf("metadata = %v", metadata)
	}
	// The combined Backend/Model fields are the fallback identity.
	metadata = map[string]any{}
	addSkillOptTrainOptimizerConfigMetadata(metadata, skillOptTrainOptimizerRequest{Backend: "codex", Model: "gpt-4o"})
	if metadata["run_optimizer_backend"] != "codex" || metadata["run_optimizer_model"] != "gpt-4o" {
		t.Fatalf("fallback metadata = %v", metadata)
	}
}

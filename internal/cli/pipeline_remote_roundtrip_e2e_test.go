package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/agenttemplate"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/pipeline"
)

func usePipelineFakeGitHubBackend(t *testing.T, backend *fakeGitHubBackend) {
	t.Helper()
	previousClient := newPipelineRemoteClient
	previousSource := newPipelineRemoteSource
	newPipelineRemoteClient = func() pipelineRemoteClient {
		return &github.GhClient{Runner: backend, MaxRetries: 1}
	}
	newPipelineRemoteSource = func() agenttemplate.RemoteSource {
		return agenttemplate.GHFetcher{Runner: backend}
	}
	t.Cleanup(func() {
		newPipelineRemoteSource = previousSource
		newPipelineRemoteClient = previousClient
	})
}

func pipelineRemoteWritePaths(backend *fakeGitHubBackend) []string {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	paths := []string{}
	for _, call := range backend.calls {
		if !strings.Contains(call, " -X PUT ") && !strings.Contains(call, " -X DELETE ") {
			continue
		}
		paths = append(paths, contentsPath(strings.Fields(strings.TrimPrefix(call, "gh "))))
	}
	return paths
}

func backendHasCall(backend *fakeGitHubBackend, fragment string) bool {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	for _, call := range backend.calls {
		if strings.Contains(call, fragment) {
			return true
		}
	}
	return false
}

// TestPipelinePublishPullRoundTripThroughSharedBackend is the GitHub transport
// counterpart to TestPipelineBundleRoundTripE2E. One in-memory gh backend is
// shared by home A's publish and home B's list/pull, while the imported pipeline
// still runs through the real #935 import and shell-worker paths.
func TestPipelinePublishPullRoundTripThroughSharedBackend(t *testing.T) {
	ctx := context.Background()
	backend := newFakeGitHubBackend()
	usePipelineFakeGitHubBackend(t, backend)

	homeA, _, storeA := heartbeatLoopE2EHome(t)
	checkoutA := pipelineBundleCheckout(t, "https://github.com/source/project.git")
	seedDaemonWorkerRepo(t, storeA, "source/project", checkoutA)

	const (
		catalogRepo = "jerry/pipeline-catalog"
		templateID  = "bundle-reviewer"
	)
	templateContent := bundleTemplateContent(templateID, "Review the pipeline result carefully.\n")
	installLocalTemplate(t, homeA, templateID, templateContent)
	agentCommand := pipelineStageResultCmd("approved", "agent reviewed", nil)
	subscribePipelineBundleShellAgent(t, homeA, "exported-reviewer", "source/project", templateID, agentCommand)

	cmd := "test -d /tmp && " + pipelineStageResultCmd("approved", "command collected", nil)
	specYAML := "# shared pipeline comment\nname: share-flow # preserve-name-comment\ndescription: Portable review flow.\nrepo: source/project # preserve-repo-comment\n" +
		"trigger:\n  kind: email\n  connection: shared-imap\n" +
		"stages:\n  # preserve-stage-comment\n" +
		pipelineE2EStage("collect", cmd, "") +
		"  - id: review\n    agent: exported-reviewer\n    prompt: Review collected output.\n    needs: [collect]\n"
	specFile := writeSpec(t, specYAML)
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"pipeline", "add", specFile, "--home", homeA}, &stdout, &stderr); code != 0 {
		t.Fatalf("pipeline add exit=%d stderr=%s", code, stderr.String())
	}
	const secretMarker = "DO-NOT-PUBLISH-BINDING-CREDENTIAL"
	if err := storeA.SetPipelineTriggerBinding(ctx, "share-flow", `{"flow_id":"`+secretMarker+`","binding_id":"local-only"}`); err != nil {
		t.Fatal(err)
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"pipeline", "remote", "set", catalogRepo, "--home", homeA}, &stdout, &stderr); code != 0 {
		t.Fatalf("home A remote set exit=%d stderr=%s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"pipeline", "publish", "share-flow", "--home", homeA, "--create"}, &stdout, &stderr); code != 0 {
		t.Fatalf("publish exit=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "created "+catalogRepo+" (private)") || !strings.Contains(stdout.String(), "published pipeline share-flow -> "+catalogRepo+"/pipelines/share-flow") {
		t.Fatalf("publish output missing create/path:\nstdout=%s\nstderr=%s", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "prompts are pushed verbatim") {
		t.Fatalf("publish missing verbatim-prompt warning:\n%s", stderr.String())
	}
	if !backendHasCall(backend, "repo create "+catalogRepo+" --private --add-readme") {
		t.Fatalf("publish --create did not create a private repo; calls=%v", backend.calls)
	}

	wantRemotePaths := []string{
		"pipelines/share-flow/bundle.yaml",
		"pipelines/share-flow/spec.yaml",
		"pipelines/share-flow/templates/" + templateID + ".md",
	}
	backend.mu.Lock()
	gotRemotePaths := make([]string, 0, len(backend.files[catalogRepo]))
	for path, content := range backend.files[catalogRepo] {
		gotRemotePaths = append(gotRemotePaths, path)
		if bytes.Contains(content, []byte(secretMarker)) || bytes.Contains(content, []byte("trigger_binding")) {
			backend.mu.Unlock()
			t.Fatalf("remote file %s contains trigger binding or credential material", path)
		}
	}
	backend.mu.Unlock()
	sort.Strings(gotRemotePaths)
	if !reflect.DeepEqual(gotRemotePaths, wantRemotePaths) {
		t.Fatalf("remote layout = %v, want %v", gotRemotePaths, wantRemotePaths)
	}

	homeB, _, storeB := heartbeatLoopE2EHome(t)
	checkoutB := pipelineBundleCheckout(t, "https://github.com/target/project.git")
	seedDaemonWorkerRepo(t, storeB, "target/project", checkoutB)
	installLocalTemplate(t, homeB, templateID, templateContent)
	subscribePipelineBundleShellAgent(t, homeB, "local-reviewer", "target/project", templateID, agentCommand)
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"pipeline", "remote", "set", catalogRepo, "--home", homeB}, &stdout, &stderr); code != 0 {
		t.Fatalf("home B remote set exit=%d stderr=%s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"pipeline", "pull", "--list", "--home", homeB}, &stdout, &stderr); code != 0 {
		t.Fatalf("pull --list exit=%d stderr=%s", code, stderr.String())
	}
	for _, want := range []string{"share-flow", "Portable review flow.", "requirements: runtimes=shell", "connections=email/shared-imap"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("pull --list missing %q:\n%s", want, stdout.String())
		}
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"pipeline", "pull", "share-flow", "--home", homeB, "--repo", "target/project", "--agent-map", "exported-reviewer=local-reviewer"}, &stdout, &stderr); code != 0 {
		t.Fatalf("pipeline pull exit=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{"Requirements report:", "runtime shell: present", "connection email/shared-imap: unchecked", "imported pipeline share-flow (disabled"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("pull stdout missing %q:\n%s", want, stdout.String())
		}
	}
	stored, found, err := storeB.GetPipeline(ctx, "share-flow")
	if err != nil || !found {
		t.Fatalf("GetPipeline found=%v err=%v", found, err)
	}
	if stored.Enabled {
		t.Fatal("pulled pipeline must be disabled by default")
	}
	storedSpec, err := pipeline.Load([]byte(stored.SpecYAML))
	if err != nil {
		t.Fatal(err)
	}
	if stored.Repo != "target/project" || storedSpec.Repo != "target/project" || storedSpec.Stages[1].Agent != "local-reviewer" {
		t.Fatalf("stored pipeline repo=%q spec=%+v", stored.Repo, storedSpec)
	}
	for _, comment := range []string{"# shared pipeline comment", "# preserve-name-comment", "# preserve-stage-comment"} {
		if !strings.Contains(stored.SpecYAML, comment) {
			t.Fatalf("pulled spec lost comment %q:\n%s", comment, stored.SpecYAML)
		}
	}

	t.Run("unknown name lists available", func(t *testing.T) {
		var out, errBuf bytes.Buffer
		code := Run([]string{"pipeline", "pull", "missing-flow", "--home", homeB, "--repo", "target/project"}, &out, &errBuf)
		if code == 0 || !strings.Contains(errBuf.String(), `unknown pipeline "missing-flow"`) || !strings.Contains(errBuf.String(), "available: share-flow") {
			t.Fatalf("code=%d stdout=%s stderr=%s", code, out.String(), errBuf.String())
		}
	})
	t.Run("collision without force", func(t *testing.T) {
		var out, errBuf bytes.Buffer
		code := Run([]string{"pipeline", "pull", "share-flow", "--home", homeB, "--repo", "target/project", "--agent-map", "exported-reviewer=local-reviewer"}, &out, &errBuf)
		if code == 0 || !strings.Contains(errBuf.String(), `pipeline "share-flow" already exists`) {
			t.Fatalf("code=%d stdout=%s stderr=%s", code, out.String(), errBuf.String())
		}
	})

	if err := storeB.SetPipelineEnabled(ctx, "share-flow", true); err != nil {
		t.Fatalf("enable pulled pipeline: %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"pipeline", "run", "share-flow", "--home", homeB}, &stdout, &stderr); code != 0 {
		t.Fatalf("pipeline run exit=%d stderr=%s", code, stderr.String())
	}
	runID := strings.TrimSpace(stdout.String())
	enqueue := newPipelineStageEnqueuer(storeB, homeB)
	worker := defaultJobWorker(storeB, io.Discard, homeB)
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 8; i++ {
		if err := runEnabledRepoWorkerTicksTracked(ctx, storeB, worker, 1, "", io.Discard, now, nil, nil); err != nil {
			t.Fatalf("worker tick %d: %v", i, err)
		}
		if err := runPipelineScanOnce(ctx, storeB, enqueue, now); err != nil {
			t.Fatalf("pipeline scan %d: %v", i, err)
		}
		run, _, err := storeB.GetPipelineRun(ctx, runID)
		if err != nil {
			t.Fatal(err)
		}
		if run.State != pipeline.RunRunning {
			break
		}
	}
	run, found, err := storeB.GetPipelineRun(ctx, runID)
	if err != nil || !found || run.State != pipeline.RunSucceeded {
		t.Fatalf("pulled pipeline run=%+v found=%v err=%v", run, found, err)
	}

	initialWrites := pipelineRemoteWritePaths(backend)
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"pipeline", "publish", "share-flow", "--home", homeA}, &stdout, &stderr); code != 0 {
		t.Fatalf("unchanged republish exit=%d stderr=%s", code, stderr.String())
	}
	afterNoop := pipelineRemoteWritePaths(backend)
	if len(afterNoop) != len(initialWrites) {
		t.Fatalf("unchanged republish recorded writes: before=%v after=%v", initialWrites, afterNoop)
	}
	if !strings.Contains(stdout.String(), "(0 changed, 0 deleted)") {
		t.Fatalf("unchanged republish output=%s", stdout.String())
	}

	updatedSpec := strings.Replace(specYAML, "# shared pipeline comment", "# shared pipeline comment updated", 1)
	updatedFile := filepath.Join(t.TempDir(), "share-flow.yaml")
	if err := os.WriteFile(updatedFile, []byte(updatedSpec), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"pipeline", "add", updatedFile, "--home", homeA}, &stdout, &stderr); code != 0 {
		t.Fatalf("update source pipeline exit=%d stderr=%s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"pipeline", "publish", "share-flow", "--home", homeA}, &stdout, &stderr); code != 0 {
		t.Fatalf("changed republish exit=%d stderr=%s", code, stderr.String())
	}
	afterChange := pipelineRemoteWritePaths(backend)
	newWrites := afterChange[len(afterNoop):]
	wantChangedWrites := []string{"pipelines/share-flow/bundle.yaml", "pipelines/share-flow/spec.yaml"}
	if !reflect.DeepEqual(newWrites, wantChangedWrites) {
		t.Fatalf("changed republish writes=%v want=%v", newWrites, wantChangedWrites)
	}
}

package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/agenttemplate"
	"github.com/jerryfane/gitmoot/internal/pipeline"
	"github.com/jerryfane/gitmoot/internal/runtime"
	yaml "gopkg.in/yaml.v3"
)

// TestPipelineBundleRoundTripE2E drives the complete offline sharing chain over
// two independent Gitmoot homes and repositories. Both pipeline stages execute
// through the real shell worker; no LLM, GitHub, Activepieces, or other network
// service participates.
func TestPipelineBundleRoundTripE2E(t *testing.T) {
	ctx := context.Background()
	homeA, _, storeA := heartbeatLoopE2EHome(t)
	checkoutA := pipelineBundleCheckout(t, "https://github.com/source/project.git")
	seedDaemonWorkerRepo(t, storeA, "source/project", checkoutA)

	const templateID = "bundle-reviewer"
	templateContent := bundleTemplateContent(templateID, "Review the pipeline result carefully.\n")
	installLocalTemplate(t, homeA, templateID, templateContent)
	agentCommand := pipelineStageResultCmd("approved", "agent reviewed", nil)
	subscribePipelineBundleShellAgent(t, homeA, "exported-reviewer", "source/project", templateID, agentCommand)

	cmd := "test -d /tmp && " + pipelineStageResultCmd("approved", "command collected", nil)
	specYAML := "# shared pipeline comment\nname: share-flow # preserve-name-comment\nrepo: source/project # preserve-repo-comment\n" +
		"trigger:\n  kind: email\n  connection: shared-imap\n" +
		"stages:\n  # preserve-stage-comment\n" +
		pipelineE2EStage("collect", cmd, "") +
		"  - id: review\n    agent: exported-reviewer\n    prompt: Review collected output.\n    needs: [collect]\n"
	specFile := writeSpec(t, specYAML)
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"pipeline", "add", specFile, "--home", homeA}, &stdout, &stderr); code != 0 {
		t.Fatalf("pipeline add exit=%d stderr=%s", code, stderr.String())
	}
	const secretMarker = "DO-NOT-EXPORT-BINDING-CREDENTIAL"
	if err := storeA.SetPipelineTriggerBinding(ctx, "share-flow", `{"flow_id":"`+secretMarker+`","binding_id":"local-only"}`); err != nil {
		t.Fatal(err)
	}

	bundleDir := filepath.Join(t.TempDir(), "share-flow.bundle")
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"pipeline", "export", "share-flow", "--home", homeA, "--output", bundleDir}, &stdout, &stderr); code != 0 {
		t.Fatalf("pipeline export exit=%d stderr=%s", code, stderr.String())
	}
	for _, path := range []string{"bundle.yaml", "spec.yaml", filepath.Join("templates", templateID+".md")} {
		if _, err := os.Stat(filepath.Join(bundleDir, path)); err != nil {
			t.Fatalf("bundle missing %s: %v", path, err)
		}
	}
	bundledSpec := readPipelineBundleTestFile(t, filepath.Join(bundleDir, "spec.yaml"))
	for _, want := range []string{
		"# shared pipeline comment", "repo: __GITMOOT_REPO__ # preserve-repo-comment", "# preserve-stage-comment",
	} {
		if !strings.Contains(string(bundledSpec), want) {
			t.Fatalf("spec.yaml missing %q:\n%s", want, bundledSpec)
		}
	}
	if strings.Contains(string(bundledSpec), "source/project") {
		t.Fatalf("spec.yaml leaked source repo:\n%s", bundledSpec)
	}

	manifestRaw := readPipelineBundleTestFile(t, filepath.Join(bundleDir, "bundle.yaml"))
	var manifest pipelineBundleManifest
	if err := yaml.Unmarshal(manifestRaw, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Repo != pipelineBundleRepoParameter || manifest.SpecSHA256 != pipeline.Hash(bundledSpec) {
		t.Fatalf("manifest repo/hash = %q/%q", manifest.Repo, manifest.SpecSHA256)
	}
	if len(manifest.Requirements.Connections) != 1 || manifest.Requirements.Connections[0].Name != "shared-imap" {
		t.Fatalf("connection requirements = %+v", manifest.Requirements.Connections)
	}
	if len(manifest.Warnings) != 1 || !strings.Contains(manifest.Warnings[0], "/tmp") {
		t.Fatalf("absolute path warnings = %v", manifest.Warnings)
	}
	if !strings.Contains(stderr.String(), "WARNING:") || !strings.Contains(stderr.String(), "/tmp") || !strings.Contains(stderr.String(), "prompts are pushed verbatim") {
		t.Fatalf("export warnings missing:\n%s", stderr.String())
	}
	originalTemplate, err := storeA.GetAgentTemplate(ctx, templateID)
	if err != nil {
		t.Fatal(err)
	}
	wantTemplate, err := agenttemplate.Export(originalTemplate)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(readPipelineBundleTestFile(t, filepath.Join(bundleDir, "templates", templateID+".md"))); got != wantTemplate {
		t.Fatalf("template snapshot differs from agenttemplate.Export\nwant:\n%q\ngot:\n%q", wantTemplate, got)
	}
	for _, path := range []string{filepath.Join(bundleDir, "bundle.yaml"), filepath.Join(bundleDir, "spec.yaml"), filepath.Join(bundleDir, "templates", templateID+".md")} {
		if raw := readPipelineBundleTestFile(t, path); bytes.Contains(raw, []byte(secretMarker)) || bytes.Contains(raw, []byte("trigger_binding")) {
			t.Fatalf("bundle file %s contains binding/credential material", path)
		}
	}

	// Home B has a differently named local shell session. --agent-map keeps that
	// machine-local command out of the bundle while still making the imported
	// pipeline runnable.
	homeB, _, storeB := heartbeatLoopE2EHome(t)
	checkoutB := pipelineBundleCheckout(t, "https://github.com/target/project.git")
	seedDaemonWorkerRepo(t, storeB, "target/project", checkoutB)
	installLocalTemplate(t, homeB, templateID, templateContent)
	subscribePipelineBundleShellAgent(t, homeB, "local-reviewer", "target/project", templateID, agentCommand)
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"pipeline", "import", bundleDir, "--home", homeB, "--repo", "target/project", "--agent-map", "exported-reviewer=local-reviewer"}, &stdout, &stderr); code != 0 {
		t.Fatalf("pipeline import exit=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{"Requirements report:", "runtime shell: present", "connection email/shared-imap: unchecked", "warning:", "imported pipeline share-flow (disabled"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("import stdout missing %q:\n%s", want, stdout.String())
		}
	}
	stored, found, err := storeB.GetPipeline(ctx, "share-flow")
	if err != nil || !found {
		t.Fatalf("GetPipeline: found=%v err=%v", found, err)
	}
	if stored.Enabled {
		t.Fatal("imported pipeline must be disabled by default")
	}
	if stored.Repo != "target/project" || stored.SpecHash != pipeline.Hash([]byte(stored.SpecYAML)) {
		t.Fatalf("stored repo/hash = %q/%q", stored.Repo, stored.SpecHash)
	}
	storedSpec, err := pipeline.Load([]byte(stored.SpecYAML))
	if err != nil {
		t.Fatal(err)
	}
	if storedSpec.Repo != "target/project" || storedSpec.Stages[1].Agent != "local-reviewer" {
		t.Fatalf("stored spec = %+v", storedSpec)
	}
	if !strings.Contains(stored.SpecYAML, "# shared pipeline comment") || !strings.Contains(stored.SpecYAML, "# preserve-stage-comment") {
		t.Fatalf("stored spec lost comments:\n%s", stored.SpecYAML)
	}

	// Enable the registry row directly: the E2E is deliberately zero-network,
	// while the user-facing enable command also materializes the email flow in
	// Activepieces. Trigger binding itself is covered by pipeline_trigger_test;
	// this test owns bundle portability and execution.
	if err := storeB.SetPipelineEnabled(ctx, "share-flow", true); err != nil {
		t.Fatalf("enable imported pipeline: %v", err)
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
		if err := runEnabledRepoWorkerTicks(ctx, storeB, worker, 1, io.Discard, now); err != nil {
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
		review := stageRow(t, storeB, runID, "review")
		agent, agentErr := storeB.GetAgent(ctx, "local-reviewer")
		events, eventsErr := storeB.ListJobEvents(ctx, review.JobID)
		t.Fatalf("imported run = %+v, found=%v err=%v; review=%+v; agent=%+v agentErr=%v; events=%+v eventsErr=%v", run, found, err, review, agent, agentErr, events, eventsErr)
	}

	t.Run("name collision", func(t *testing.T) {
		var out, errBuf bytes.Buffer
		code := Run([]string{"pipeline", "import", bundleDir, "--home", homeB, "--repo", "target/project", "--agent-map", "exported-reviewer=local-reviewer"}, &out, &errBuf)
		if code == 0 || !strings.Contains(errBuf.String(), `pipeline "share-flow" already exists`) {
			t.Fatalf("code=%d stdout=%s stderr=%s", code, out.String(), errBuf.String())
		}
	})
	t.Run("corrupt sha", func(t *testing.T) {
		variant := writePipelineBundleVariant(t, bundleDir, func(manifest *pipelineBundleManifest) { manifest.SpecSHA256 = strings.Repeat("0", 64) })
		var out, errBuf bytes.Buffer
		code := Run([]string{"pipeline", "import", variant, "--home", t.TempDir(), "--repo", "target/project", "--agent-map", "exported-reviewer=local-reviewer"}, &out, &errBuf)
		if code == 0 || !strings.Contains(errBuf.String(), "spec_sha256 mismatch") || !strings.Contains(out.String(), "Requirements report:") {
			t.Fatalf("code=%d stdout=%s stderr=%s", code, out.String(), errBuf.String())
		}
	})
	t.Run("unknown bundle version", func(t *testing.T) {
		variant := writePipelineBundleVariant(t, bundleDir, func(manifest *pipelineBundleManifest) { manifest.BundleVersion = 99 })
		var out, errBuf bytes.Buffer
		code := Run([]string{"pipeline", "import", variant, "--home", t.TempDir(), "--repo", "target/project", "--agent-map", "exported-reviewer=local-reviewer"}, &out, &errBuf)
		if code == 0 || !strings.Contains(errBuf.String(), "unsupported bundle_version 99") || !strings.Contains(out.String(), "Requirements report:") {
			t.Fatalf("code=%d stdout=%s stderr=%s", code, out.String(), errBuf.String())
		}
	})
	t.Run("missing runtime names agent", func(t *testing.T) {
		variant := writePipelineBundleVariant(t, bundleDir, func(manifest *pipelineBundleManifest) {
			manifest.Agents[0].Runtime = "missing-runtime"
			manifest.Requirements.Runtimes = []string{"missing-runtime", runtime.ShellRuntime}
		})
		var out, errBuf bytes.Buffer
		code := Run([]string{"pipeline", "import", variant, "--home", t.TempDir(), "--repo", "target/project"}, &out, &errBuf)
		if code == 0 || !strings.Contains(errBuf.String(), `agent "exported-reviewer" requires missing runtime "missing-runtime"`) || !strings.Contains(out.String(), "runtime missing-runtime: missing") {
			t.Fatalf("code=%d stdout=%s stderr=%s", code, out.String(), errBuf.String())
		}
	})
}

func pipelineBundleCheckout(t *testing.T, remote string) string {
	t.Helper()
	checkout := createDaemonWorkerGitCheckout(t, "main")
	runDaemonWorkerGit(t, checkout, "remote", "set-url", "origin", remote)
	return checkout
}

func subscribePipelineBundleShellAgent(t *testing.T, home, name, repo, templateID, command string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"agent", "subscribe", name,
		"--home", home,
		"--runtime", runtime.ShellRuntime,
		"--session", command,
		"--role", "worker",
		"--template", templateID,
		"--repo", repo,
		"--capability", "ask",
		"--capability", "review",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("agent subscribe %s exit=%d stdout=%s stderr=%s", name, code, stdout.String(), stderr.String())
	}
}

func readPipelineBundleTestFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func writePipelineBundleVariant(t *testing.T, source string, mutate func(*pipelineBundleManifest)) string {
	t.Helper()
	target := t.TempDir()
	manifestRaw := readPipelineBundleTestFile(t, filepath.Join(source, "bundle.yaml"))
	var manifest pipelineBundleManifest
	if err := yaml.Unmarshal(manifestRaw, &manifest); err != nil {
		t.Fatal(err)
	}
	mutate(&manifest)
	manifestRaw, err := yaml.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "bundle.yaml"), manifestRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "spec.yaml"), readPipelineBundleTestFile(t, filepath.Join(source, "spec.yaml")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(target, "templates"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, agent := range manifest.Agents {
		if agent.TemplateRef == "" {
			continue
		}
		raw := readPipelineBundleTestFile(t, filepath.Join(source, "templates", agent.TemplateRef+".md"))
		if err := os.WriteFile(filepath.Join(target, "templates", agent.TemplateRef+".md"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return target
}

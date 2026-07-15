package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/agenttemplate"
	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/pipeline"
	"github.com/jerryfane/gitmoot/internal/runtime"
	yaml "gopkg.in/yaml.v3"
)

func TestReadPipelineBundleManifestKnownFields(t *testing.T) {
	dir := t.TempDir()
	manifest := validPipelineBundleManifestForTest(t, []byte(bundleSpecForTest))
	raw, err := yaml.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, []byte("unknown_bundle_key: true\n")...)
	if err := os.WriteFile(filepath.Join(dir, "bundle.yaml"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "spec.yaml"), []byte(bundleSpecForTest), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err = readPipelineBundleFiles(dir)
	if err == nil || !strings.Contains(err.Error(), "field unknown_bundle_key not found") {
		t.Fatalf("KnownFields error = %v", err)
	}
}

func TestPipelineBundleVersionGate(t *testing.T) {
	for _, test := range []struct {
		name, minimum, current string
		wantErr                string
	}{
		{name: "same", minimum: "1.2.3", current: "1.2.3"},
		{name: "newer current", minimum: "1.2.3", current: "1.3.0"},
		{name: "stable after prerelease", minimum: "1.2.3-beta.8", current: "1.2.3"},
		{name: "development at floor", minimum: pipelineBundleDevelopmentMinimumVersion, current: "dev-abc123"},
		{name: "development rejects future", minimum: "99.0.0", current: "dev-abc123", wantErr: "requires Gitmoot >= 99.0.0"},
		{name: "too old", minimum: "1.3.0", current: "1.2.9", wantErr: "requires Gitmoot >= 1.3.0"},
		{name: "prerelease too old", minimum: "1.2.3", current: "1.2.3-beta.8", wantErr: "requires Gitmoot >= 1.2.3"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := requirePipelineBundleVersion(test.minimum, test.current)
			if test.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)) {
				t.Fatalf("error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

func TestPipelineBundleExportMinimumVersionIsSemver(t *testing.T) {
	got := pipelineBundleExportMinimumVersion()
	if _, err := parsePipelineBundleSemver(got); err != nil {
		t.Fatalf("export minimum %q is not semver: %v", got, err)
	}
}

func TestRewritePipelineBundleSpecPreservesCommentsOrderAndIsDeterministic(t *testing.T) {
	raw := []byte("# pipeline comment\nname: source-flow # name comment\ngroup: sharing\nrepo: 'owner/source' # repo comment\nstages:\n  # stage comment\n  - id: review\n    agent: old-agent # agent comment\n    prompt: Review.\n")
	want := "# pipeline comment\nname: imported-flow # name comment\ngroup: sharing\nrepo: target/repo # repo comment\nstages:\n  # stage comment\n  - id: review\n    agent: local-agent # agent comment\n    prompt: Review.\n"
	one, err := rewritePipelineBundleSpec(raw, "target/repo", "imported-flow", map[string]string{"old-agent": "local-agent"})
	if err != nil {
		t.Fatal(err)
	}
	two, err := rewritePipelineBundleSpec(raw, "target/repo", "imported-flow", map[string]string{"old-agent": "local-agent"})
	if err != nil {
		t.Fatal(err)
	}
	if string(one) != want {
		t.Fatalf("rewrite changed non-target bytes\nwant:\n%s\ngot:\n%s", want, one)
	}
	if !bytes.Equal(one, two) {
		t.Fatal("rewrite is not deterministic")
	}
}

func TestRewritePipelineBundleSpecInsertsMissingRepo(t *testing.T) {
	raw := []byte("name: repo-less\n# keep me here\nstages:\n  - id: check\n    cmd: echo ok\n")
	got, err := rewritePipelineBundleSpec(raw, pipelineBundleRepoParameter, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "name: repo-less\nrepo: __GITMOOT_REPO__\n# keep me here\nstages:\n  - id: check\n    cmd: echo ok\n"
	if string(got) != want {
		t.Fatalf("want:\n%s\ngot:\n%s", want, got)
	}
}

func TestDerivePipelineBundleRequirements(t *testing.T) {
	raw := []byte(`name: share-me
repo: owner/repo
trigger:
  kind: email
  connection: inbox-imap
stages:
  - id: collect
    cmd: echo ok
  - id: review
    agent: reviewer
    prompt: Review.
`)
	spec, err := pipeline.Load(raw)
	if err != nil {
		t.Fatal(err)
	}
	got := derivePipelineBundleRequirements(spec, []pipelineBundleAgent{{Name: "reviewer", Runtime: runtime.CodexRuntime}})
	if !reflect.DeepEqual(got.Runtimes, []string{runtime.CodexRuntime, runtime.ShellRuntime}) {
		t.Fatalf("runtimes = %v", got.Runtimes)
	}
	if !reflect.DeepEqual(got.Connections, []pipelineBundleConnection{{Kind: "email", Name: "inbox-imap"}}) {
		t.Fatalf("connections = %+v", got.Connections)
	}
	if len(got.UpstreamPipelines) != 0 {
		t.Fatalf("upstreams = %v", got.UpstreamPipelines)
	}

	upstreamSpec, err := pipeline.Load([]byte("name: downstream\nrepo: owner/repo\ntrigger:\n  kind: pipeline\n  pipeline: upstream\nstages:\n  - id: check\n    cmd: echo ok\n"))
	if err != nil {
		t.Fatal(err)
	}
	upstreamRequirements := derivePipelineBundleRequirements(upstreamSpec, nil)
	if !reflect.DeepEqual(upstreamRequirements.UpstreamPipelines, []string{"upstream"}) {
		t.Fatalf("upstreams = %v", upstreamRequirements.UpstreamPipelines)
	}
}

func TestParsePipelineAgentMappings(t *testing.T) {
	got, err := parsePipelineAgentMappings([]string{"reviewer=local-reviewer", "writer=local_writer"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"reviewer": "local-reviewer", "writer": "local_writer"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got = %v, want %v", got, want)
	}
	for _, invalid := range []string{"missing-equals", "a=b=c", "bad/name=ok", "a=one", "a=two"} {
		values := []string{invalid}
		if invalid == "a=one" {
			values = []string{"a=one", "a=two"}
		}
		if _, err := parsePipelineAgentMappings(values); err == nil {
			t.Fatalf("parse %v unexpectedly succeeded", values)
		}
		if invalid == "a=one" {
			break
		}
	}
}

func TestValidatePipelineBundleSHAAndVersion(t *testing.T) {
	raw := []byte(bundleSpecForTest)
	manifest := validPipelineBundleManifestForTest(t, raw)
	if err := validatePipelineBundle(manifest, raw, "1.0.0"); err != nil {
		t.Fatalf("valid bundle: %v", err)
	}
	corrupt := manifest
	corrupt.SpecSHA256 = strings.Repeat("0", 64)
	if err := validatePipelineBundle(corrupt, raw, "1.0.0"); err == nil || !strings.Contains(err.Error(), "spec_sha256 mismatch") {
		t.Fatalf("sha error = %v", err)
	}
	unknown := manifest
	unknown.BundleVersion = 99
	if err := validatePipelineBundle(unknown, raw, "1.0.0"); err == nil || !strings.Contains(err.Error(), "unsupported bundle_version 99") {
		t.Fatalf("version error = %v", err)
	}
}

func TestDetectPipelineAbsolutePathWarnings(t *testing.T) {
	spec, err := pipeline.Load([]byte("name: paths\nstages:\n  - id: one\n    cmd: cp /tmp/in /home/alice/out && echo /var/ok\n  - id: agent\n    agent: reviewer\n    prompt: Inspect /root but do not execute it.\n"))
	if err != nil {
		t.Fatal(err)
	}
	got := detectPipelineAbsolutePathWarnings(spec)
	if len(got) != 2 || !strings.Contains(got[0], "/tmp/in") || !strings.Contains(got[1], "/home/alice/out") {
		t.Fatalf("warnings = %v", got)
	}
}

func TestPipelineBundleTemplateAndAgentCollisionMatrix(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	bundle := t.TempDir()
	if err := os.MkdirAll(filepath.Join(bundle, "templates"), 0o755); err != nil {
		t.Fatal(err)
	}
	templatePath := filepath.Join(bundle, "templates", "bundle-reviewer.md")
	first := bundleTemplateContent("bundle-reviewer", "Review carefully.")
	if err := os.WriteFile(templatePath, []byte(first), 0o600); err != nil {
		t.Fatal(err)
	}
	bundled := pipelineBundleAgent{Name: "reviewer", Runtime: runtime.CodexRuntime, TemplateRef: "bundle-reviewer"}
	if err := installPipelineBundleTemplate(ctx, store, bundle, bundled, false); err != nil {
		t.Fatalf("new template: %v", err)
	}
	if err := installPipelineBundleTemplate(ctx, store, bundle, bundled, false); err != nil {
		t.Fatalf("same template no-op: %v", err)
	}
	if err := installPipelineBundleTemplate(ctx, store, bundle, bundled, true); err != nil {
		t.Fatalf("same template with force no-op: %v", err)
	}
	second := bundleTemplateContent("bundle-reviewer", "Review differently.")
	if err := os.WriteFile(templatePath, []byte(second), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := installPipelineBundleTemplate(ctx, store, bundle, bundled, false); err == nil || !strings.Contains(err.Error(), "different content") {
		t.Fatalf("different template without force = %v", err)
	}
	if err := installPipelineBundleTemplate(ctx, store, bundle, bundled, true); err != nil {
		t.Fatalf("different template with force: %v", err)
	}
	if err := installPipelineBundleAgent(ctx, store, bundled, "owner/repo", false); err != nil {
		t.Fatalf("new agent: %v", err)
	}
	if err := installPipelineBundleAgent(ctx, store, bundled, "owner/repo", false); err != nil {
		t.Fatalf("same agent no-op: %v", err)
	}
	if err := installPipelineBundleAgent(ctx, store, bundled, "owner/repo", true); err != nil {
		t.Fatalf("same agent with force no-op: %v", err)
	}
	different := bundled
	different.Runtime = runtime.ClaudeRuntime
	if err := installPipelineBundleAgent(ctx, store, different, "owner/repo", false); err == nil || !strings.Contains(err.Error(), "different runtime") {
		t.Fatalf("different agent without force = %v", err)
	}
	if err := installPipelineBundleAgent(ctx, store, different, "owner/repo", true); err != nil {
		t.Fatalf("different agent with force: %v", err)
	}
	stored, err := store.GetAgent(ctx, bundled.Name)
	if err != nil || stored.Runtime != runtime.ClaudeRuntime {
		t.Fatalf("forced agent = %+v, err=%v", stored, err)
	}
}

func TestPipelineBundleMissingRuntimeNamesAgent(t *testing.T) {
	original := pipelineBundleLookPath
	pipelineBundleLookPath = func(string) (string, error) { return "", errors.New("missing") }
	t.Cleanup(func() { pipelineBundleLookPath = original })
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	manifest := validPipelineBundleManifestForTest(t, []byte(bundleSpecForTest))
	manifest.Agents = []pipelineBundleAgent{{Name: "reviewer", Runtime: runtime.CodexRuntime}}
	manifest.Requirements.Runtimes = []string{runtime.CodexRuntime}
	report := inspectPipelineBundleRequirements(context.Background(), store, paths, home, manifest, nil)
	if len(report.AgentErrors) != 1 || !strings.Contains(report.AgentErrors[0].Error(), `agent "reviewer" requires missing runtime "codex"`) {
		t.Fatalf("agent errors = %v", report.AgentErrors)
	}
}

const bundleSpecForTest = `name: bundle-test
repo: __GITMOOT_REPO__
stages:
  - id: check
    cmd: echo ok
`

func validPipelineBundleManifestForTest(t *testing.T, raw []byte) pipelineBundleManifest {
	t.Helper()
	return pipelineBundleManifest{
		BundleVersion:     pipelineBundleVersion,
		GitmootVersionMin: "0.1.0",
		Pipeline:          "bundle-test",
		Repo:              pipelineBundleRepoParameter,
		Requirements:      pipelineBundleRequirements{Runtimes: []string{runtime.ShellRuntime}, Connections: []pipelineBundleConnection{}, UpstreamPipelines: []string{}},
		Warnings:          []string{},
		WriteAuthority:    []string{},
		Agents:            []pipelineBundleAgent{},
		SpecSHA256:        pipeline.Hash(raw),
	}
}

func bundleTemplateContent(id, body string) string {
	return agenttemplate.FormatTemplateContent(agenttemplate.Metadata{
		ID:                   id,
		Name:                 "Bundle Reviewer",
		Description:          "Reviews imported bundle behavior.",
		Kind:                 agenttemplate.TemplateKind,
		Version:              agenttemplate.TemplateVersion,
		Capabilities:         []string{"ask", "review"},
		RuntimeCompatibility: []string{"codex", "claude", "kimi"},
		Tags:                 []string{"review"},
		Inputs:               []string{"task"},
		Outputs:              []string{"review"},
	}, body)
}

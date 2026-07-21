package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/pipeline"
)

func TestRunPipelineExposeTokenLifecycleAndJSON(t *testing.T) {
	home := t.TempDir()
	if err := withStore(home, func(store *db.Store) error {
		spec := "name: marketing-kit\nrepo: owner/repo\nstages:\n  - id: build\n    cmd: printf ok\n"
		return store.CreateOrUpdatePipeline(context.Background(), db.Pipeline{
			Name: "marketing-kit", Repo: "owner/repo", SpecYAML: spec, SpecHash: pipeline.Hash([]byte(spec)),
		})
	}); err != nil {
		t.Fatal(err)
	}
	schemaPath := filepath.Join(t.TempDir(), "service-schema.json")
	if err := os.WriteFile(schemaPath, []byte(validExposeSchema), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	args := []string{"pipeline", "expose", "--schema", schemaPath, "--home", home, "marketing-kit"}
	if code := Run(args, &stdout, &stderr); code != 0 {
		t.Fatalf("first expose exit=%d stderr=%s", code, stderr.String())
	}
	first := stdout.String()
	if !strings.Contains(first, "token: ") || !strings.Contains(first, "(shown once)") ||
		!strings.Contains(first, "status_url: /v1/pipelines/runs/<run-id>") ||
		!strings.Contains(first, "receipt_url: /receipts/<run-id>") {
		t.Fatalf("first expose output:\n%s", first)
	}
	firstToken := exposeTokenFromText(t, first)

	stdout.Reset()
	stderr.Reset()
	jsonArgs := []string{"pipeline", "expose", "--schema", schemaPath, "--home", home, "--json", "marketing-kit"}
	if code := Run(jsonArgs, &stdout, &stderr); code != 0 {
		t.Fatalf("reinspect exit=%d stderr=%s", code, stderr.String())
	}
	var inspected pipelineExposeOutput
	if err := json.Unmarshal(stdout.Bytes(), &inspected); err != nil {
		t.Fatalf("decode JSON output: %v\n%s", err, stdout.String())
	}
	if inspected.Token != "" || inspected.TokenIssued || inspected.SchemaHash == "" || len(inspected.Fields) != 2 {
		t.Fatalf("reinspection leaked/reminted token or lost schema: %+v", inspected)
	}
	if strings.Contains(stdout.String(), "token_hash") || strings.Contains(stdout.String(), firstToken) {
		t.Fatalf("reinspection exposed secret material: %s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	rotateArgs := []string{"pipeline", "expose", "--schema", schemaPath, "--home", home, "--rotate-token", "marketing-kit"}
	if code := Run(rotateArgs, &stdout, &stderr); code != 0 {
		t.Fatalf("rotate exit=%d stderr=%s", code, stderr.String())
	}
	rotated := exposeTokenFromText(t, stdout.String())
	if rotated == firstToken {
		t.Fatal("--rotate-token returned the original token")
	}
	if err := withStore(home, func(store *db.Store) error {
		_, oldValid, err := store.AuthenticateExposureToken(context.Background(), "marketing-kit", firstToken)
		if err != nil {
			return err
		}
		_, newValid, err := store.AuthenticateExposureToken(context.Background(), "marketing-kit", rotated)
		if err != nil {
			return err
		}
		if oldValid || !newValid {
			t.Fatalf("rotation auth old=%v new=%v", oldValid, newValid)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	stdout.Reset()
	stderr.Reset()
	disableArgs := []string{"pipeline", "expose", "--schema", schemaPath, "--home", home, "--disable", "marketing-kit"}
	if code := Run(disableArgs, &stdout, &stderr); code != 0 {
		t.Fatalf("disable exit=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "status: disabled") || !strings.Contains(stdout.String(), "token: -") {
		t.Fatalf("disable output:\n%s", stdout.String())
	}
	if err := withStore(home, func(store *db.Store) error {
		exposure, ok, err := store.GetExposure(context.Background(), "marketing-kit")
		if err == nil && (!ok || exposure.Enabled) {
			t.Fatalf("disabled exposure = %+v ok=%v", exposure, ok)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRunPipelineExposeRejectsInvalidSchemaAndUnknownPipeline(t *testing.T) {
	home := t.TempDir()
	invalidPath := filepath.Join(t.TempDir(), "invalid-schema.json")
	if err := os.WriteFile(invalidPath, []byte(`{"version":1,"fields":{"prompt":{"type":"string"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"pipeline", "expose", "--schema", invalidPath, "--home", home, "missing"}, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "max_length is required") {
		t.Fatalf("invalid schema exit=%d stderr=%s", code, stderr.String())
	}

	validPath := filepath.Join(t.TempDir(), "valid-schema.json")
	if err := os.WriteFile(validPath, []byte(validExposeSchema), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"pipeline", "expose", "--schema", validPath, "--home", home, "missing"}, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "pipeline missing not found") {
		t.Fatalf("unknown pipeline exit=%d stderr=%s", code, stderr.String())
	}
	agentSpec := "name: agent-flow\nrepo: owner/repo\nstages:\n  - id: review\n    agent: reviewer\n    action: ask\n    prompt: review\n"
	if err := withStore(home, func(store *db.Store) error {
		return store.CreateOrUpdatePipeline(context.Background(), db.Pipeline{Name: "agent-flow", Repo: "owner/repo", SpecYAML: agentSpec, SpecHash: pipeline.Hash([]byte(agentSpec))})
	}); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"pipeline", "expose", "--schema", validPath, "--home", home, "agent-flow"}, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), `stage "review" is not a template-free shell stage`) {
		t.Fatalf("unsafe pipeline exposure exit=%d stderr=%s", code, stderr.String())
	}

	envSpec := "name: secret-shell\nrepo: owner/repo\nstages:\n  - id: publish\n    cmd: printf ok\n    env_keys: [DEPLOY_TOKEN]\n"
	if err := withStore(home, func(store *db.Store) error {
		return store.CreateOrUpdatePipeline(context.Background(), db.Pipeline{Name: "secret-shell", Repo: "owner/repo", SpecYAML: envSpec, SpecHash: pipeline.Hash([]byte(envSpec))})
	}); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"pipeline", "expose", "--schema", validPath, "--home", home, "secret-shell"}, &stdout, &stderr); code == 0 ||
		!strings.Contains(stderr.String(), `stage "publish" declares env_keys`) || !strings.Contains(stderr.String(), "forbids keycard secrets") {
		t.Fatalf("env_keys pipeline exposure exit=%d stderr=%s", code, stderr.String())
	}
}

func TestServicePipelinePublicSafetyRejectsAuthorityDeclarations(t *testing.T) {
	plain := pipeline.Spec{Stages: []pipeline.Stage{{ID: "build", Cmd: "printf ok"}}}
	if servicePipelinePublicSafetyError(plain) != nil {
		t.Fatalf("plain shell pipeline rejected: %v", servicePipelinePublicSafetyError(plain))
	}
	tests := []struct {
		name  string
		stage pipeline.Stage
		field string
	}{
		{name: "env keys", stage: pipeline.Stage{ID: "build", Cmd: "printf ok", EnvKeys: []string{"TOKEN"}}, field: "env_keys"},
		{name: "write", stage: pipeline.Stage{ID: "build", Cmd: "printf ok", Write: true}, field: "write"},
		{name: "writes", stage: pipeline.Stage{ID: "build", Cmd: "printf ok", Writes: []string{"/tmp/out"}}, field: "writes"},
		{name: "reads", stage: pipeline.Stage{ID: "build", Cmd: "printf ok", Reads: []string{"/tmp/in"}}, field: "reads"},
		{name: "network", stage: pipeline.Stage{ID: "build", Cmd: "printf ok", Network: true}, field: "network"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := pipeline.Spec{Stages: []pipeline.Stage{tt.stage}}
			err := servicePipelinePublicSafetyError(spec)
			if err == nil || !strings.Contains(err.Error(), `stage "build"`) || !strings.Contains(err.Error(), tt.field) {
				t.Fatalf("public safety error=%v", err)
			}
		})
	}
}

const validExposeSchema = `{"version":1,"fields":{"app_name":{"type":"string","required":true,"max_length":120},"count":{"type":"integer","minimum":1,"maximum":5}}}`

func exposeTokenFromText(t *testing.T, output string) string {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "token: ") && strings.HasSuffix(line, " (shown once)") {
			return strings.TrimSuffix(strings.TrimPrefix(line, "token: "), " (shown once)")
		}
	}
	t.Fatalf("one-time token not found in output:\n%s", output)
	return ""
}

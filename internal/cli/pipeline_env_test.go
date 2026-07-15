package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/pipeline"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

const (
	pipelineEnvSecretA = "secret-alpha-968"
	pipelineEnvSecretB = "secret-beta-968"
)

func writePipelineEnvFile(t *testing.T, dir, body string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(dir, "pipeline.env")
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPipelineAddEnvFileValidation(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, home string) (envFile, envBody, stage string)
		want  string
	}{
		{
			name: "missing file",
			setup: func(t *testing.T, _ string) (string, string, string) {
				return filepath.Join(t.TempDir(), "missing.env"), "", "{id: run, cmd: echo, env_keys: [TOKEN]}"
			},
			want: "does not exist",
		},
		{
			name: "wrong mode",
			setup: func(t *testing.T, _ string) (string, string, string) {
				return writePipelineEnvFile(t, t.TempDir(), "TOKEN="+pipelineEnvSecretA+"\n", 0o644), "", "{id: run, cmd: echo, env_keys: [TOKEN]}"
			},
			want: "want 0600",
		},
		{
			name: "inside Gitmoot home",
			setup: func(t *testing.T, home string) (string, string, string) {
				dir := filepath.Join(home, ".gitmoot", "private")
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				return writePipelineEnvFile(t, dir, "TOKEN="+pipelineEnvSecretA+"\n", 0o600), "", "{id: run, cmd: echo, env_keys: [TOKEN]}"
			},
			want: "inside Gitmoot home",
		},
		{
			name: "inside managed checkout",
			setup: func(t *testing.T, home string) (string, string, string) {
				checkout := t.TempDir()
				if err := withStore(home, func(store *db.Store) error {
					return store.UpsertRepo(context.Background(), db.Repo{Owner: "owner", Name: "repo", CheckoutPath: checkout})
				}); err != nil {
					t.Fatal(err)
				}
				return writePipelineEnvFile(t, checkout, "TOKEN="+pipelineEnvSecretA+"\n", 0o600), "", "{id: run, cmd: echo, env_keys: [TOKEN]}"
			},
			want: "inside managed checkout",
		},
		{
			name: "reserved file key",
			setup: func(t *testing.T, _ string) (string, string, string) {
				return writePipelineEnvFile(t, t.TempDir(), "GITMOOT_PIPELINE_NAME="+pipelineEnvSecretA+"\n", 0o600), "", "{id: run, cmd: echo}"
			},
			want: "reserved GITMOOT_*",
		},
		{
			name: "absent key",
			setup: func(t *testing.T, _ string) (string, string, string) {
				return writePipelineEnvFile(t, t.TempDir(), "PRESENT="+pipelineEnvSecretA+"\n", 0o600), "", "{id: run, cmd: echo, env_keys: [MISSING]}"
			},
			want: `env_keys entry "MISSING" does not resolve`,
		},
		{
			name: "agent stage denied",
			setup: func(t *testing.T, _ string) (string, string, string) {
				return writePipelineEnvFile(t, t.TempDir(), "TOKEN="+pipelineEnvSecretA+"\n", 0o600), "", "{id: run, agent: scout, action: ask, prompt: inspect, env_keys: [TOKEN]}"
			},
			want: "agent and gate stages receive no injected environment",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			envFile, extra, stage := tt.setup(t, home)
			raw := fmt.Sprintf("name: env-validation\nenv_file: %q\n%s\nstages:\n  - %s\n", envFile, extra, stage)
			specFile := writeSpec(t, raw)
			var stdout, stderr bytes.Buffer
			code := Run([]string{"pipeline", "add", specFile, "--home", home}, &stdout, &stderr)
			if code == 0 || !strings.Contains(stderr.String(), tt.want) {
				t.Fatalf("code=%d stderr=%q, want %q", code, stderr.String(), tt.want)
			}
			if strings.Contains(stdout.String()+stderr.String(), pipelineEnvSecretA) {
				t.Fatalf("validation leaked secret value: stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
			if err := withStore(home, func(store *db.Store) error {
				_, found, err := store.GetPipeline(context.Background(), "env-validation")
				if err == nil && found {
					return fmt.Errorf("invalid pipeline was persisted")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPipelineAddEnvFileWrongOwner(t *testing.T) {
	home := t.TempDir()
	envFile := writePipelineEnvFile(t, t.TempDir(), "TOKEN="+pipelineEnvSecretA+"\n", 0o600)
	original := pipelineEnvCurrentUID
	pipelineEnvCurrentUID = func() uint32 { return original() + 1 }
	t.Cleanup(func() { pipelineEnvCurrentUID = original })
	specFile := writeSpec(t, fmt.Sprintf("name: wrong-owner\nenv_file: %q\nstages: [{id: run, cmd: echo, env_keys: [TOKEN]}]\n", envFile))
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"pipeline", "add", specFile, "--home", home}, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "owned by uid") {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if strings.Contains(stderr.String(), pipelineEnvSecretA) {
		t.Fatalf("owner error leaked secret: %q", stderr.String())
	}
}

type pipelineEnvCaptureAdapter struct {
	jobs []runtime.Job
}

func (a *pipelineEnvCaptureAdapter) Deliver(_ context.Context, _ runtime.Agent, job runtime.Job) (runtime.Result, error) {
	a.jobs = append(a.jobs, job)
	return runtime.Result{Raw: `{"gitmoot_result":{"decision":"approved","summary":"ok","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`}, nil
}

func TestPipelineEnvDeliveryScopePrecedenceAndRotation(t *testing.T) {
	home, _, store := heartbeatLoopE2EHome(t)
	envFile := writePipelineEnvFile(t, t.TempDir(), "KEY_A="+pipelineEnvSecretA+"\nKEY_B="+pipelineEnvSecretB+"\n", 0o600)
	spec := pipeline.Spec{EnvFile: envFile, Env: map[string]string{"DEFAULT": "inline"}}
	stage := pipeline.Stage{ID: "a", Cmd: "echo", EnvKeys: []string{"KEY_A", "DEFAULT"}}
	access, err := resolvePipelineStageEnvAccess(context.Background(), store, home, spec, stage)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(access.Keys, []string{"KEY_A", "DEFAULT"}) {
		t.Fatalf("resolved keys = %#v", access.Keys)
	}
	capture := &pipelineEnvCaptureAdapter{}
	wrapped := wrapPipelineEnvDeliveryAdapter(store, home, workflow.JobPayload{
		PipelineEnvFile: access.File, PipelineEnvKeys: access.Keys, PipelineEnv: access.Defaults,
	}, capture)
	base := runtime.Job{ShellEnv: []string{"GITMOOT_PIPELINE_NAME=real"}}
	if _, err := wrapped.Deliver(context.Background(), runtime.Agent{}, base); err != nil {
		t.Fatal(err)
	}
	if got := capture.jobs[0].ShellEnv; !reflect.DeepEqual(got, []string{"KEY_A=" + pipelineEnvSecretA, "DEFAULT=inline", "GITMOOT_PIPELINE_NAME=real"}) {
		t.Fatalf("first delivery env = %#v", got)
	}
	if strings.Contains(strings.Join(capture.jobs[0].ShellEnv, "\n"), "KEY_B=") {
		t.Fatalf("sibling key leaked: %#v", capture.jobs[0].ShellEnv)
	}
	emptyCapture := &pipelineEnvCaptureAdapter{}
	empty := wrapPipelineEnvDeliveryAdapter(store, home, workflow.JobPayload{}, emptyCapture)
	if _, err := empty.Deliver(context.Background(), runtime.Agent{}, base); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(emptyCapture.jobs[0].ShellEnv, base.ShellEnv) {
		t.Fatalf("empty env_keys changed delivery env: %#v", emptyCapture.jobs[0].ShellEnv)
	}

	if err := os.WriteFile(envFile, []byte("KEY_A=rotated-alpha-968\nKEY_B="+pipelineEnvSecretB+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := wrapped.Deliver(context.Background(), runtime.Agent{}, base); err != nil {
		t.Fatal(err)
	}
	if got := capture.jobs[1].ShellEnv[0]; got != "KEY_A=rotated-alpha-968" {
		t.Fatalf("rotated delivery = %q", got)
	}

	reservedCapture := &pipelineEnvCaptureAdapter{}
	reserved := wrapPipelineEnvDeliveryAdapter(store, home, workflow.JobPayload{
		PipelineEnvKeys: []string{"GITMOOT_PIPELINE_NAME"},
		PipelineEnv:     map[string]string{"GITMOOT_PIPELINE_NAME": "untrusted"},
	}, reservedCapture)
	if _, err := reserved.Deliver(context.Background(), runtime.Agent{}, base); err != nil {
		t.Fatal(err)
	}
	if got := reservedCapture.jobs[0].ShellEnv; !reflect.DeepEqual(got, []string{"GITMOOT_PIPELINE_NAME=untrusted", "GITMOOT_PIPELINE_NAME=real"}) {
		t.Fatalf("Gitmoot internal value did not win at delivery: %#v", got)
	}
}

func TestPipelineInjectedEnvShellE2E(t *testing.T) {
	ctx := context.Background()
	home, paths, store := heartbeatLoopE2EHome(t)
	checkout := createDaemonWorkerGitCheckout(t, "main")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	const keyA = "PIPELINE_KEY_A_968"
	const keyB = "PIPELINE_KEY_B_968"
	for _, key := range []string{keyA, keyB} {
		old, existed := os.LookupEnv(key)
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if existed {
				_ = os.Setenv(key, old)
			} else {
				_ = os.Unsetenv(key)
			}
		})
	}
	envFile := writePipelineEnvFile(t, t.TempDir(), keyA+"="+pipelineEnvSecretA+"\n"+keyB+"="+pipelineEnvSecretB+"\n", 0o600)
	outA := filepath.Join(t.TempDir(), "a.txt")
	outB := filepath.Join(t.TempDir(), "b.txt")
	resultA := pipelineShellResultCommand(t, "approved", "stage a")
	resultB := pipelineShellResultCommand(t, "approved", "stage b")
	cmdA := fmt.Sprintf(`test -n "$%s"; test -z "${%s+x}"; printf 'A:%s-present,%s-absent' > %q; %s`, keyA, keyB, keyA, keyB, outA, resultA)
	cmdB := fmt.Sprintf(`test -n "$%s"; test -z "${%s+x}"; printf 'B:%s-present,%s-absent' > %q; %s`, keyB, keyA, keyB, keyA, outB, resultB)
	specYAML := fmt.Sprintf("name: env-e2e\nrepo: owner/repo\nenv_file: %q\nstages:\n  - id: a\n    cmd: |\n      %s\n    env_keys: [%s]\n  - id: b\n    cmd: |\n      %s\n    env_keys: [%s]\n    needs: [a]\n", envFile, cmdA, keyA, cmdB, keyB)
	specFile := writeSpec(t, specYAML)
	var out, errBuf bytes.Buffer
	if code := Run([]string{"pipeline", "add", specFile, "--home", home}, &out, &errBuf); code != 0 {
		t.Fatalf("pipeline add exit=%d stderr=%s", code, errBuf.String())
	}
	out.Reset()
	errBuf.Reset()
	if code := Run([]string{"pipeline", "run", "env-e2e", "--home", home}, &out, &errBuf); code != 0 {
		t.Fatalf("pipeline run exit=%d stderr=%s", code, errBuf.String())
	}
	runID := strings.TrimSpace(out.String())
	enqueue := newPipelineStageEnqueuer(store, home)
	worker := defaultJobWorker(store, io.Discard, home)
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 8; i++ {
		if err := runEnabledRepoWorkerTicks(ctx, store, worker, 1, io.Discard, now); err != nil {
			t.Fatalf("worker tick %d: %v", i, err)
		}
		if err := runPipelineScanOnce(ctx, store, enqueue, now); err != nil {
			t.Fatalf("pipeline scan %d: %v", i, err)
		}
		run, _, err := store.GetPipelineRun(ctx, runID)
		if err != nil {
			t.Fatal(err)
		}
		if run.State != pipeline.RunRunning {
			if run.State != pipeline.RunSucceeded {
				t.Fatalf("run state=%s halt=%s reason=%s", run.State, run.HaltStage, run.HaltReason)
			}
			break
		}
	}
	for path, want := range map[string]string{outA: "A:" + keyA + "-present," + keyB + "-absent", outB: "B:" + keyB + "-present," + keyA + "-absent"} {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != want {
			t.Fatalf("output %s = %q err=%v, want %q", path, data, err, want)
		}
	}
	for _, stageID := range []string{"a", "b"} {
		stage := stageRow(t, store, runID, stageID)
		job, err := store.GetJob(ctx, stage.JobID)
		if err != nil {
			t.Fatal(err)
		}
		payload, err := workflow.ParseJobPayload(job.Payload)
		if err != nil {
			t.Fatal(err)
		}
		wantKey := keyA
		if stageID == "b" {
			wantKey = keyB
		}
		if !reflect.DeepEqual(payload.PipelineEnvKeys, []string{wantKey}) || payload.PipelineEnvFile != envFile {
			t.Fatalf("stage %s audit payload = keys:%#v file:%q", stageID, payload.PipelineEnvKeys, payload.PipelineEnvFile)
		}
		if strings.Contains(job.Payload, pipelineEnvSecretA) || strings.Contains(job.Payload, pipelineEnvSecretB) {
			t.Fatalf("stage %s persisted a secret value", stageID)
		}
		events, err := store.ListJobEvents(ctx, stage.JobID)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			if strings.Contains(event.Message, pipelineEnvSecretA) || strings.Contains(event.Message, pipelineEnvSecretB) {
				t.Fatalf("stage %s event %q persisted a secret value", stageID, event.Kind)
			}
		}
	}
	for _, path := range []string{paths.Database, paths.Database + "-wal"} {
		data, err := os.ReadFile(path)
		if err == nil && (bytes.Contains(data, []byte(pipelineEnvSecretA)) || bytes.Contains(data, []byte(pipelineEnvSecretB))) {
			t.Fatalf("secret value persisted in %s", path)
		}
	}
	if err := filepath.WalkDir(paths.Logs, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(data, []byte(pipelineEnvSecretA)) || bytes.Contains(data, []byte(pipelineEnvSecretB)) {
			return fmt.Errorf("secret value persisted in log %s", path)
		}
		return nil
	}); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

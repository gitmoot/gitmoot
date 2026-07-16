package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	dashboard "github.com/gitmoot/gitmoot-dashboard"

	"github.com/gitmoot/gitmoot/internal/credgw"
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
		name   string
		setup  func(t *testing.T, home string) (envFile, envBody, stage string)
		want   string
		enable bool
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
			want:   `env_keys entry "MISSING" is unresolved`,
			enable: true,
		},
		{
			name: "agent env file key unresolved",
			setup: func(t *testing.T, _ string) (string, string, string) {
				return writePipelineEnvFile(t, t.TempDir(), "TOKEN="+pipelineEnvSecretA+"\n", 0o600), "", "{id: run, agent: scout, action: ask, prompt: inspect, env_keys: [TOKEN]}"
			},
			want:   "gitmoot key grant TOKEN --agent scout",
			enable: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			envFile, extra, stage := tt.setup(t, home)
			raw := fmt.Sprintf("name: env-validation\nenv_file: %q\n%s\nstages:\n  - %s\n", envFile, extra, stage)
			specFile := writeSpec(t, raw)
			var stdout, stderr bytes.Buffer
			args := []string{"pipeline", "add", specFile, "--home", home}
			if tt.enable {
				args = append(args, "--enable")
			}
			code := Run(args, &stdout, &stderr)
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

func TestClassifyPipelineEnvFileStatuses(t *testing.T) {
	home := dashboardTestHome(t)
	store := openPipelineTestStore(t, home)
	defer store.Close()

	insideHome := filepath.Join(home, ".gitmoot", "inside.env")
	if err := os.WriteFile(insideHome, []byte("BROKEN"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name          string
		path          func(t *testing.T) string
		want          string
		wrongOwnerUID bool
	}{
		{name: "none", path: func(*testing.T) string { return "" }, want: pipelineEnvFileStatusNone},
		{name: "ok", path: func(t *testing.T) string {
			return writePipelineEnvFile(t, t.TempDir(), "ALPHA=secret\nBETA=secret\n", 0o600)
		}, want: pipelineEnvFileStatusOK},
		{name: "missing", path: func(t *testing.T) string { return filepath.Join(t.TempDir(), "missing.env") }, want: pipelineEnvFileStatusMissing},
		{name: "bad mode before parse", path: func(t *testing.T) string {
			return writePipelineEnvFile(t, t.TempDir(), "BROKEN", 0o644)
		}, want: pipelineEnvFileStatusBadMode},
		{name: "bad owner before parse", path: func(t *testing.T) string {
			return writePipelineEnvFile(t, t.TempDir(), "BROKEN", 0o600)
		}, want: pipelineEnvFileStatusBadOwner, wrongOwnerUID: true},
		{name: "bad location before parse", path: func(*testing.T) string { return insideHome }, want: pipelineEnvFileStatusBadLocation},
		{name: "invalid parse", path: func(t *testing.T) string {
			return writePipelineEnvFile(t, t.TempDir(), "BROKEN", 0o600)
		}, want: pipelineEnvFileStatusInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			originalUID := pipelineEnvCurrentUID
			if tt.wrongOwnerUID {
				pipelineEnvCurrentUID = func() uint32 { return originalUID() + 1 }
			}
			defer func() { pipelineEnvCurrentUID = originalUID }()

			got := classifyPipelineEnvFile(context.Background(), store, home, tt.path(t))
			if got.Status != tt.want {
				t.Fatalf("status = %q, want %q (inspection=%+v)", got.Status, tt.want, got)
			}
			if got.Names == nil {
				t.Fatal("Names is nil")
			}
			if tt.want == pipelineEnvFileStatusOK && !reflect.DeepEqual(got.Names, map[string]struct{}{"ALPHA": {}, "BETA": {}}) {
				t.Fatalf("ok names = %#v", got.Names)
			}
			if tt.want != pipelineEnvFileStatusOK && len(got.Names) != 0 {
				t.Fatalf("unsafe file exposed names: %#v", got.Names)
			}
		})
	}
}

type pipelineEnvCaptureAdapter struct {
	jobs    []runtime.Job
	deliver func(runtime.Job) (runtime.Result, error)
}

func (a *pipelineEnvCaptureAdapter) Deliver(_ context.Context, _ runtime.Agent, job runtime.Job) (runtime.Result, error) {
	a.jobs = append(a.jobs, job)
	if a.deliver != nil {
		return a.deliver(job)
	}
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

func TestPipelineKeyAccessResolutionPrecedenceAndGrantBoundary(t *testing.T) {
	ctx := context.Background()
	home, _, store := heartbeatLoopE2EHome(t)
	writeDefaultKeychain(t, home, "OVERLAP=shared-overlap\nSHARED_ONLY="+pipelineEnvSecretA+"\nUNGRANTED="+pipelineEnvSecretB+"\n")
	if err := store.CreateOrUpdatePipeline(ctx, db.Pipeline{Name: "sources", SpecYAML: "name: sources\nstages: [{id: run, cmd: echo}]\n"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"OVERLAP", "SHARED_ONLY", "UNGRANTED"} {
		if _, err := store.AddKeychainKey(ctx, name, db.KeychainModeInjected); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"OVERLAP", "SHARED_ONLY"} {
		if _, err := store.GrantKeychainKey(ctx, db.KeychainConsumerPipeline, "sources", name); err != nil {
			t.Fatal(err)
		}
	}
	ownFile := writePipelineEnvFile(t, t.TempDir(), "OVERLAP=own-overlap\nOWN_ONLY=own\n", 0o600)
	spec := pipeline.Spec{
		Name:    "sources",
		EnvFile: ownFile,
		Env: map[string]string{
			"OVERLAP":      "default-overlap",
			"SHARED_ONLY":  "default-shared",
			"DEFAULT_ONLY": "default-only",
		},
		Stages: []pipeline.Stage{{ID: "run", Cmd: "echo", EnvKeys: []string{"OVERLAP", "SHARED_ONLY", "DEFAULT_ONLY"}}},
	}
	resolution, err := resolvePipelineEnvironment(ctx, store, home, spec)
	if err != nil {
		t.Fatal(err)
	}
	want := []workflow.PipelineKeyAccess{
		{Stage: "run", Name: "OVERLAP", Source: pipelineKeySourceOwn, Mode: db.KeychainModeInjected},
		{Stage: "run", Name: "SHARED_ONLY", Source: pipelineKeySourceShared, Mode: db.KeychainModeInjected},
		{Stage: "run", Name: "DEFAULT_ONLY", Source: pipelineKeySourceDefault, Mode: db.KeychainModeInjected},
	}
	if !reflect.DeepEqual(resolution.Access, want) || len(resolution.Unresolved) != 0 {
		t.Fatalf("resolution=%#v unresolved=%#v, want %#v", resolution.Access, resolution.Unresolved, want)
	}

	for _, selector := range []string{"UNGRANTED", "UNGRANTED*"} {
		probe := spec
		probe.Stages = []pipeline.Stage{{ID: "run", Cmd: "echo", EnvKeys: []string{selector}}}
		got, err := resolvePipelineEnvironment(ctx, store, home, probe)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Access) != 0 || !reflect.DeepEqual(got.Unresolved, []pipelineEnvUnresolved{{Stage: "run", Selector: selector}}) {
			t.Fatalf("selector %q resolved without a grant: access=%#v unresolved=%#v", selector, got.Access, got.Unresolved)
		}
	}

	raw := fmt.Sprintf("name: sources\nenv_file: %q\nenv:\n  OVERLAP: default-overlap\n  SHARED_ONLY: default-shared\n  DEFAULT_ONLY: default-only\nstages:\n  - id: run\n    cmd: echo\n    env_keys: [OVERLAP, SHARED_ONLY, DEFAULT_ONLY]\n", ownFile)
	if err := store.CreateOrUpdatePipeline(ctx, db.Pipeline{Name: "sources", SpecYAML: raw, SpecHash: pipeline.Hash([]byte(raw))}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"pipeline", "show", "sources", "--home", home, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("pipeline show code=%d stderr=%q", code, stderr.String())
	}
	var shown pipelineJSON
	if err := json.Unmarshal(stdout.Bytes(), &shown); err != nil {
		t.Fatal(err)
	}
	if shown.EnvFile != ownFile || !reflect.DeepEqual(shown.KeyAccess, want) || !reflect.DeepEqual(shown.Stages[0].EnvKeys, spec.Stages[0].EnvKeys) {
		t.Fatalf("pipeline show projection=%+v", shown)
	}
	if err := os.WriteFile(ownFile, []byte("OWN_ONLY=own\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	capture := &pipelineEnvCaptureAdapter{}
	pinnedOwn := wrapPipelineEnvDeliveryAdapter(store, home, workflow.JobPayload{
		PipelineName: "sources", PipelineEnvFile: ownFile, PipelineKeyAccess: []workflow.PipelineKeyAccess{want[0]},
	}, capture)
	if _, err := pinnedOwn.Deliver(ctx, runtime.Agent{}, runtime.Job{}); err == nil || !strings.Contains(err.Error(), "source own") {
		t.Fatalf("disappeared own source silently switched to shared: err=%v", err)
	}
	if len(capture.jobs) != 0 {
		t.Fatalf("disappeared own source reached delivery: %#v", capture.jobs)
	}
}

func TestPipelineAgentKeyResolutionRequiresSeatGrantAndProxiedMode(t *testing.T) {
	ctx := context.Background()
	home, _, store := heartbeatLoopE2EHome(t)
	writeDefaultKeychain(t, home, strings.Join([]string{
		"AGENT_PROXY=agent-proxy-value",
		"PIPELINE_ONLY=pipeline-proxy-value",
		"AGENT_INJECTED=injected-value",
		"OWN_ONLY=own-value",
	}, "\n")+"\n")
	if err := store.CreateOrUpdatePipeline(ctx, db.Pipeline{Name: "agent-sources", SpecYAML: "name: agent-sources\nstages: [{id: shell, cmd: echo}]\n"}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAgent(ctx, db.Agent{Name: "scout", Runtime: "codex"}); err != nil {
		t.Fatal(err)
	}
	for _, key := range []struct {
		name string
		mode string
	}{
		{name: "AGENT_PROXY", mode: db.KeychainModeProxied},
		{name: "PIPELINE_ONLY", mode: db.KeychainModeProxied},
		{name: "AGENT_INJECTED", mode: db.KeychainModeInjected},
	} {
		if _, err := store.AddKeychainKey(ctx, key.name, key.mode); err != nil {
			t.Fatal(err)
		}
		if key.mode == db.KeychainModeProxied {
			if _, err := store.ConfigureKeychainProxy(ctx, key.name, "https://api.example.test/v1", db.KeychainProxyAuthBearer, ""); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := store.GrantKeychainKey(ctx, db.KeychainConsumerAgent, "scout", "AGENT_PROXY"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GrantKeychainKey(ctx, db.KeychainConsumerPipeline, "agent-sources", "PIPELINE_ONLY"); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", store.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.ExecContext(ctx, `INSERT INTO keychain_grants(consumer_kind, consumer_id, key_name) VALUES (?, ?, ?)`, db.KeychainConsumerAgent, "scout", "AGENT_INJECTED"); err != nil {
		t.Fatal(err)
	}
	envFile := writePipelineEnvFile(t, t.TempDir(), "OWN_ONLY=own-value\n", 0o600)
	spec := pipeline.Spec{
		Name: "agent-sources", EnvFile: envFile, Env: map[string]string{"DEFAULT_ONLY": "default"},
		Stages: []pipeline.Stage{
			{ID: "agent", Agent: "scout", Action: "ask", Prompt: "inspect", EnvKeys: []string{"AGENT_PROXY", "PIPELINE_ONLY", "AGENT_INJECTED", "OWN_ONLY", "DEFAULT_ONLY"}},
			{ID: "shell", Cmd: "echo", EnvKeys: []string{"PIPELINE_ONLY", "DEFAULT_ONLY"}},
		},
	}
	resolution, err := resolvePipelineEnvironment(ctx, store, home, spec)
	if err != nil {
		t.Fatal(err)
	}
	wantAccess := []workflow.PipelineKeyAccess{
		{Stage: "agent", Name: "AGENT_PROXY", Source: pipelineKeySourceShared, Mode: db.KeychainModeProxied},
		{Stage: "shell", Name: "PIPELINE_ONLY", Source: pipelineKeySourceShared, Mode: db.KeychainModeProxied},
		{Stage: "shell", Name: "DEFAULT_ONLY", Source: pipelineKeySourceDefault, Mode: db.KeychainModeInjected},
	}
	wantUnresolved := []pipelineEnvUnresolved{
		{Stage: "agent", Selector: "PIPELINE_ONLY"},
		{Stage: "agent", Selector: "AGENT_INJECTED"},
		{Stage: "agent", Selector: "OWN_ONLY"},
		{Stage: "agent", Selector: "DEFAULT_ONLY"},
	}
	if !reflect.DeepEqual(resolution.Access, wantAccess) || !reflect.DeepEqual(resolution.Unresolved, wantUnresolved) {
		t.Fatalf("resolution access=%#v unresolved=%#v", resolution.Access, resolution.Unresolved)
	}
	if err := pipelineEnvironmentResolutionError(spec, []pipelineEnvUnresolved{{Stage: "agent", Selector: "MISSING"}}); err == nil || !strings.Contains(err.Error(), "gitmoot key grant MISSING --agent scout") {
		t.Fatalf("agent unresolved hint = %v", err)
	}
}

func TestPipelineOwnKeyDoesNotRequireLowerPriorityKeychain(t *testing.T) {
	ctx := context.Background()
	home, _, store := heartbeatLoopE2EHome(t)
	if err := store.CreateOrUpdatePipeline(ctx, db.Pipeline{Name: "own-only", SpecYAML: "name: own-only\nstages: [{id: run, cmd: echo}]\n"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddKeychainKey(ctx, "OVERLAP", db.KeychainModeInjected); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GrantKeychainKey(ctx, db.KeychainConsumerPipeline, "own-only", "OVERLAP"); err != nil {
		t.Fatal(err)
	}
	envFile := writePipelineEnvFile(t, t.TempDir(), "OVERLAP=own\n", 0o600)
	spec := pipeline.Spec{
		Name: "own-only", EnvFile: envFile,
		Stages: []pipeline.Stage{{ID: "run", Cmd: "echo", EnvKeys: []string{"OVERLAP"}}},
	}
	resolution, err := resolvePipelineEnvironment(ctx, store, home, spec)
	if err != nil {
		t.Fatalf("own source consulted missing lower-priority keychain: %v", err)
	}
	want := []workflow.PipelineKeyAccess{{Stage: "run", Name: "OVERLAP", Source: pipelineKeySourceOwn, Mode: db.KeychainModeInjected}}
	if !reflect.DeepEqual(resolution.Access, want) || len(resolution.Unresolved) != 0 {
		t.Fatalf("resolution=%#v unresolved=%#v", resolution.Access, resolution.Unresolved)
	}
}

func TestPipelineSharedKeyDeliveryRotationAndRevocation(t *testing.T) {
	ctx := context.Background()
	home, _, store := heartbeatLoopE2EHome(t)
	path := writeDefaultKeychain(t, home, "SHARED="+pipelineEnvSecretA+"\n")
	if err := store.CreateOrUpdatePipeline(ctx, db.Pipeline{Name: "shared-delivery", SpecYAML: "name: shared-delivery\nstages: [{id: run, cmd: echo}]\n"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddKeychainKey(ctx, "SHARED", db.KeychainModeInjected); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GrantKeychainKey(ctx, db.KeychainConsumerPipeline, "shared-delivery", "SHARED"); err != nil {
		t.Fatal(err)
	}
	spec := pipeline.Spec{Name: "shared-delivery", Stages: []pipeline.Stage{{ID: "run", Cmd: "echo", EnvKeys: []string{"SHARED"}}}}
	access, err := resolvePipelineStageEnvAccess(ctx, store, home, spec, spec.Stages[0])
	if err != nil {
		t.Fatal(err)
	}
	capture := &pipelineEnvCaptureAdapter{}
	wrapped := wrapPipelineEnvDeliveryAdapter(store, home, workflow.JobPayload{
		PipelineName: "shared-delivery", PipelineKeyAccess: access.Access,
	}, capture)
	if _, err := wrapped.Deliver(ctx, runtime.Agent{}, runtime.Job{}); err != nil {
		t.Fatal(err)
	}
	if got := capture.jobs[0].ShellEnv; !reflect.DeepEqual(got, []string{"SHARED=" + pipelineEnvSecretA}) {
		t.Fatalf("first shared delivery=%#v", got)
	}
	if err := os.WriteFile(path, []byte("SHARED=rotated-shared-874\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := wrapped.Deliver(ctx, runtime.Agent{}, runtime.Job{}); err != nil {
		t.Fatal(err)
	}
	if got := capture.jobs[1].ShellEnv; !reflect.DeepEqual(got, []string{"SHARED=rotated-shared-874"}) {
		t.Fatalf("rotated shared delivery=%#v", got)
	}
	if _, err := store.RevokeKeychainKey(ctx, db.KeychainConsumerPipeline, "shared-delivery", "SHARED"); err != nil {
		t.Fatal(err)
	}
	if _, err := wrapped.Deliver(ctx, runtime.Agent{}, runtime.Job{}); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("revoked grant delivery error=%v", err)
	}
	if len(capture.jobs) != 2 {
		t.Fatalf("revoked grant reached inner adapter: jobs=%d", len(capture.jobs))
	}
}

func TestPipelineProxiedKeyResolutionAndDeliveryLease(t *testing.T) {
	ctx := context.Background()
	home, _, store := heartbeatLoopE2EHome(t)
	writeDefaultKeychain(t, home, "PROXY_KEY="+pipelineEnvSecretA+"\n")
	if err := store.CreateOrUpdatePipeline(ctx, db.Pipeline{Name: "proxy-delivery", SpecYAML: "name: proxy-delivery\nstages: [{id: run, cmd: echo}]\n"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddKeychainKey(ctx, "PROXY_KEY", db.KeychainModeProxied); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ConfigureKeychainProxy(ctx, "PROXY_KEY", "https://api.example.test/v1", db.KeychainProxyAuthBearer, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GrantKeychainKey(ctx, db.KeychainConsumerPipeline, "proxy-delivery", "PROXY_KEY"); err != nil {
		t.Fatal(err)
	}
	spec := pipeline.Spec{Name: "proxy-delivery", Stages: []pipeline.Stage{{ID: "run", Cmd: "echo", EnvKeys: []string{"PROXY_KEY"}}}}
	access, err := resolvePipelineStageEnvAccess(ctx, store, home, spec, spec.Stages[0])
	if err != nil {
		t.Fatal(err)
	}
	want := []workflow.PipelineKeyAccess{{Stage: "run", Name: "PROXY_KEY", Source: pipelineKeySourceShared, Mode: db.KeychainModeProxied}}
	if !reflect.DeepEqual(access.Access, want) {
		t.Fatalf("proxied access = %#v, want %#v", access.Access, want)
	}

	previousRegistry := modelGatewayRegistry
	previousLogf := modelGatewayLogf
	modelGatewayRegistry = credgw.NewRegistry()
	modelGatewayLogf = func(string, ...any) {}
	paths, err := configPathsForPipelineStore(store, home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = modelGatewayRegistry.CloseHome(context.Background(), paths.Home)
		modelGatewayRegistry = previousRegistry
		modelGatewayLogf = previousLogf
	})
	capture := &pipelineEnvCaptureAdapter{}
	wrapper := wrapPipelineEnvDeliveryAdapter(store, home, workflow.JobPayload{
		PipelineName: "proxy-delivery", PipelineKeyAccess: access.Access,
	}, capture)
	if _, err := wrapper.Deliver(ctx, runtime.Agent{}, runtime.Job{ID: "proxy-delivery-job"}); err != nil {
		t.Fatal(err)
	}
	if len(capture.jobs) != 1 {
		t.Fatalf("deliveries = %d", len(capture.jobs))
	}
	env := capture.jobs[0].ShellEnv
	placeholder := envEntryValue(env, "PROXY_KEY")
	leaseURL := envEntryValue(env, "GITMOOT_PROXY_PROXY_KEY_URL")
	if !strings.HasPrefix(placeholder, "gitmoot-kc-proxy-delivery-job-") || !strings.HasPrefix(leaseURL, "http://127.0.0.1:") {
		t.Fatalf("proxied shell env = %#v", env)
	}
	if strings.Contains(strings.Join(env, "\n"), pipelineEnvSecretA) {
		t.Fatalf("real proxied value reached ShellEnv: %#v", env)
	}
	request, _ := http.NewRequest(http.MethodGet, leaseURL, nil)
	request.Header.Set("Authorization", "Bearer "+placeholder)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("deferred lease revocation status = %d", response.StatusCode)
	}
}

func TestPipelineAgentProxiedKeyDeliveryE2E(t *testing.T) {
	ctx := context.Background()
	home, _, store := heartbeatLoopE2EHome(t)
	const (
		keyName      = "AGENT_PROXY"
		real         = "agent-real-value-874-pr2"
		pipelineName = "agent-proxy-e2e"
		seatName     = "scout"
	)
	keychainPath := writeDefaultKeychain(t, home, keyName+"="+real+"\n")
	specYAML := "name: " + pipelineName + "\nrepo: owner/repo\nstages: [{id: inspect, agent: " + seatName + ", action: ask, prompt: inspect, env_keys: [" + keyName + "]}]\n"
	if err := store.CreateOrUpdatePipeline(ctx, db.Pipeline{Name: pipelineName, Repo: "owner/repo", SpecYAML: specYAML}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAgent(ctx, db.Agent{Name: seatName, Runtime: runtime.CodexRuntime}); err != nil {
		t.Fatal(err)
	}
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		if got := r.Header.Get("Authorization"); got != "Bearer "+real {
			t.Errorf("upstream Authorization = %q, want real credential", got)
		}
		if strings.Contains(r.Header.Get("Authorization"), "gitmoot-kc-") {
			t.Errorf("upstream received placeholder: %q", r.Header.Get("Authorization"))
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	if _, err := store.AddKeychainKey(ctx, keyName, db.KeychainModeProxied); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ConfigureKeychainProxy(ctx, keyName, upstream.URL+"/v1", db.KeychainProxyAuthBearer, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GrantKeychainKey(ctx, db.KeychainConsumerAgent, seatName, keyName); err != nil {
		t.Fatal(err)
	}
	spec := pipeline.Spec{Name: pipelineName, Repo: "owner/repo", Stages: []pipeline.Stage{{ID: "inspect", Agent: seatName, Action: "ask", Prompt: "inspect", EnvKeys: []string{keyName}}}}
	access, err := resolvePipelineStageEnvAccess(ctx, store, home, spec, spec.Stages[0])
	if err != nil {
		t.Fatal(err)
	}
	wantAccess := []workflow.PipelineKeyAccess{{Stage: "inspect", Name: keyName, Source: pipelineKeySourceShared, Mode: db.KeychainModeProxied}}
	if !reflect.DeepEqual(access.Access, wantAccess) {
		t.Fatalf("agent access = %#v, want %#v", access.Access, wantAccess)
	}
	rec, found, err := store.GetPipeline(ctx, pipelineName)
	if err != nil || !found {
		t.Fatalf("GetPipeline: found=%t err=%v", found, err)
	}
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	run, err := createPipelineRun(ctx, store, rec, spec, "manual", "{}", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := advancePipelineRun(ctx, store, testStageEnqueuer(store), rec, spec, run, now); err != nil {
		t.Fatal(err)
	}
	stageRow, ok, err := store.GetPipelineRunStage(ctx, run.ID, "inspect")
	if err != nil || !ok || stageRow.JobID == "" {
		t.Fatalf("agent stage row: ok=%t row=%+v err=%v", ok, stageRow, err)
	}
	queued, err := store.GetJob(ctx, stageRow.JobID)
	if err != nil {
		t.Fatal(err)
	}
	queuedPayload, err := workflow.ParseJobPayload(queued.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if queuedPayload.PipelineName != pipelineName || queuedPayload.PipelineKeyAgent != seatName || !reflect.DeepEqual(queuedPayload.PipelineKeyAccess, wantAccess) {
		t.Fatalf("queued names-only authority = %+v", queuedPayload)
	}
	events, err := store.ListJobEvents(ctx, queued.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if strings.Contains(event.Message, real) {
			t.Fatalf("real credential persisted in event %q", event.Kind)
		}
	}
	detail, err := (&webDataSource{home: home}).PipelineDetail(ctx, pipelineName)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Keys.Stages) != 1 || !reflect.DeepEqual(detail.Keys.Stages[0].Keys, []dashboard.PipelineKeyEntry{{Name: keyName, Source: pipelineKeySourceShared, Mode: db.KeychainModeProxied}}) {
		t.Fatalf("agent Keys projection = %+v", detail.Keys)
	}
	detailJSON, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(detailJSON), real) {
		t.Fatalf("dashboard projection contains real credential: %s", detailJSON)
	}
	payload := workflow.JobPayload{PipelineName: pipelineName, PipelineKeyAgent: seatName, PipelineKeyAccess: access.Access}
	persisted, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(persisted), real) || strings.Contains(string(persisted), "gitmoot-kc-") {
		t.Fatalf("persisted payload contains credential material: %s", persisted)
	}

	previousRegistry := modelGatewayRegistry
	previousLogf := modelGatewayLogf
	previousLoopback := pipelineProxyAllowLoopbackHTTP
	modelGatewayRegistry = credgw.NewRegistry()
	modelGatewayLogf = func(string, ...any) {}
	pipelineProxyAllowLoopbackHTTP = true
	paths, err := configPathsForPipelineStore(store, home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = modelGatewayRegistry.CloseHome(context.Background(), paths.Home)
		modelGatewayRegistry = previousRegistry
		modelGatewayLogf = previousLogf
		pipelineProxyAllowLoopbackHTTP = previousLoopback
	})
	capture := &pipelineEnvCaptureAdapter{deliver: func(job runtime.Job) (runtime.Result, error) {
		if len(job.ShellEnv) != 0 {
			t.Fatalf("agent delivery populated ShellEnv: %#v", job.ShellEnv)
		}
		placeholder := envEntryValue(job.AgentEnv, keyName)
		leaseURL := envEntryValue(job.AgentEnv, "GITMOOT_PROXY_"+keyName+"_URL")
		if !strings.HasPrefix(placeholder, "gitmoot-kc-agent-proxy-job-") || !strings.HasPrefix(leaseURL, "http://127.0.0.1:") {
			t.Fatalf("agent env = %#v", job.AgentEnv)
		}
		if strings.Contains(strings.Join(job.AgentEnv, "\n"), real) {
			t.Fatalf("real credential reached AgentEnv: %#v", job.AgentEnv)
		}
		req, err := http.NewRequest(http.MethodGet, leaseURL+"/models", nil)
		if err != nil {
			return runtime.Result{}, err
		}
		req.Header.Set("Authorization", "Bearer "+placeholder)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return runtime.Result{}, err
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			return runtime.Result{}, fmt.Errorf("proxy status %d", resp.StatusCode)
		}
		return runtime.Result{Raw: "ok"}, nil
	}}
	wrapper := wrapPipelineEnvDeliveryAdapter(store, home, payload, capture)
	result, err := wrapper.Deliver(ctx, runtime.Agent{}, runtime.Job{ID: "agent-proxy-job"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result.Raw, real) {
		t.Fatalf("delivery result contains real credential: %q", result.Raw)
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstream calls = %d, want 1", upstreamCalls)
	}

	if _, err := store.RevokeKeychainKey(ctx, db.KeychainConsumerAgent, seatName, keyName); err != nil {
		t.Fatal(err)
	}
	if _, err := wrapper.Deliver(ctx, runtime.Agent{}, runtime.Job{ID: "agent-proxy-revoked"}); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("revoked agent grant delivery error = %v", err)
	}
	if len(capture.jobs) != 1 || upstreamCalls != 1 {
		t.Fatalf("revoked delivery reached child/upstream: jobs=%d upstream=%d", len(capture.jobs), upstreamCalls)
	}

	for _, file := range []string{store.DatabasePath(), store.DatabasePath() + "-wal"} {
		body, readErr := os.ReadFile(file)
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			t.Fatal(readErr)
		}
		if bytes.Contains(body, []byte(real)) {
			t.Fatalf("real credential persisted in %s", file)
		}
	}
	keychainBody, err := os.ReadFile(keychainPath)
	if err != nil || !bytes.Contains(keychainBody, []byte(real)) {
		t.Fatalf("operator keychain unexpectedly changed: err=%v body=%q", err, keychainBody)
	}
}

func TestPipelineUnconfiguredProxiedKeyFailsAtEnqueue(t *testing.T) {
	ctx := context.Background()
	home, _, store := heartbeatLoopE2EHome(t)
	writeDefaultKeychain(t, home, "PROXY_KEY="+pipelineEnvSecretA+"\n")
	if err := store.CreateOrUpdatePipeline(ctx, db.Pipeline{Name: "unconfigured-proxy", SpecYAML: "name: unconfigured-proxy\nstages: [{id: run, cmd: echo}]\n"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddKeychainKey(ctx, "PROXY_KEY", db.KeychainModeProxied); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", store.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.ExecContext(ctx, `INSERT INTO keychain_grants(consumer_kind, consumer_id, key_name) VALUES (?, ?, ?)`, db.KeychainConsumerPipeline, "unconfigured-proxy", "PROXY_KEY"); err != nil {
		t.Fatal(err)
	}
	spec := pipeline.Spec{Name: "unconfigured-proxy", Stages: []pipeline.Stage{{ID: "run", Cmd: "echo", EnvKeys: []string{"PROXY_KEY"}}}}
	_, err = resolvePipelineStageEnvAccess(ctx, store, home, spec, spec.Stages[0])
	if err == nil || !strings.Contains(err.Error(), "gitmoot key configure PROXY_KEY") {
		t.Fatalf("unconfigured proxied resolution error = %v", err)
	}
}

func envEntryValue(env []string, name string) string {
	prefix := name + "="
	for i := len(env) - 1; i >= 0; i-- {
		if strings.HasPrefix(env[i], prefix) {
			return strings.TrimPrefix(env[i], prefix)
		}
	}
	return ""
}

func TestPipelineEnvironmentValidationTiming(t *testing.T) {
	home := t.TempDir()
	specFile := writeSpec(t, "name: unresolved-env\nrepo: owner/repo\nstages: [{id: run, cmd: echo, env_keys: [MISSING]}]\n")
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"pipeline", "add", specFile, "--home", home}, &stdout, &stderr); code != 0 || !strings.Contains(stderr.String(), "warning:") || !strings.Contains(stderr.String(), "gitmoot key grant MISSING --pipeline unresolved-env") {
		t.Fatalf("disabled add code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	for _, args := range [][]string{
		{"pipeline", "enable", "unresolved-env", "--home", home},
		{"pipeline", "run", "unresolved-env", "--home", home},
	} {
		stdout.Reset()
		stderr.Reset()
		if code := Run(args, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "gitmoot key grant MISSING --pipeline unresolved-env") {
			t.Fatalf("%v code=%d stdout=%q stderr=%q", args, code, stdout.String(), stderr.String())
		}
	}

	enabledSpec := writeSpec(t, "name: unresolved-enabled\nrepo: owner/repo\nstages: [{id: run, cmd: echo, env_keys: [MISSING]}]\n")
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"pipeline", "add", enabledSpec, "--enable", "--home", home}, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "gitmoot key grant MISSING --pipeline unresolved-enabled") {
		t.Fatalf("enabled add code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if err := withStore(home, func(store *db.Store) error {
		_, found, err := store.GetPipeline(context.Background(), "unresolved-enabled")
		if err == nil && found {
			return fmt.Errorf("unresolved enabled pipeline was persisted")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}

	agentHome := t.TempDir()
	if err := withStore(agentHome, func(store *db.Store) error {
		return store.UpsertAgent(context.Background(), db.Agent{Name: "scout", Runtime: runtime.CodexRuntime})
	}); err != nil {
		t.Fatal(err)
	}
	agentSpec := writeSpec(t, "name: unresolved-agent-env\nrepo: owner/repo\nstages: [{id: inspect, agent: scout, action: ask, prompt: inspect, env_keys: [MISSING]}]\n")
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"pipeline", "add", agentSpec, "--home", agentHome}, &stdout, &stderr); code != 0 || !strings.Contains(stderr.String(), "warning:") || !strings.Contains(stderr.String(), "gitmoot key grant MISSING --agent scout") {
		t.Fatalf("disabled agent add code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	for _, args := range [][]string{
		{"pipeline", "enable", "unresolved-agent-env", "--home", agentHome},
		{"pipeline", "run", "unresolved-agent-env", "--home", agentHome},
	} {
		stdout.Reset()
		stderr.Reset()
		if code := Run(args, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "gitmoot key grant MISSING --agent scout") {
			t.Fatalf("%v code=%d stdout=%q stderr=%q", args, code, stdout.String(), stderr.String())
		}
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

func TestPipelineKeychainRegistryGrantsShellE2E(t *testing.T) {
	ctx := context.Background()
	home, paths, store := heartbeatLoopE2EHome(t)
	checkout := createDaemonWorkerGitCheckout(t, "main")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	const (
		sharedA  = "KEYCARD_SHARED_A"
		sharedB  = "KEYCARD_SHARED_B"
		override = "KEYCARD_OVERRIDE"
		revoked  = "KEYCARD_REVOKED"
		unused   = "KEYCARD_UNGRANTED"
		oldA     = "registry-old-a-874"
		oldB     = "registry-old-b-874"
		rotatedB = "registry-rotated-b-874"
		sharedO  = "registry-shared-override-874"
		ownO     = "pipeline-own-override-874"
		revokedV = "registry-revoked-874"
		unusedV  = "registry-ungranted-874"
	)
	keychainPath := writeDefaultKeychain(t, home, fmt.Sprintf("%s=%s\n%s=%s\n%s=%s\n%s=%s\n%s=%s\n", sharedA, oldA, sharedB, oldB, override, sharedO, revoked, revokedV, unused, unusedV))
	ownFile := writePipelineEnvFile(t, t.TempDir(), override+"="+ownO+"\n", 0o600)
	outA := filepath.Join(t.TempDir(), "shared-a.txt")
	outB := filepath.Join(t.TempDir(), "shared-b.txt")
	outRevoked := filepath.Join(t.TempDir(), "revoked.txt")
	specYAML := fmt.Sprintf(`name: keycard-e2e
repo: owner/repo
env_file: %q
stages:
  - id: a
    cmd: |
      test -n "$%s"; test -n "$%s"; test -z "${%s+x}"; printf '%%s|%%s' "$%s" "$%s" > %q; %s
    env_keys: [%s, %s]
  - id: b
    cmd: |
      test -n "$%s"; test -z "${%s+x}"; printf '%%s' "$%s" > %q; %s
    env_keys: [%s]
    needs: [a]
  - id: revoked
    cmd: |
      printf '%%s' "$%s" > %q; %s
    env_keys: [%s]
    needs: [b]
`, ownFile, sharedA, override, sharedB, sharedA, override, outA, pipelineShellResultCommand(t, "approved", "a"), sharedA, override, sharedB, sharedA, sharedB, outB, pipelineShellResultCommand(t, "approved", "b"), sharedB, revoked, outRevoked, pipelineShellResultCommand(t, "approved", "revoked"), revoked)
	specFile := writeSpec(t, specYAML)
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"pipeline", "add", specFile, "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("pipeline add code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	for _, name := range []string{sharedA, sharedB, override, revoked, unused} {
		stdout.Reset()
		stderr.Reset()
		if code := Run([]string{"key", "add", name, "--mode", "injected", "--home", home}, &stdout, &stderr); code != 0 {
			t.Fatalf("key add %s code=%d stdout=%q stderr=%q", name, code, stdout.String(), stderr.String())
		}
	}
	for _, name := range []string{sharedA, sharedB, override, revoked} {
		stdout.Reset()
		stderr.Reset()
		if code := Run([]string{"key", "grant", name, "--pipeline", "keycard-e2e", "--home", home}, &stdout, &stderr); code != 0 {
			t.Fatalf("key grant %s code=%d stdout=%q stderr=%q", name, code, stdout.String(), stderr.String())
		}
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"pipeline", "run", "keycard-e2e", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("pipeline run code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	runID := strings.TrimSpace(stdout.String())
	enqueue := newPipelineStageEnqueuer(store, home)
	worker := defaultJobWorker(store, io.Discard, home)
	now := time.Date(2026, 7, 16, 15, 0, 0, 0, time.UTC)
	rotated := false
	revokedAfterEnqueue := false
	for i := 0; i < 12; i++ {
		if err := runEnabledRepoWorkerTicks(ctx, store, worker, 1, io.Discard, now); err != nil {
			t.Fatalf("worker tick %d: %v", i, err)
		}
		if !rotated {
			row := stageRow(t, store, runID, "a")
			if row.JobID != "" {
				job, err := store.GetJob(ctx, row.JobID)
				if err != nil {
					t.Fatal(err)
				}
				if job.State == string(workflow.JobSucceeded) {
					rotatedBody := fmt.Sprintf("%s=%s\n%s=%s\n%s=%s\n%s=%s\n%s=%s\n", sharedA, oldA, sharedB, rotatedB, override, sharedO, revoked, revokedV, unused, unusedV)
					if err := os.WriteFile(keychainPath, []byte(rotatedBody), 0o600); err != nil {
						t.Fatal(err)
					}
					rotated = true
				}
			}
		}
		if err := runPipelineScanOnce(ctx, store, enqueue, now); err != nil {
			t.Fatalf("pipeline scan %d: %v", i, err)
		}
		if !revokedAfterEnqueue {
			row := stageRow(t, store, runID, "revoked")
			if row.JobID != "" {
				if _, err := store.RevokeKeychainKey(ctx, db.KeychainConsumerPipeline, "keycard-e2e", revoked); err != nil {
					t.Fatal(err)
				}
				revokedAfterEnqueue = true
			}
		}
		run, _, err := store.GetPipelineRun(ctx, runID)
		if err != nil {
			t.Fatal(err)
		}
		if run.State != pipeline.RunRunning {
			if run.State != pipeline.RunFailed {
				t.Fatalf("run state=%s halt=%s reason=%s", run.State, run.HaltStage, run.HaltReason)
			}
			break
		}
	}
	if !rotated || !revokedAfterEnqueue {
		t.Fatalf("rotation=%v revoke-after-enqueue=%v", rotated, revokedAfterEnqueue)
	}
	if data, err := os.ReadFile(outA); err != nil || string(data) != oldA+"|"+ownO {
		t.Fatalf("stage a output=%q err=%v", data, err)
	}
	if data, err := os.ReadFile(outB); err != nil || string(data) != rotatedB {
		t.Fatalf("stage b output=%q err=%v", data, err)
	}
	if _, err := os.Stat(outRevoked); !os.IsNotExist(err) {
		t.Fatalf("revoked stage executed: err=%v", err)
	}

	wantSources := map[string]map[string]string{
		"a":       {sharedA: pipelineKeySourceShared, override: pipelineKeySourceOwn},
		"b":       {sharedB: pipelineKeySourceShared},
		"revoked": {revoked: pipelineKeySourceShared},
	}
	for stageID, names := range wantSources {
		row := stageRow(t, store, runID, stageID)
		job, err := store.GetJob(ctx, row.JobID)
		if err != nil {
			t.Fatal(err)
		}
		payload, err := workflow.ParseJobPayload(job.Payload)
		if err != nil {
			t.Fatal(err)
		}
		if payload.PipelineName != "keycard-e2e" || len(payload.PipelineKeyAccess) != len(names) {
			t.Fatalf("stage %s payload=%+v", stageID, payload)
		}
		for _, access := range payload.PipelineKeyAccess {
			if names[access.Name] != access.Source || access.Stage != stageID || access.Mode != db.KeychainModeInjected {
				t.Fatalf("stage %s access=%+v want=%+v", stageID, payload.PipelineKeyAccess, names)
			}
		}
	}

	ungrantedSpec := writeSpec(t, "name: keycard-ungranted\nrepo: owner/repo\nstages: [{id: run, cmd: echo, env_keys: [KEYCARD_UNGRANTED]}]\n")
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"pipeline", "add", ungrantedSpec, "--home", home}, &stdout, &stderr); code != 0 || !strings.Contains(stderr.String(), "warning:") {
		t.Fatalf("ungranted add code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"pipeline", "run", "keycard-ungranted", "--home", home}, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "gitmoot key grant KEYCARD_UNGRANTED --pipeline keycard-ungranted") {
		t.Fatalf("ungranted run code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if runs, err := store.ListPipelineRuns(ctx, "keycard-ungranted"); err != nil || len(runs) != 0 {
		t.Fatalf("ungranted runs=%+v err=%v", runs, err)
	}

	sentinels := []string{oldA, oldB, rotatedB, sharedO, ownO, revokedV, unusedV}
	for _, stageID := range []string{"a", "b", "revoked"} {
		row := stageRow(t, store, runID, stageID)
		job, err := store.GetJob(ctx, row.JobID)
		if err != nil {
			t.Fatal(err)
		}
		for _, sentinel := range sentinels {
			if strings.Contains(job.Payload, sentinel) {
				t.Fatalf("stage %s payload persisted sentinel %q", stageID, sentinel)
			}
		}
		events, err := store.ListJobEvents(ctx, row.JobID)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			for _, sentinel := range sentinels {
				if strings.Contains(event.Message, sentinel) {
					t.Fatalf("stage %s event persisted sentinel %q", stageID, sentinel)
				}
			}
		}
	}
	for _, persistedPath := range []string{paths.Database, paths.Database + "-wal"} {
		data, err := os.ReadFile(persistedPath)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		for _, sentinel := range sentinels {
			if bytes.Contains(data, []byte(sentinel)) {
				t.Fatalf("sentinel %q persisted in %s", sentinel, persistedPath)
			}
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
		for _, sentinel := range sentinels {
			if bytes.Contains(data, []byte(sentinel)) {
				return fmt.Errorf("sentinel %q persisted in log %s", sentinel, path)
			}
		}
		return nil
	}); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestPipelineProxiedKeyShellE2E(t *testing.T) {
	ctx := context.Background()
	home, paths, store := heartbeatLoopE2EHome(t)
	checkout := createDaemonWorkerGitCheckout(t, "main")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	const (
		keyName  = "KEYCARD_PROXY_E2E"
		sentinel = "proxied-real-sentinel-874"
	)
	writeDefaultKeychain(t, home, keyName+"="+sentinel+"\n")

	var upstreamMu sync.Mutex
	upstreamCalls := 0
	var upstreamProblems []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamMu.Lock()
		upstreamCalls++
		if r.URL.Path != "/pinned/action" {
			upstreamProblems = append(upstreamProblems, "unexpected path "+r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+sentinel {
			upstreamProblems = append(upstreamProblems, "upstream did not receive the real bearer")
		}
		if strings.Contains(r.Header.Get("Authorization"), "gitmoot-kc-") {
			upstreamProblems = append(upstreamProblems, "upstream received the placeholder")
		}
		upstreamMu.Unlock()
		if _, err := store.RevokeKeychainKey(context.Background(), db.KeychainConsumerPipeline, "proxy-e2e", keyName); err != nil {
			upstreamMu.Lock()
			upstreamProblems = append(upstreamProblems, "revoke failed")
			upstreamMu.Unlock()
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	previousRegistry := modelGatewayRegistry
	previousLogf := modelGatewayLogf
	previousConfigureHTTP := keyConfigureAllowLoopbackHTTP
	previousDeliveryHTTP := pipelineProxyAllowLoopbackHTTP
	modelGatewayRegistry = credgw.NewRegistry()
	var gatewayLogs gatewayLogSink
	modelGatewayLogf = gatewayLogs.Logf
	keyConfigureAllowLoopbackHTTP = true
	pipelineProxyAllowLoopbackHTTP = true
	configPaths, err := configPathsForPipelineStore(store, home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = modelGatewayRegistry.CloseHome(context.Background(), configPaths.Home)
		modelGatewayRegistry = previousRegistry
		modelGatewayLogf = previousLogf
		keyConfigureAllowLoopbackHTTP = previousConfigureHTTP
		pipelineProxyAllowLoopbackHTTP = previousDeliveryHTTP
	})

	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	envDump := filepath.Join(t.TempDir(), "proxy-child-env.txt")
	t.Setenv("GITMOOT_PIPELINE_PROXY_HELPER", "1")
	t.Setenv("GITMOOT_PIPELINE_PROXY_ENV_DUMP", envDump)
	command := fmt.Sprintf("%q -test.run '^TestPipelineProxiedShellHelperProcess$'", testBinary)
	specFile := writeSpec(t, fmt.Sprintf(`name: proxy-e2e
repo: owner/repo
stages:
  - id: call
    cmd: |
      %s
    env_keys: [%s]
`, command, keyName))
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"pipeline", "add", specFile, "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("pipeline add code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	for _, args := range [][]string{
		{"key", "add", keyName, "--mode", "proxied", "--home", home},
		{"key", "configure", keyName, "--upstream", upstream.URL + "/pinned", "--auth", "bearer", "--home", home},
		{"key", "grant", keyName, "--pipeline", "proxy-e2e", "--home", home},
	} {
		stdout.Reset()
		stderr.Reset()
		if code := Run(args, &stdout, &stderr); code != 0 {
			t.Fatalf("%v code=%d stdout=%q stderr=%q", args, code, stdout.String(), stderr.String())
		}
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"pipeline", "run", "proxy-e2e", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("pipeline run code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	runID := strings.TrimSpace(stdout.String())
	enqueue := newPipelineStageEnqueuer(store, home)
	worker := defaultJobWorker(store, io.Discard, home)
	now := time.Date(2026, 7, 16, 17, 0, 0, 0, time.UTC)
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
	upstreamMu.Lock()
	if upstreamCalls != 1 || len(upstreamProblems) != 0 {
		t.Fatalf("upstream calls=%d problems=%v", upstreamCalls, upstreamProblems)
	}
	upstreamMu.Unlock()
	dump, err := os.ReadFile(envDump)
	if err != nil {
		t.Fatal(err)
	}
	dumpLines := strings.Split(strings.TrimSpace(string(dump)), "\n")
	if len(dumpLines) != 2 || !strings.HasPrefix(dumpLines[0], "gitmoot-kc-") || !strings.HasPrefix(dumpLines[1], "http://127.0.0.1:") || strings.Contains(string(dump), sentinel) {
		t.Fatalf("child proxy env dump = %q", dump)
	}
	placeholder := dumpLines[0]

	stage := stageRow(t, store, runID, "call")
	job, err := store.GetJob(ctx, stage.JobID)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := workflow.ParseJobPayload(job.Payload)
	if err != nil {
		t.Fatal(err)
	}
	wantAccess := []workflow.PipelineKeyAccess{{Stage: "call", Name: keyName, Source: pipelineKeySourceShared, Mode: db.KeychainModeProxied}}
	if !reflect.DeepEqual(payload.PipelineKeyAccess, wantAccess) {
		t.Fatalf("proxied payload access = %#v", payload.PipelineKeyAccess)
	}
	detail, err := (&webDataSource{home: home}).PipelineDetail(ctx, "proxy-e2e")
	if err != nil {
		t.Fatal(err)
	}
	dashboardJSON, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	events, err := store.ListJobEvents(ctx, stage.JobID)
	if err != nil {
		t.Fatal(err)
	}
	persisted := job.Payload + string(dashboardJSON) + gatewayLogs.String()
	for _, event := range events {
		persisted += event.Message
	}
	for _, forbidden := range []string{sentinel, placeholder} {
		if strings.Contains(persisted, forbidden) {
			t.Fatalf("persisted/logged output contains forbidden capability or value")
		}
	}
	for _, persistedPath := range []string{paths.Database, paths.Database + "-wal"} {
		data, err := os.ReadFile(persistedPath)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte(sentinel)) || bytes.Contains(data, []byte(placeholder)) {
			t.Fatalf("credential material persisted in %s", persistedPath)
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
		if bytes.Contains(data, []byte(sentinel)) || bytes.Contains(data, []byte(placeholder)) {
			return fmt.Errorf("credential material persisted in log %s", path)
		}
		return nil
	}); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestPipelineProxiedShellHelperProcess(t *testing.T) {
	if os.Getenv("GITMOOT_PIPELINE_PROXY_HELPER") != "1" {
		return
	}
	placeholder := os.Getenv("KEYCARD_PROXY_E2E")
	leaseURL := os.Getenv("GITMOOT_PROXY_KEYCARD_PROXY_E2E_URL")
	if !strings.HasPrefix(placeholder, "gitmoot-kc-") || leaseURL == "" || placeholder == "proxied-real-sentinel-874" {
		fmt.Fprintln(os.Stderr, "invalid proxied child environment")
		os.Exit(2)
	}
	if err := os.WriteFile(os.Getenv("GITMOOT_PIPELINE_PROXY_ENV_DUMP"), []byte(placeholder+"\n"+leaseURL+"\n"), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "write proxy environment audit failed")
		os.Exit(2)
	}
	call := func(want int) {
		request, err := http.NewRequest(http.MethodPost, leaseURL+"/action", strings.NewReader("safe-body"))
		if err != nil {
			fmt.Fprintln(os.Stderr, "build proxy request failed")
			os.Exit(2)
		}
		request.Header.Set("Authorization", "Bearer "+placeholder)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			fmt.Fprintln(os.Stderr, "proxy request failed")
			os.Exit(2)
		}
		response.Body.Close()
		if response.StatusCode != want {
			fmt.Fprintln(os.Stderr, "unexpected proxy response")
			os.Exit(2)
		}
	}
	call(http.StatusNoContent)
	call(http.StatusUnauthorized)
	fmt.Fprint(os.Stdout, `{"gitmoot_result":{"decision":"approved","summary":"proxied request lifecycle passed","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`)
	os.Exit(0)
}

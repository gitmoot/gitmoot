package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/pipeline"
)

func newPipelineServiceSecurityFixture(t *testing.T, name, specYAML string, bucket float64) (string, *db.Store, string, *httptest.Server) {
	t.Helper()
	home, paths, store := heartbeatLoopE2EHome(t)
	if err := store.CreateOrUpdatePipeline(context.Background(), db.Pipeline{
		Name: name, Repo: "owner/repo", SpecYAML: specYAML, SpecHash: pipeline.Hash([]byte(specYAML)),
	}); err != nil {
		t.Fatal(err)
	}
	token, created, err := store.CreateExposure(context.Background(), db.PipelineExposure{
		PipelineName: name, SchemaVersion: 1, SchemaJSON: `{"version":1,"fields":{}}`, SchemaHash: "sha256:test",
		Enabled: true, BucketTokens: bucket, BucketUpdatedAt: time.Now().UTC(),
	})
	if err != nil || !created {
		t.Fatalf("CreateExposure token=%q created=%v err=%v", token, created, err)
	}
	server := httptest.NewServer(newPipelineServiceHandler(home, paths, store, io.Discard))
	t.Cleanup(server.Close)
	return home, store, token, server
}

func TestPipelineServiceRejectsEnvKeysAtRequestTime(t *testing.T) {
	const name = "secret-shell"
	spec := "name: " + name + "\nrepo: owner/repo\nstages:\n  - id: publish\n    cmd: printf ok\n    env_keys: [DEPLOY_TOKEN]\n"
	home, store, token, server := newPipelineServiceSecurityFixture(t, name, spec, 5)
	response := serviceRequest(t, http.MethodPost, server.URL+"/v1/pipelines/"+name+"/runs", token, `{}`)
	if response.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("unsafe service POST status=%d body=%s", response.StatusCode, readResponse(t, response))
	}
	body := readResponse(t, response)
	if !strings.Contains(body, `stage \"publish\" declares env_keys`) || !strings.Contains(body, "forbids keycard secrets") {
		t.Fatalf("unsafe service diagnostic is not stage/field-specific: %s", body)
	}
	if runs, err := store.ListPipelineRuns(context.Background(), name); err != nil || len(runs) != 0 {
		t.Fatalf("unsafe request created runs=%v err=%v", runs, err)
	}
	assertNoPipelineServiceFreeze(t, home)
}

func TestPipelineServicePreAdmissionRejectsWithoutFreeze(t *testing.T) {
	tests := []struct {
		name       string
		bucket     float64
		wantStatus int
		setup      func(*testing.T, *db.Store, string)
	}{
		{name: "empty bucket", bucket: 0, wantStatus: http.StatusTooManyRequests},
		{name: "same pipeline active", bucket: 5, wantStatus: http.StatusConflict, setup: func(t *testing.T, store *db.Store, pipelineName string) {
			if err := store.CreatePipelineRun(context.Background(), db.PipelineRun{
				ID: "psr-11111111111111111111111111111111", Pipeline: pipelineName, Trigger: "service", State: pipeline.RunRunning, StartedAt: time.Now().UTC(),
			}); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "global cap", bucket: 5, wantStatus: http.StatusTooManyRequests, setup: func(t *testing.T, store *db.Store, _ string) {
			for i, other := range []string{"other-one", "other-two"} {
				spec := "name: " + other + "\nrepo: owner/repo\nstages:\n  - id: build\n    cmd: printf ok\n"
				if err := store.CreateOrUpdatePipeline(context.Background(), db.Pipeline{Name: other, Repo: "owner/repo", SpecYAML: spec, SpecHash: pipeline.Hash([]byte(spec))}); err != nil {
					t.Fatal(err)
				}
				if err := store.CreatePipelineRun(context.Background(), db.PipelineRun{
					ID: "psr-2222222222222222222222222222222" + string(rune('0'+i)), Pipeline: other, Trigger: "service", State: pipeline.RunRunning, StartedAt: time.Now().UTC(),
				}); err != nil {
					t.Fatal(err)
				}
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pipelineName := "preflight-" + strings.ReplaceAll(tt.name, " ", "-")
			spec := "name: " + pipelineName + "\nrepo: owner/repo\nstages:\n  - id: build\n    cmd: printf ok\n"
			home, store, token, server := newPipelineServiceSecurityFixture(t, pipelineName, spec, tt.bucket)
			if tt.setup != nil {
				tt.setup(t, store, pipelineName)
			}
			response := serviceRequest(t, http.MethodPost, server.URL+"/v1/pipelines/"+pipelineName+"/runs", token, `{}`)
			if response.StatusCode != tt.wantStatus {
				t.Fatalf("pre-admission status=%d want=%d body=%s", response.StatusCode, tt.wantStatus, readResponse(t, response))
			}
			_ = readResponse(t, response)
			assertNoPipelineServiceFreeze(t, home)
		})
	}
}

func TestPipelineServicePrivateRunLookupIsUniformlyUnauthorized(t *testing.T) {
	const name = "private-lookup"
	spec := "name: " + name + "\nrepo: owner/repo\nstages:\n  - id: build\n    cmd: printf ok\n"
	_, store, token, server := newPipelineServiceSecurityFixture(t, name, spec, 5)
	const existingID = "psr-33333333333333333333333333333333"
	if err := store.CreatePipelineRun(context.Background(), db.PipelineRun{ID: existingID, Pipeline: name, Trigger: "service", State: pipeline.RunRunning, StartedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateServiceRun(context.Background(), db.PipelineServiceRun{RunID: existingID, PipelineName: name, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}

	missing := serviceRequest(t, http.MethodGet, server.URL+"/v1/pipelines/runs/psr-44444444444444444444444444444444", token, "")
	missingBody := readResponse(t, missing)
	wrong := serviceRequest(t, http.MethodGet, server.URL+"/v1/pipelines/runs/"+existingID, "wrong-token", "")
	wrongBody := readResponse(t, wrong)
	if missing.StatusCode != http.StatusUnauthorized || wrong.StatusCode != http.StatusUnauthorized || missingBody != wrongBody || missingBody != "{\"error\":\"unauthorized\"}\n" {
		t.Fatalf("private lookup oracle remains: missing=%d %q wrong=%d %q", missing.StatusCode, missingBody, wrong.StatusCode, wrongBody)
	}
}

func assertNoPipelineServiceFreeze(t *testing.T, home string) {
	t.Helper()
	paths, err := pathsFromFlag(home)
	if err != nil {
		t.Fatal(err)
	}
	_, err = os.Stat(filepath.Join(paths.Home, pipelineServiceRunsDir))
	if !os.IsNotExist(err) {
		t.Fatalf("rejected request touched pipeline service bundle storage: err=%v", err)
	}
}

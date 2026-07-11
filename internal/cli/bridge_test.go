package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/runtime"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

const bridgeTestToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestBridgeTokenCommandPrintsPathNotTokenAndRotates(t *testing.T) {
	home := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := runBridge([]string{"token", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("bridge token exit=%d stderr=%s", code, stderr.String())
	}
	path := strings.TrimSpace(stdout.String())
	if path != filepath.Join(home, ".gitmoot", bridgeTokenName) {
		t.Fatalf("token path = %q, want bridge token path under home", path)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read token: %v", err)
	}
	if strings.Contains(stdout.String(), strings.TrimSpace(string(first))) {
		t.Fatalf("bridge token printed the token, not just the path")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat token: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("token mode = %v, want 0600", got)
	}

	stdout.Reset()
	stderr.Reset()
	if code := runBridge([]string{"token", "--home", home, "--rotate"}, &stdout, &stderr); code != 0 {
		t.Fatalf("bridge token --rotate exit=%d stderr=%s", code, stderr.String())
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read rotated token: %v", err)
	}
	if bytes.Equal(first, second) {
		t.Fatalf("rotate did not change token")
	}
}

func TestBridgeTokenAuth(t *testing.T) {
	home, _, store := heartbeatLoopE2EHome(t)
	seedBridgeJob(t, store, "job-auth")
	var audit bytes.Buffer
	handler := newBridgeHandler(home, store, bridgeTestToken, &audit)

	missing := httptest.NewRecorder()
	handler.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/v1/jobs/job-auth", nil))
	if missing.Code != http.StatusUnauthorized {
		t.Fatalf("missing auth status = %d, want 401", missing.Code)
	}

	wrong := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/jobs/job-auth", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	handler.ServeHTTP(wrong, req)
	if wrong.Code != http.StatusUnauthorized {
		t.Fatalf("wrong auth status = %d, want 401", wrong.Code)
	}

	right := bridgeRequest(http.MethodGet, "/v1/jobs/job-auth", nil)
	ok := httptest.NewRecorder()
	handler.ServeHTTP(ok, right)
	if ok.Code != http.StatusOK {
		t.Fatalf("right auth status = %d, want 200 body=%s", ok.Code, ok.Body.String())
	}
	if lines := nonEmptyLines(audit.String()); len(lines) != 3 {
		t.Fatalf("audit lines = %d, want one per request:\n%s", len(lines), audit.String())
	}
}

func TestBridgeLoopbackBindGuard(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:8791", "localhost:8791", "[::1]:8791"} {
		if err := validateBridgeAddr(addr, false); err != nil {
			t.Fatalf("validateBridgeAddr(%q) returned error: %v", addr, err)
		}
	}
	for _, addr := range []string{"0.0.0.0:8791", ":8791", "192.168.1.10:8791"} {
		if err := validateBridgeAddr(addr, false); err == nil {
			t.Fatalf("validateBridgeAddr(%q) allowed non-loopback bind without --allow-remote", addr)
		}
		if err := validateBridgeAddr(addr, true); err != nil {
			t.Fatalf("validateBridgeAddr(%q, allowRemote) returned error: %v", addr, err)
		}
	}
}

func TestBridgeEndpointsWithSeededHome(t *testing.T) {
	ctx := context.Background()
	home, _, store := heartbeatLoopE2EHome(t)
	checkout := createDaemonWorkerGitCheckout(t, "main")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "planner", runtime.ShellRuntime, heartbeatShellResultScript, []string{"ask"}, "owner/repo")
	seedBridgeMemory(t, store)
	handler := newBridgeHandler(home, store, bridgeTestToken, nil)

	specYAML := "name: bridge-flow\nrepo: owner/repo\nstages:\n" +
		pipelineE2EStage("source", pipelineStageResultCmd("approved", "source ok", nil), "")
	specFile := writeSpec(t, specYAML)
	var out, errBuf bytes.Buffer
	if code := Run([]string{"pipeline", "add", specFile, "--enable", "--home", home}, &out, &errBuf); code != 0 {
		t.Fatalf("pipeline add exit=%d stderr=%s", code, errBuf.String())
	}

	runResp := bridgeDo(t, handler, bridgeRequest(http.MethodPost, "/v1/pipelines/bridge-flow/run", strings.NewReader(`{}`)))
	if runResp.Code != http.StatusOK {
		t.Fatalf("pipeline run status=%d body=%s", runResp.Code, runResp.Body.String())
	}
	var runOut struct {
		RunID string `json:"run_id"`
	}
	decodeBridgeBody(t, runResp, &runOut)
	if runOut.RunID == "" {
		t.Fatalf("pipeline run returned empty run_id")
	}

	stateResp := bridgeDo(t, handler, bridgeRequest(http.MethodGet, "/v1/runs/"+runOut.RunID, nil))
	if stateResp.Code != http.StatusOK {
		t.Fatalf("run state status=%d body=%s", stateResp.Code, stateResp.Body.String())
	}
	var runState pipelineRunJSON
	decodeBridgeBody(t, stateResp, &runState)
	if runState.ID != runOut.RunID || runState.State == "" || len(runState.Stages) != 1 {
		t.Fatalf("run state = %+v, want matching run with one stage", runState)
	}

	recallReq := `{"query":"arm64 runner flake","repo":"owner/repo","agent":"lead","limit":3}`
	recallResp := bridgeDo(t, handler, bridgeRequest(http.MethodPost, "/v1/memory/recall", strings.NewReader(recallReq)))
	if recallResp.Code != http.StatusOK {
		t.Fatalf("memory recall status=%d body=%s", recallResp.Code, recallResp.Body.String())
	}
	var recall bridgeMemoryRecallResponse
	decodeBridgeBody(t, recallResp, &recall)
	if len(recall.Entries) == 0 || recall.Entries[0].Key != "bridge-memory" {
		t.Fatalf("recall entries = %+v, want seeded bridge memory", recall.Entries)
	}

	askReq := `{"message":"plan the work","repo":"owner/repo"}`
	askResp := bridgeDo(t, handler, bridgeRequest(http.MethodPost, "/v1/agents/planner/ask", strings.NewReader(askReq)))
	if askResp.Code != http.StatusOK {
		t.Fatalf("agent ask status=%d body=%s", askResp.Code, askResp.Body.String())
	}
	var askOut struct {
		JobID string `json:"job_id"`
	}
	decodeBridgeBody(t, askResp, &askOut)
	if askOut.JobID == "" {
		t.Fatalf("ask returned empty job_id")
	}
	job, err := store.GetJob(ctx, askOut.JobID)
	if err != nil {
		t.Fatalf("GetJob(%s): %v", askOut.JobID, err)
	}
	if job.State != string(workflow.JobQueued) || job.Type != "ask" || job.Agent != "planner" {
		t.Fatalf("ask job = %+v, want queued planner ask", job)
	}

	jobResp := bridgeDo(t, handler, bridgeRequest(http.MethodGet, "/v1/jobs/"+askOut.JobID, nil))
	if jobResp.Code != http.StatusOK {
		t.Fatalf("job get status=%d body=%s", jobResp.Code, jobResp.Body.String())
	}
	var jobOut jobShowOutput
	decodeBridgeBody(t, jobResp, &jobOut)
	if jobOut.Job.ID != askOut.JobID || jobOut.Payload.Repo != "owner/repo" {
		t.Fatalf("job output = %+v, want bridge-created job", jobOut)
	}
}

func TestBridgePipelineOverlapReturnsConflict(t *testing.T) {
	home, _, store := heartbeatLoopE2EHome(t)
	checkout := createDaemonWorkerGitCheckout(t, "main")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	handler := newBridgeHandler(home, store, bridgeTestToken, nil)
	specYAML := "name: bridge-flow\nrepo: owner/repo\nstages:\n" +
		pipelineE2EStage("source", pipelineStageResultCmd("approved", "source ok", nil), "")
	specFile := writeSpec(t, specYAML)
	var out, errBuf bytes.Buffer
	if code := Run([]string{"pipeline", "add", specFile, "--enable", "--home", home}, &out, &errBuf); code != 0 {
		t.Fatalf("pipeline add exit=%d stderr=%s", code, errBuf.String())
	}
	first := bridgeDo(t, handler, bridgeRequest(http.MethodPost, "/v1/pipelines/bridge-flow/run", strings.NewReader(`{}`)))
	if first.Code != http.StatusOK {
		t.Fatalf("first run status=%d body=%s", first.Code, first.Body.String())
	}
	second := bridgeDo(t, handler, bridgeRequest(http.MethodPost, "/v1/pipelines/bridge-flow/run", strings.NewReader(`{}`)))
	if second.Code != http.StatusConflict {
		t.Fatalf("second run status=%d, want 409 body=%s", second.Code, second.Body.String())
	}
}

func TestBridgePipelineRejectsDisabledPipeline(t *testing.T) {
	home, _, store := heartbeatLoopE2EHome(t)
	checkout := createDaemonWorkerGitCheckout(t, "main")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	handler := newBridgeHandler(home, store, bridgeTestToken, nil)
	specYAML := "name: disabled-flow\nrepo: owner/repo\nstages:\n" +
		pipelineE2EStage("source", pipelineStageResultCmd("approved", "source ok", nil), "")
	specFile := writeSpec(t, specYAML)
	var out, errBuf bytes.Buffer
	if code := Run([]string{"pipeline", "add", specFile, "--home", home}, &out, &errBuf); code != 0 {
		t.Fatalf("pipeline add exit=%d stderr=%s", code, errBuf.String())
	}
	resp := bridgeDo(t, handler, bridgeRequest(http.MethodPost, "/v1/pipelines/disabled-flow/run", strings.NewReader(`{}`)))
	if resp.Code == http.StatusOK || !strings.Contains(resp.Body.String(), "disabled") {
		t.Fatalf("disabled run status=%d body=%s", resp.Code, resp.Body.String())
	}
	if _, ok, err := store.ActivePipelineRun(context.Background(), "disabled-flow"); err != nil || ok {
		t.Fatalf("disabled pipeline created a run: ok=%v err=%v", ok, err)
	}
}

func TestBridgeRateLimitTrip(t *testing.T) {
	home, _, store := heartbeatLoopE2EHome(t)
	seedBridgeJob(t, store, "job-rate")
	handler := newBridgeHandler(home, store, bridgeTestToken, nil)
	for i := 0; i < 30; i++ {
		resp := bridgeDo(t, handler, bridgeRequest(http.MethodGet, "/v1/jobs/job-rate", nil))
		if resp.Code != http.StatusOK {
			t.Fatalf("request %d status=%d body=%s", i+1, resp.Code, resp.Body.String())
		}
	}
	resp := bridgeDo(t, handler, bridgeRequest(http.MethodGet, "/v1/jobs/job-rate", nil))
	if resp.Code != http.StatusTooManyRequests {
		t.Fatalf("31st request status=%d, want 429 body=%s", resp.Code, resp.Body.String())
	}
}

func TestBridgeBodySizeCap(t *testing.T) {
	home, _, store := heartbeatLoopE2EHome(t)
	handler := newBridgeHandler(home, store, bridgeTestToken, nil)
	body := `{"query":"` + strings.Repeat("x", bridgeBodyLimit) + `"}`
	resp := bridgeDo(t, handler, bridgeRequest(http.MethodPost, "/v1/memory/recall", strings.NewReader(body)))
	if resp.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize body status=%d, want 413 body=%s", resp.Code, resp.Body.String())
	}
}

func bridgeRequest(method, target string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, target, body)
	req.Header.Set("Authorization", "Bearer "+bridgeTestToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

func bridgeDo(t *testing.T, handler http.Handler, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	return resp
}

func decodeBridgeBody(t *testing.T, resp *httptest.ResponseRecorder, out any) {
	t.Helper()
	if err := json.Unmarshal(resp.Body.Bytes(), out); err != nil {
		t.Fatalf("decode response: %v body=%s", err, resp.Body.String())
	}
}

func seedBridgeJob(t *testing.T, store *db.Store, id string) {
	t.Helper()
	payload := workflow.JobPayload{
		Repo:         "owner/repo",
		Branch:       "main",
		Sender:       "test",
		Instructions: "seeded bridge job",
		Result: &workflow.AgentResult{
			Decision:    "approved",
			Summary:     "seeded",
			Findings:    nil,
			ChangesMade: []string{},
			TestsRun:    []string{},
			Needs:       []string{},
			Delegations: []workflow.Delegation{},
		},
	}
	if err := store.CreateJob(context.Background(), db.Job{
		ID:      id,
		Agent:   "planner",
		Type:    "ask",
		State:   string(workflow.JobSucceeded),
		Payload: mustJSON(t, payload),
	}); err != nil {
		t.Fatalf("CreateJob(%s): %v", id, err)
	}
}

func seedBridgeMemory(t *testing.T, store *db.Store) {
	t.Helper()
	if _, err := store.UpsertConfirmedMemory(context.Background(), db.ConfirmedMemory{
		Owner:      db.MemoryOwner{Kind: "agent", Ref: "lead"},
		Repo:       "owner/repo",
		Scope:      "repo",
		Key:        "bridge-memory",
		Content:    "arm64 runner flake is visible through bridge recall",
		Provenance: "seed",
	}); err != nil {
		t.Fatalf("UpsertConfirmedMemory: %v", err)
	}
}

func nonEmptyLines(s string) []string {
	lines := []string{}
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

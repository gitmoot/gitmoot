package cli

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/pipeline"
	"github.com/gitmoot/gitmoot/internal/proof"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestPipelineServiceAcceptanceE2E(t *testing.T) {
	ctx := context.Background()
	t.Setenv("HERDR_SOCKET_PATH", filepath.Join(t.TempDir(), "throwaway.sock"))
	t.Setenv("HERDR_ENV", "")
	home, _, store := heartbeatLoopE2EHome(t)
	checkout := createDaemonWorkerGitCheckout(t, "main")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	beforeHead := gitOutput(t, checkout, "rev-parse", "HEAD")
	beforeStatus := gitOutput(t, checkout, "status", "--porcelain")

	const envSentinel = "GITMOOT_INPUT_COUNT=3"
	cmd := `test "$GITMOOT_INPUT_COUNT" = "3" || exit 91; printf '%s' '{"gitmoot_result":{"decision":"approved","summary":"constant service result","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}'`
	specYAML := "name: count-service\nrepo: owner/repo\nstages:\n" + pipelineE2EStage("check", cmd, "")
	specFile := writeSpec(t, specYAML)
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"pipeline", "add", specFile, "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("pipeline add exit=%d stderr=%s", code, stderr.String())
	}
	schemaPath := filepath.Join(t.TempDir(), "schema.json")
	if err := os.WriteFile(schemaPath, []byte(`{"version":1,"fields":{"count":{"type":"integer","required":true,"minimum":1,"maximum":5}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"pipeline", "expose", "--schema", schemaPath, "--home", home, "count-service"}, &stdout, &stderr); code != 0 {
		t.Fatalf("pipeline expose exit=%d stderr=%s", code, stderr.String())
	}
	token := exposeTokenFromText(t, stdout.String())
	paths, err := pathsFromFlag(home)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(newPipelineServiceHandler(home, paths, store, io.Discard))
	defer server.Close()

	bucketBeforeBad, _, err := store.GetExposure(ctx, "count-service")
	if err != nil {
		t.Fatal(err)
	}
	bad := serviceRequest(t, http.MethodPost, server.URL+"/v1/pipelines/count-service/runs", token, `{"count":"wrong","unknown":true}`)
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad input status=%d body=%s", bad.StatusCode, readResponse(t, bad))
	}
	badBody := readResponse(t, bad)
	if strings.Contains(badBody, "wrong") || strings.Contains(badBody, "true") || !strings.Contains(badBody, `"code":"type"`) || !strings.Contains(badBody, `"code":"unknown"`) {
		t.Fatalf("bad input diagnostic is not sorted/value-free: %s", badBody)
	}
	if runs, err := store.ListPipelineRuns(ctx, "count-service"); err != nil || len(runs) != 0 {
		t.Fatalf("schema rejection created a run: runs=%v err=%v", runs, err)
	}
	bucketAfterBad, _, err := store.GetExposure(ctx, "count-service")
	if err != nil || bucketAfterBad.BucketTokens != bucketBeforeBad.BucketTokens || !bucketAfterBad.BucketUpdatedAt.Equal(bucketBeforeBad.BucketUpdatedAt) {
		t.Fatalf("schema rejection consumed/touched token bucket: before=%+v after=%+v err=%v", bucketBeforeBad, bucketAfterBad, err)
	}

	acceptedResponse := serviceRequest(t, http.MethodPost, server.URL+"/v1/pipelines/count-service/runs", token, `{"count":3}`)
	if acceptedResponse.StatusCode != http.StatusAccepted {
		t.Fatalf("valid input status=%d body=%s", acceptedResponse.StatusCode, readResponse(t, acceptedResponse))
	}
	var accepted pipelineServiceAccepted
	decodeResponse(t, acceptedResponse, &accepted)
	if !pipelineServiceRunIDPattern.MatchString(accepted.RunID) || strings.HasPrefix(accepted.RunID, "prun-") {
		t.Fatalf("service run id is not unpredictable 128-bit form: %q", accepted.RunID)
	}
	if accepted.StatusURL != pipelineServiceStatusURL(accepted.RunID) || accepted.ReceiptURL != pipelineServiceReceiptURL(accepted.RunID) {
		t.Fatalf("202 URLs = %+v", accepted)
	}
	conflictResponse := serviceRequest(t, http.MethodPost, server.URL+"/v1/pipelines/count-service/runs", token, `{"count":3}`)
	if conflictResponse.StatusCode != http.StatusConflict {
		t.Fatalf("same-pipeline overlap status=%d body=%s", conflictResponse.StatusCode, readResponse(t, conflictResponse))
	}
	_ = readResponse(t, conflictResponse)
	if runs, err := store.ListPipelineRuns(ctx, "count-service"); err != nil || len(runs) != 1 {
		t.Fatalf("overlap conflict created a partial run: runs=%v err=%v", runs, err)
	}
	frozen, err := os.ReadDir(filepath.Join(paths.Home, pipelineServiceRunsDir))
	if err != nil || len(frozen) != 1 || frozen[0].Name() != accepted.RunID {
		t.Fatalf("happy path/conflict froze bundles=%v err=%v, want exactly %s", frozen, err, accepted.RunID)
	}
	bucketAfterConflict, _, err := store.GetExposure(ctx, "count-service")
	if err != nil || bucketAfterConflict.BucketTokens != bucketBeforeBad.BucketTokens-1 {
		t.Fatalf("overlap conflict consumed a token: bucket=%+v err=%v", bucketAfterConflict, err)
	}

	worker := defaultJobWorker(store, io.Discard, home)
	enqueue := newPipelineStageEnqueuer(store, home)
	now := time.Now().UTC()
	for i := 0; i < 8; i++ {
		if err := runEnabledRepoWorkerTicks(ctx, store, worker, 1, io.Discard, now.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatalf("worker tick %d: %v", i, err)
		}
		if err := runPipelineScanOnce(ctx, store, enqueue, now.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatalf("pipeline scan %d: %v", i, err)
		}
		run, _, err := store.GetPipelineRun(ctx, accepted.RunID)
		if err != nil {
			t.Fatal(err)
		}
		if run.State != pipeline.RunRunning {
			break
		}
	}
	run, ok, err := store.GetPipelineRun(ctx, accepted.RunID)
	if err != nil || !ok || run.State != pipeline.RunSucceeded {
		t.Fatalf("service run did not succeed: run=%+v ok=%v err=%v", run, ok, err)
	}
	stage := stageRow(t, store, accepted.RunID, "check")
	job, err := store.GetJobForProof(ctx, stage.JobID)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := workflow.ParseJobPayload(job.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(payload.PipelineInputEnv) != 1 || payload.PipelineInputEnv[0] != envSentinel {
		t.Fatalf("typed input env was not persisted/delivered: %v", payload.PipelineInputEnv)
	}
	if strings.Contains(payload.Instructions, envSentinel) || strings.Contains(payload.Instructions, `{"count":3}`) {
		t.Fatalf("submitted input leaked into Instructions: %q", payload.Instructions)
	}
	if strings.TrimSpace(payload.WorktreePath) == "" || !payload.ReadOnlyWorktree || payload.WorktreePath == checkout || !strings.HasPrefix(payload.WorktreePath, filepath.Clean(paths.Home)+string(filepath.Separator)) {
		t.Fatalf("service shell did not use a detached home-contained worktree: %+v", payload)
	}
	if got := gitOutput(t, checkout, "rev-parse", "HEAD"); got != beforeHead {
		t.Fatalf("primary checkout head changed: before=%s after=%s", beforeHead, got)
	}
	if got := gitOutput(t, checkout, "status", "--porcelain"); got != beforeStatus {
		t.Fatalf("primary checkout became dirty: before=%q after=%q", beforeStatus, got)
	}
	dashboard := httptest.NewServer(newDashboardWebHandler(&webDataSource{home: home, responseCache: newDashboardJSONCache(io.Discard)}))
	defer dashboard.Close()
	preReceipt, err := http.Get(dashboard.URL + accepted.ReceiptURL)
	if err != nil {
		t.Fatal(err)
	}
	if preReceipt.StatusCode != http.StatusNotFound {
		t.Fatalf("read-only receipt finalized an unfinalized success: status=%d body=%s", preReceipt.StatusCode, readResponse(t, preReceipt))
	}
	_ = readResponse(t, preReceipt)
	unfinalized, ok, err := store.GetServiceRun(ctx, accepted.RunID)
	if err != nil || !ok || unfinalized.ArtifactRelpath != "" || unfinalized.ProofID != "" {
		t.Fatalf("public pre-receipt mutated service metadata: %+v ok=%v err=%v", unfinalized, ok, err)
	}

	statusResponse := serviceRequest(t, http.MethodGet, server.URL+accepted.StatusURL, token, "")
	if statusResponse.StatusCode != http.StatusOK {
		t.Fatalf("status GET=%d body=%s", statusResponse.StatusCode, readResponse(t, statusResponse))
	}
	statusBody := readResponseBytes(t, statusResponse)
	if bytes.Contains(statusBody, []byte(`"payload_json"`)) || bytes.Contains(statusBody, []byte(`"count":`)) || bytes.Contains(statusBody, []byte(envSentinel)) {
		t.Fatalf("status response leaked submitted input: %s", statusBody)
	}
	var status pipelineServiceStatus
	if err := json.Unmarshal(statusBody, &status); err != nil {
		t.Fatal(err)
	}
	if status.Status != pipeline.RunSucceeded || !status.ProofVerified || status.ProofVerificationKind != "stored_pipeline_outcome" || status.ProofID == "" || status.BundleURL == "" {
		t.Fatalf("succeeded status lacks verified proof metadata: %+v", status)
	}
	serviceRun, ok, err := store.GetServiceRun(ctx, accepted.RunID)
	if err != nil || !ok {
		t.Fatalf("finalized service metadata ok=%v err=%v", ok, err)
	}
	artifactPath, err := containedServiceArtifactPath(paths, serviceRun.ArtifactRelpath)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(artifactPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("final archive is not mode 0600: info=%v err=%v", info, err)
	}
	rotatedToken, err := store.RotateExposureToken(ctx, "count-service")
	if err != nil {
		t.Fatal(err)
	}
	revokedPoll := serviceRequest(t, http.MethodGet, server.URL+accepted.StatusURL, token, "")
	if revokedPoll.StatusCode != http.StatusUnauthorized {
		t.Fatalf("rotated token was not revoked: status=%d body=%s", revokedPoll.StatusCode, readResponse(t, revokedPoll))
	}
	_ = readResponse(t, revokedPoll)
	token = rotatedToken
	if err := store.SetExposureEnabled(ctx, "count-service", false); err != nil {
		t.Fatal(err)
	}
	disabledPost := serviceRequest(t, http.MethodPost, server.URL+"/v1/pipelines/count-service/runs", token, `{"count":3}`)
	if disabledPost.StatusCode != http.StatusForbidden {
		t.Fatalf("disabled exposure accepted POST: status=%d body=%s", disabledPost.StatusCode, readResponse(t, disabledPost))
	}
	_ = readResponse(t, disabledPost)
	disabledPoll := serviceRequest(t, http.MethodGet, server.URL+accepted.StatusURL, token, "")
	if disabledPoll.StatusCode != http.StatusOK {
		t.Fatalf("disablement blocked accepted-run polling: status=%d body=%s", disabledPoll.StatusCode, readResponse(t, disabledPoll))
	}
	_ = readResponse(t, disabledPoll)

	bundleResponse := serviceRequest(t, http.MethodGet, server.URL+status.BundleURL, token, "")
	if bundleResponse.StatusCode != http.StatusOK {
		t.Fatalf("bundle GET=%d body=%s", bundleResponse.StatusCode, readResponse(t, bundleResponse))
	}
	bundleBytes := readResponseBytes(t, bundleResponse)
	archive, err := zip.NewReader(bytes.NewReader(bundleBytes), int64(len(bundleBytes)))
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(archive.File))
	var proofBytes []byte
	for _, file := range archive.File {
		names = append(names, file.Name)
		if file.Name == "proof.json" {
			reader, err := file.Open()
			if err != nil {
				t.Fatal(err)
			}
			proofBytes, err = io.ReadAll(reader)
			_ = reader.Close()
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	sort.Strings(names)
	for _, required := range []string{"bundle.yaml", "proof.json", "spec.yaml", "verification.json"} {
		if !serviceContainsString(names, required) {
			t.Fatalf("archive missing %s: %v", required, names)
		}
	}
	var manifest proof.Manifest
	if err := json.Unmarshal(proofBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	if err := proof.VerifyManifest(manifest); err != nil || manifest.ProofID != status.ProofID {
		t.Fatalf("embedded proof invalid/mismatched: proof=%s status=%s err=%v", manifest.ProofID, status.ProofID, err)
	}
	if bytes.Contains(proofBytes, []byte(envSentinel)) || bytes.Contains(proofBytes, []byte(`{"count":3}`)) || bytes.Contains(proofBytes, []byte(cmd)) {
		t.Fatal("submitted input or shell command leaked into public proof")
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"proof", "--verify", "--json", "--home", home, accepted.RunID}, &stdout, &stderr); code != 0 {
		t.Fatalf("proof --verify exit=%d stderr=%s", code, stderr.String())
	}
	if !bytes.Equal(bytes.TrimSpace(stdout.Bytes()), bytes.TrimSpace(proofBytes)) {
		t.Fatal("proof --verify did not reproduce the embedded canonical manifest")
	}

	receiptResponse, err := http.Get(dashboard.URL + accepted.ReceiptURL)
	if err != nil {
		t.Fatal(err)
	}
	if receiptResponse.StatusCode != http.StatusOK {
		t.Fatalf("public receipt=%d body=%s", receiptResponse.StatusCode, readResponse(t, receiptResponse))
	}
	receiptBody := readResponse(t, receiptResponse)
	if !strings.Contains(receiptBody, status.ProofID) || !strings.Contains(receiptBody, accepted.ReceiptURL+"/bundle") || strings.Contains(receiptBody, envSentinel) || strings.Contains(receiptBody, `{"count":3}`) || strings.Contains(receiptBody, cmd) {
		t.Fatalf("public receipt missing proof/bundle or leaked input: %s", receiptBody)
	}
	publicBundle, err := http.Get(dashboard.URL + accepted.ReceiptURL + "/bundle")
	if err != nil {
		t.Fatal(err)
	}
	if publicBundle.StatusCode != http.StatusOK || !strings.Contains(publicBundle.Header.Get("Content-Disposition"), accepted.RunID) || publicBundle.Header.Get("Content-Security-Policy") == "" {
		t.Fatalf("public bundle status/headers: status=%d headers=%v", publicBundle.StatusCode, publicBundle.Header)
	}
	if got := readResponseBytes(t, publicBundle); !bytes.Equal(got, bundleBytes) {
		t.Fatal("public receipt bundle differs from authenticated bundle")
	}
}

func TestPipelineRemoveRefusesActiveServiceRun(t *testing.T) {
	home, _, store := heartbeatLoopE2EHome(t)
	ctx := context.Background()
	if err := store.CreateOrUpdatePipeline(ctx, db.Pipeline{Name: "active-service", SpecYAML: "name: active-service\n", SpecHash: "spec"}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreatePipelineRun(ctx, db.PipelineRun{ID: "psr-00000000000000000000000000000001", Pipeline: "active-service", Trigger: "service", State: pipeline.RunRunning, StartedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"pipeline", "remove", "--home", home, "active-service"}, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "active service run") {
		t.Fatalf("pipeline remove exit=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if _, ok, err := store.GetPipeline(ctx, "active-service"); err != nil || !ok {
		t.Fatalf("active service pipeline was removed: ok=%v err=%v", ok, err)
	}
}

func serviceRequest(t *testing.T, method, url, token, body string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func readResponse(t *testing.T, response *http.Response) string {
	t.Helper()
	return string(readResponseBytes(t, response))
}

func readResponseBytes(t *testing.T, response *http.Response) []byte {
	t.Helper()
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func decodeResponse(t *testing.T, response *http.Response, target any) {
	t.Helper()
	defer response.Body.Close()
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		t.Fatal(err)
	}
}

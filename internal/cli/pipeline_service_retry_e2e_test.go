package cli

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/pipeline"
	"github.com/gitmoot/gitmoot/internal/proof"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestPipelineServiceRetrySuccessFinalizesProofAndReceipt(t *testing.T) {
	ctx := context.Background()
	t.Setenv("HERDR_SOCKET_PATH", filepath.Join(t.TempDir(), "throwaway.sock"))
	t.Setenv("HERDR_ENV", "")
	home, paths, store := heartbeatLoopE2EHome(t)
	checkout := createDaemonWorkerGitCheckout(t, "main")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)

	marker := filepath.Join(t.TempDir(), "first-attempt-failed")
	quotedMarker := shellQuote(marker, "posix")
	cmd := fmt.Sprintf(`if [ ! -f %s ]; then : > %s; exit 17; fi; printf '%%s' '{"gitmoot_result":{"decision":"approved","summary":"retry succeeded","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}'`, quotedMarker, quotedMarker)
	specYAML := "name: retry-service\nrepo: owner/repo\nstages:\n  - id: check\n    cmd: |\n      " + cmd + "\n    retry: 1\n"
	specFile := writeSpec(t, specYAML)
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"pipeline", "add", specFile, "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("pipeline add exit=%d stderr=%s", code, stderr.String())
	}
	schemaPath := filepath.Join(t.TempDir(), "schema.json")
	if err := os.WriteFile(schemaPath, []byte(`{"version":1,"fields":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"pipeline", "expose", "--schema", schemaPath, "--home", home, "retry-service"}, &stdout, &stderr); code != 0 {
		t.Fatalf("pipeline expose exit=%d stderr=%s", code, stderr.String())
	}
	token := exposeTokenFromText(t, stdout.String())
	server := httptest.NewServer(newPipelineServiceHandler(home, paths, store, io.Discard))
	defer server.Close()

	acceptedResponse := serviceRequest(t, http.MethodPost, server.URL+"/v1/pipelines/retry-service/runs", token, `{}`)
	if acceptedResponse.StatusCode != http.StatusAccepted {
		t.Fatalf("service POST=%d body=%s", acceptedResponse.StatusCode, readResponse(t, acceptedResponse))
	}
	var accepted pipelineServiceAccepted
	decodeResponse(t, acceptedResponse, &accepted)

	worker := defaultJobWorker(store, io.Discard, home)
	enqueue := newPipelineStageEnqueuer(store, home)
	now := time.Now().UTC()
	for i := 0; i < 12; i++ {
		if err := runEnabledRepoWorkerTicksTracked(ctx, store, worker, 1, "", io.Discard, now.Add(time.Duration(i)*time.Second), nil, nil); err != nil {
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
		t.Fatalf("retried service run did not succeed: run=%+v ok=%v err=%v", run, ok, err)
	}
	stage := stageRow(t, store, accepted.RunID, "check")
	failedID := pipelineStageJobID(accepted.RunID, "check", 0)
	succeededID := pipelineStageJobID(accepted.RunID, "check", 1)
	if stage.Attempt != 1 || stage.JobID != succeededID {
		t.Fatalf("current stage attempt=%d job=%q, want attempt 1 job %q", stage.Attempt, stage.JobID, succeededID)
	}
	failedJob, err := store.GetJobForProof(ctx, failedID)
	if err != nil || failedJob.State != string(workflow.JobFailed) {
		t.Fatalf("superseded attempt=%+v err=%v, want durable failed job", failedJob, err)
	}

	statusResponse := serviceRequest(t, http.MethodGet, server.URL+accepted.StatusURL, token, "")
	if statusResponse.StatusCode != http.StatusOK {
		t.Fatalf("retry status GET=%d body=%s", statusResponse.StatusCode, readResponse(t, statusResponse))
	}
	var status pipelineServiceStatus
	decodeResponse(t, statusResponse, &status)
	if !status.ProofVerified || status.ProofID == "" || status.BundleURL == "" {
		t.Fatalf("retried run did not finalize verified proof: %+v", status)
	}
	bundleResponse := serviceRequest(t, http.MethodGet, server.URL+status.BundleURL, token, "")
	if bundleResponse.StatusCode != http.StatusOK {
		t.Fatalf("retry bundle GET=%d body=%s", bundleResponse.StatusCode, readResponse(t, bundleResponse))
	}
	bundleBytes := readResponseBytes(t, bundleResponse)
	archive, err := zip.NewReader(bytes.NewReader(bundleBytes), int64(len(bundleBytes)))
	if err != nil {
		t.Fatal(err)
	}
	var proofBytes []byte
	for _, file := range archive.File {
		if file.Name != "proof.json" {
			continue
		}
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
	var manifest proof.Manifest
	if err := json.Unmarshal(proofBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	if err := proof.VerifyManifest(manifest); err != nil {
		t.Fatalf("retried proof does not verify: %v", err)
	}
	if bytes.Contains(proofBytes, []byte(failedID)) || !bytes.Contains(proofBytes, []byte(succeededID)) {
		t.Fatalf("proof did not select only current attempt: failed=%q succeeded=%q proof=%s", failedID, succeededID, proofBytes)
	}

	dashboard := httptest.NewServer(newDashboardWebHandler(&webDataSource{home: home, responseCache: newDashboardJSONCache(io.Discard)}))
	defer dashboard.Close()
	receipt, err := http.Get(dashboard.URL + accepted.ReceiptURL)
	if err != nil {
		t.Fatal(err)
	}
	receiptBody := readResponse(t, receipt)
	if receipt.StatusCode != http.StatusOK || !strings.Contains(receiptBody, status.ProofID) {
		t.Fatalf("retried public receipt=%d body=%s", receipt.StatusCode, receiptBody)
	}
}

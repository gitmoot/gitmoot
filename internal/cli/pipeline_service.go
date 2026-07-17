package cli

import (
	"archive/zip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/pipeline"
	"github.com/gitmoot/gitmoot/internal/proof"
)

const (
	defaultPipelineServiceAddr           = "127.0.0.1:8792"
	pipelineServiceBucketCapacity        = 5
	pipelineServiceBucketRefillPerSecond = 1.0 / 60.0
	pipelineServiceBucketCost            = 1
	pipelineServiceDefaultMaxActive      = 2
	pipelineServiceReadTimeout           = 10 * time.Second
	pipelineServiceWriteTimeout          = 30 * time.Second
	pipelineServiceIdleTimeout           = 60 * time.Second
)

type pipelineServiceHandler struct {
	rawHome    string
	paths      config.Paths
	store      *db.Store
	stderr     io.Writer
	finalizeMu sync.Mutex
}

type pipelineServiceAccepted struct {
	RunID      string `json:"run_id"`
	Status     string `json:"status"`
	StatusURL  string `json:"status_url"`
	ReceiptURL string `json:"receipt_url"`
}

type pipelineServiceStatus struct {
	RunID                 string `json:"run_id"`
	Pipeline              string `json:"pipeline"`
	Status                string `json:"status"`
	ProofID               string `json:"proof_id,omitempty"`
	ProofVerified         bool   `json:"proof_verified"`
	ProofVerificationKind string `json:"proof_verification_kind,omitempty"`
	BundleURL             string `json:"bundle_url,omitempty"`
	ReceiptURL            string `json:"receipt_url"`
}

func runPipelineServe(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pipeline serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	addr := fs.String("addr", defaultPipelineServiceAddr, "listen address")
	allowRemote := fs.Bool("allow-remote", false, "permit a non-loopback bind; dangerous without an outer firewall")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "pipeline serve does not accept positional arguments")
		return 2
	}
	if err := validateBridgeAddr(*addr, *allowRemote); err != nil {
		fmt.Fprintf(stderr, "pipeline serve: %v\n", err)
		return 2
	}
	if *allowRemote && !serviceAddressIsLoopback(*addr) {
		fmt.Fprintln(stderr, "WARNING: pipeline service is bound remotely; use only behind an owner-controlled firewall and TLS proxy")
	}
	if err := withStoreAndPaths(*home, func(paths config.Paths, store *db.Store) error {
		listener, err := net.Listen("tcp", *addr)
		if err != nil {
			return err
		}
		defer listener.Close()
		server := &http.Server{
			Addr: *addr, Handler: newPipelineServiceHandler(*home, paths, store, stderr),
			ReadTimeout: pipelineServiceReadTimeout, WriteTimeout: pipelineServiceWriteTimeout,
			IdleTimeout: pipelineServiceIdleTimeout,
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		errCh := make(chan error, 1)
		go func() {
			serveErr := server.Serve(listener)
			if errors.Is(serveErr, http.ErrServerClosed) {
				serveErr = nil
			}
			errCh <- serveErr
		}()
		writeLine(stdout, "pipeline service listening on http://%s", listener.Addr().String())
		select {
		case err := <-errCh:
			return err
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := server.Shutdown(shutdownCtx); err != nil {
				return err
			}
			return <-errCh
		}
	}); err != nil {
		fmt.Fprintf(stderr, "pipeline serve: %v\n", err)
		return 1
	}
	return 0
}

func serviceAddressIsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(addr))
	return err == nil && bridgeHostIsLoopback(host)
}

func newPipelineServiceHandler(rawHome string, paths config.Paths, store *db.Store, stderr io.Writer) http.Handler {
	h := &pipelineServiceHandler{rawHome: rawHome, paths: paths, store: store, stderr: stderr}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/pipelines/{name}/runs", h.handleCreateRun)
	mux.HandleFunc("GET /v1/pipelines/runs/{id}", h.handleGetRun)
	mux.HandleFunc("GET /v1/pipelines/runs/{id}/bundle", h.handleGetBundle)
	return mux
}

func (h *pipelineServiceHandler) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := strings.TrimSpace(r.PathValue("name"))
	exposure, authorized, err := h.authenticate(ctx, name, r)
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	if !authorized {
		h.writeUnauthorized(w)
		return
	}
	if !exposure.Enabled {
		h.writeError(w, http.StatusForbidden, "exposure_disabled")
		return
	}
	schema, err := pipeline.ParseServiceSchema([]byte(exposure.SchemaJSON))
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, "invalid_stored_schema")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, pipeline.DefaultServiceInputMaxBody+1))
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "malformed_json")
		return
	}
	values, fieldErrors := pipeline.ValidateServiceInput(schema, body, pipeline.DefaultServiceInputMaxBody)
	if len(fieldErrors) != 0 {
		h.writeJSON(w, http.StatusBadRequest, pipeline.NewInputDiagnostic(fieldErrors))
		return
	}
	record, ok, err := h.store.GetPipeline(ctx, name)
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	if !ok {
		h.writeError(w, http.StatusNotFound, "pipeline_not_found")
		return
	}
	spec, err := pipeline.Load([]byte(record.SpecYAML))
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, "invalid_stored_pipeline")
		return
	}
	if safetyErr := servicePipelinePublicSafetyError(spec); safetyErr != nil {
		h.writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error":   "service_requires_shell_only_template_free_pipeline",
			"message": safetyErr.Error(),
		})
		return
	}
	payload, err := canonicalPipelineServicePayload(values)
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	now := time.Now().UTC()
	if err := h.store.CheckServiceRunAdmission(ctx, db.ServiceRunAdmissionCheck{
		PipelineName: name, Now: now, BucketCapacity: pipelineServiceBucketCapacity,
		RefillPerSecond: pipelineServiceBucketRefillPerSecond, BucketCost: pipelineServiceBucketCost,
		MaxActive: pipelineServiceDefaultMaxActive,
	}); err != nil {
		h.writeServiceAdmissionError(w, err)
		return
	}
	runID, err := newPipelineServiceRunID()
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	if err := freezePipelineServiceBundle(ctx, h.store, h.paths, record, runID); err != nil {
		h.writeError(w, http.StatusInternalServerError, "bundle_freeze_failed")
		return
	}
	root, _, _, _, _ := pipelineServiceRunPaths(h.paths, runID)
	accepted := false
	defer func() {
		if !accepted {
			_ = os.RemoveAll(root)
		}
	}()
	run := db.PipelineRun{
		ID: runID, Pipeline: name, Trigger: "service", PayloadJSON: string(payload), SpecHash: record.SpecHash,
		State: pipeline.RunRunning, StartedAt: now,
	}
	stages := make([]db.PipelineRunStage, 0, len(spec.Stages))
	for _, stage := range spec.Stages {
		stages = append(stages, db.PipelineRunStage{RunID: runID, StageID: stage.ID, State: pipeline.StagePending, NeedsJSON: marshalPipelineNeeds(stage.Needs)})
	}
	err = h.store.AdmitServiceRun(ctx, db.ServiceRunAdmission{
		Run: run, Stages: stages, ServiceRun: db.PipelineServiceRun{RunID: runID, PipelineName: name, CreatedAt: now},
		Now: now, BucketCapacity: pipelineServiceBucketCapacity, RefillPerSecond: pipelineServiceBucketRefillPerSecond,
		BucketCost: pipelineServiceBucketCost, MaxActive: pipelineServiceDefaultMaxActive,
	})
	if err != nil {
		h.writeServiceAdmissionError(w, err)
		return
	}
	accepted = true
	if _, err := advancePipelineRun(ctx, h.store, newPipelineStageEnqueuer(h.store, h.rawHome), record, spec, run, now); err != nil {
		fmt.Fprintf(h.stderr, "pipeline service: immediate advance %s: %v\n", runID, err)
	}
	h.writeJSON(w, http.StatusAccepted, pipelineServiceAccepted{
		RunID: runID, Status: pipeline.RunRunning, StatusURL: pipelineServiceStatusURL(runID), ReceiptURL: pipelineServiceReceiptURL(runID),
	})
}

func (h *pipelineServiceHandler) handleGetRun(w http.ResponseWriter, r *http.Request) {
	runID := strings.TrimSpace(r.PathValue("id"))
	serviceRun, run, ok := h.authorizedRun(w, r, runID)
	if !ok {
		return
	}
	if run.State == pipeline.RunSucceeded && serviceRun.ArtifactRelpath == "" {
		var err error
		serviceRun, err = h.finalize(r.Context(), runID)
		if err != nil {
			h.writeError(w, http.StatusInternalServerError, "proof_finalization_failed")
			return
		}
	}
	status := pipelineServiceStatus{
		RunID: run.ID, Pipeline: run.Pipeline, Status: run.State,
		ReceiptURL: pipelineServiceReceiptURL(run.ID),
	}
	if serviceRun.ArtifactRelpath != "" && serviceRun.ProofID != "" && !serviceRun.ProofVerifiedAt.IsZero() {
		status.ProofID = serviceRun.ProofID
		status.ProofVerified = true
		status.ProofVerificationKind = "stored_pipeline_outcome"
		status.BundleURL = pipelineServiceBundleURL(run.ID)
	}
	h.writeJSON(w, http.StatusOK, status)
}

func (h *pipelineServiceHandler) handleGetBundle(w http.ResponseWriter, r *http.Request) {
	runID := strings.TrimSpace(r.PathValue("id"))
	serviceRun, run, ok := h.authorizedRun(w, r, runID)
	if !ok {
		return
	}
	if run.State != pipeline.RunSucceeded {
		h.writeError(w, http.StatusConflict, "run_not_succeeded")
		return
	}
	if serviceRun.ArtifactRelpath == "" {
		var err error
		serviceRun, err = h.finalize(r.Context(), runID)
		if err != nil {
			h.writeError(w, http.StatusInternalServerError, "proof_finalization_failed")
			return
		}
	}
	path, err := containedServiceArtifactPath(h.paths, serviceRun.ArtifactRelpath)
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, "invalid_artifact_path")
		return
	}
	servePipelineServiceBundle(w, r, path, runID)
}

func (h *pipelineServiceHandler) authorizedRun(w http.ResponseWriter, r *http.Request, runID string) (db.PipelineServiceRun, db.PipelineRun, bool) {
	if !pipelineServiceRunIDPattern.MatchString(runID) {
		h.writeUnauthorized(w)
		return db.PipelineServiceRun{}, db.PipelineRun{}, false
	}
	serviceRun, ok, err := h.store.GetServiceRun(r.Context(), runID)
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, "internal_error")
		return db.PipelineServiceRun{}, db.PipelineRun{}, false
	}
	if !ok {
		h.writeUnauthorized(w)
		return db.PipelineServiceRun{}, db.PipelineRun{}, false
	}
	_, authorized, err := h.authenticate(r.Context(), serviceRun.PipelineName, r)
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, "internal_error")
		return db.PipelineServiceRun{}, db.PipelineRun{}, false
	}
	if !authorized {
		h.writeUnauthorized(w)
		return db.PipelineServiceRun{}, db.PipelineRun{}, false
	}
	run, ok, err := h.store.GetPipelineRun(r.Context(), runID)
	if err != nil || !ok {
		h.writeError(w, http.StatusInternalServerError, "inconsistent_service_run")
		return db.PipelineServiceRun{}, db.PipelineRun{}, false
	}
	return serviceRun, run, true
}

func (h *pipelineServiceHandler) authenticate(ctx context.Context, pipelineName string, r *http.Request) (db.PipelineExposure, bool, error) {
	token, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok {
		return db.PipelineExposure{}, false, nil
	}
	return h.store.AuthenticateExposureToken(ctx, pipelineName, token)
}

func bearerToken(header string) (string, bool) {
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" {
		return "", false
	}
	return parts[1], true
}

func (h *pipelineServiceHandler) finalize(ctx context.Context, runID string) (db.PipelineServiceRun, error) {
	h.finalizeMu.Lock()
	defer h.finalizeMu.Unlock()
	root, _, _, _, err := pipelineServiceRunPaths(h.paths, runID)
	if err != nil {
		return db.PipelineServiceRun{}, err
	}
	lockFile, err := os.OpenFile(filepath.Join(root, ".finalize.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return db.PipelineServiceRun{}, err
	}
	defer lockFile.Close()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		return db.PipelineServiceRun{}, err
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN) //nolint:errcheck -- unlock cannot repair a completed response
	serviceRun, ok, err := h.store.GetServiceRun(ctx, runID)
	if err != nil || !ok {
		return db.PipelineServiceRun{}, firstError(err, fmt.Errorf("service run %s not found", runID))
	}
	if serviceRun.ArtifactRelpath != "" {
		return serviceRun, nil
	}
	verified, err := verifyPipelineRunFromStore(ctx, h.store, h.paths, runID)
	if err != nil {
		return db.PipelineServiceRun{}, err
	}
	manifestJSON, err := proof.Marshal(verified.Manifest)
	if err != nil {
		return db.PipelineServiceRun{}, err
	}
	verifiedAt := time.Now().UTC()
	verified.Verification.VerifiedAt = verifiedAt.Format(time.RFC3339Nano)
	verificationJSON, err := json.Marshal(verified.Verification)
	if err != nil {
		return db.PipelineServiceRun{}, err
	}
	_, base, _, archive, err := pipelineServiceRunPaths(h.paths, runID)
	if err != nil {
		return db.PipelineServiceRun{}, err
	}
	if err := writePipelineServiceArchive(base, archive, manifestJSON, verificationJSON); err != nil {
		return db.PipelineServiceRun{}, err
	}
	digest, err := hashFileSHA256(archive)
	if err != nil {
		return db.PipelineServiceRun{}, err
	}
	relpath, err := filepath.Rel(h.paths.Home, archive)
	if err != nil || !containedRelativePath(relpath) {
		return db.PipelineServiceRun{}, errors.New("final service artifact escaped the Gitmoot home")
	}
	serviceRun.ArtifactRelpath = filepath.ToSlash(relpath)
	serviceRun.ArtifactSHA256 = digest
	serviceRun.ProofID = verified.Manifest.ProofID
	serviceRun.ProofVerifiedAt = verifiedAt
	if err := h.store.FinalizeServiceRun(ctx, serviceRun); err != nil {
		return db.PipelineServiceRun{}, err
	}
	return serviceRun, nil
}

func servicePipelinePublicSafetyError(spec pipeline.Spec) error {
	if len(spec.Stages) == 0 {
		return errors.New("v1 service exposure requires at least one shell stage")
	}
	for _, stage := range spec.Stages {
		if stage.Kind() != pipeline.StageKindShell || strings.TrimSpace(stage.Agent) != "" {
			return fmt.Errorf("pipeline stage %q is not a template-free shell stage", stage.ID)
		}
		if len(stage.EnvKeys) != 0 {
			return fmt.Errorf("pipeline stage %q declares env_keys; service exposure forbids keycard secrets", stage.ID)
		}
		if stage.Write {
			return fmt.Errorf("pipeline stage %q declares write; service exposure forbids writable stages", stage.ID)
		}
		if len(stage.Writes) != 0 {
			return fmt.Errorf("pipeline stage %q declares writes; service exposure forbids writable paths", stage.ID)
		}
		if len(stage.Reads) != 0 {
			return fmt.Errorf("pipeline stage %q declares reads; service exposure forbids extra filesystem grants", stage.ID)
		}
		if stage.Network {
			return fmt.Errorf("pipeline stage %q declares network; service exposure forbids network access", stage.ID)
		}
	}
	return nil
}

func servicePipelineIsPublicSafe(spec pipeline.Spec) bool {
	return servicePipelinePublicSafetyError(spec) == nil
}

func canonicalPipelineServicePayload(values map[string]pipeline.TypedValue) ([]byte, error) {
	native := make(map[string]any, len(values))
	for name, value := range values {
		switch value.Type {
		case pipeline.ServiceFieldString:
			native[name] = value.String
		case pipeline.ServiceFieldInteger:
			native[name] = value.Integer
		case pipeline.ServiceFieldBoolean:
			native[name] = value.Boolean
		default:
			return nil, fmt.Errorf("unsupported typed service value %q", value.Type)
		}
	}
	return json.Marshal(native)
}

func newPipelineServiceRunID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "psr-" + hex.EncodeToString(raw[:]), nil
}

func freezePipelineServiceBundle(ctx context.Context, store *db.Store, paths config.Paths, record db.Pipeline, runID string) error {
	root, base, sourceSpec, _, err := pipelineServiceRunPaths(paths, runID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	if err := exportPipelineBundle(ctx, store, record.Name, base, io.Discard, io.Discard); err != nil {
		return err
	}
	if err := os.WriteFile(sourceSpec, []byte(record.SpecYAML), 0o600); err != nil {
		return err
	}
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return os.Chmod(path, 0o700)
		}
		return os.Chmod(path, 0o600)
	})
}

func writePipelineServiceArchive(base, archive string, proofJSON, verificationJSON []byte) error {
	files := map[string][]byte{
		"proof.json":        append(append([]byte(nil), proofJSON...), '\n'),
		"verification.json": append(append([]byte(nil), verificationJSON...), '\n'),
	}
	err := filepath.WalkDir(base, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(base, path)
		if err != nil || !containedRelativePath(rel) {
			return errors.New("frozen bundle contains an invalid path")
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = content
		return nil
	})
	if err != nil {
		return err
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	temp, err := os.CreateTemp(filepath.Dir(archive), ".bundle-*.tmp")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	keep := false
	defer func() {
		_ = temp.Close()
		if !keep {
			_ = os.Remove(tempName)
		}
	}()
	if err := temp.Chmod(0o600); err != nil {
		return err
	}
	zw := zip.NewWriter(temp)
	fixedTime := time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, name := range names {
		header := &zip.FileHeader{Name: name, Method: zip.Deflate, Modified: fixedTime}
		header.SetMode(0o600)
		writer, err := zw.CreateHeader(header)
		if err != nil {
			_ = zw.Close()
			return err
		}
		if _, err := writer.Write(files[name]); err != nil {
			_ = zw.Close()
			return err
		}
	}
	if err := zw.Close(); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempName, archive); err != nil {
		return err
	}
	if err := os.Chmod(archive, 0o600); err != nil {
		return err
	}
	keep = true
	return nil
}

func hashFileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func containedServiceArtifactPath(paths config.Paths, relpath string) (string, error) {
	if !containedRelativePath(relpath) {
		return "", errors.New("artifact path is not a rooted relative path")
	}
	path := filepath.Join(paths.Home, filepath.FromSlash(relpath))
	rel, err := filepath.Rel(filepath.Join(paths.Home, pipelineServiceRunsDir), path)
	if err != nil || !containedRelativePath(rel) {
		return "", errors.New("artifact path is outside pipeline service storage")
	}
	return path, nil
}

func containedRelativePath(path string) bool {
	path = filepath.Clean(strings.TrimSpace(path))
	return path != "" && path != "." && !filepath.IsAbs(path) && path != ".." && !strings.HasPrefix(path, ".."+string(filepath.Separator))
}

func servePipelineServiceBundle(w http.ResponseWriter, r *http.Request, path, runID string) {
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="gitmoot-%s.zip"`, runID))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeFile(w, r, path)
}

func pipelineServiceStatusURL(runID string) string { return "/v1/pipelines/runs/" + runID }
func pipelineServiceBundleURL(runID string) string {
	return pipelineServiceStatusURL(runID) + "/bundle"
}
func pipelineServiceReceiptURL(runID string) string { return "/receipts/" + runID }

func (h *pipelineServiceHandler) writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="gitmoot-pipeline"`)
	h.writeError(w, http.StatusUnauthorized, "unauthorized")
}

func (h *pipelineServiceHandler) writeError(w http.ResponseWriter, status int, code string) {
	h.writeJSON(w, status, map[string]string{"error": code})
}

func (h *pipelineServiceHandler) writeServiceAdmissionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, db.ErrExposureDisabled):
		h.writeError(w, http.StatusForbidden, "exposure_disabled")
	case errors.Is(err, db.ErrServiceRunPipelineActive):
		h.writeError(w, http.StatusConflict, "pipeline_run_active")
	case errors.Is(err, db.ErrExposureRateLimited):
		var limited *db.ExposureRateLimitError
		retry := time.Second
		if errors.As(err, &limited) && limited.RetryAfter > 0 {
			retry = limited.RetryAfter
		}
		w.Header().Set("Retry-After", strconv.Itoa(max(1, int(retry.Seconds()))))
		h.writeError(w, http.StatusTooManyRequests, "rate_limited")
	case errors.Is(err, db.ErrServiceRunGlobalLimit):
		w.Header().Set("Retry-After", "1")
		h.writeError(w, http.StatusTooManyRequests, "active_run_limit")
	default:
		h.writeError(w, http.StatusInternalServerError, "internal_error")
	}
}

func (h *pipelineServiceHandler) writeJSON(w http.ResponseWriter, status int, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		http.Error(w, `{"error":"internal_error"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}

func firstError(primary, fallback error) error {
	if primary != nil {
		return primary
	}
	return fallback
}

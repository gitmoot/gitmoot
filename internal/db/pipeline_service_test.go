package db

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

func seedPipelineExposure(t *testing.T, store *Store, now time.Time) string {
	t.Helper()
	if err := store.CreateOrUpdatePipeline(context.Background(), samplePipeline()); err != nil {
		t.Fatal(err)
	}
	token, created, err := store.CreateExposure(context.Background(), PipelineExposure{
		PipelineName: "deploy-flow", SchemaVersion: 1,
		SchemaJSON: `{"version":1,"fields":{}}`, SchemaHash: "sha256:schema",
		Enabled: true, BucketTokens: 2, BucketUpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateExposure: %v", err)
	}
	if !created || token == "" {
		t.Fatalf("CreateExposure token=%q created=%v", token, created)
	}
	return token
}

func TestPipelineExposureCRUDTokenSecurityAndCascade(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	now := time.Date(2026, 7, 17, 10, 0, 0, 0, time.UTC)
	token := seedPipelineExposure(t, store, now)
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(decoded) != pipelineExposureTokenBytes {
		t.Fatalf("token is not 32 random base64url bytes: len=%d err=%v", len(decoded), err)
	}

	exposure, ok, err := store.GetExposure(ctx, "deploy-flow")
	if err != nil || !ok {
		t.Fatalf("GetExposure ok=%v err=%v", ok, err)
	}
	if !exposure.Enabled || exposure.SchemaVersion != 1 || exposure.SchemaHash != "sha256:schema" {
		t.Fatalf("exposure roundtrip = %+v", exposure)
	}
	if string(exposure.TokenHash) == token || len(exposure.TokenHash) != sha256.Size {
		t.Fatalf("stored token hash is plaintext or wrong length: %q (%d)", exposure.TokenHash, len(exposure.TokenHash))
	}
	var persistedText string
	if err := store.db.QueryRowContext(ctx, `SELECT schema_json || schema_hash || hex(token_hash) FROM pipeline_exposures WHERE pipeline_name = ?`, "deploy-flow").Scan(&persistedText); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(persistedText, token) {
		t.Fatal("plaintext bearer token was persisted")
	}

	if _, valid, err := store.AuthenticateExposureToken(ctx, "deploy-flow", token); err != nil || !valid {
		t.Fatalf("AuthenticateExposureToken(correct) valid=%v err=%v", valid, err)
	}
	if _, valid, err := store.AuthenticateExposureToken(ctx, "deploy-flow", token+"x"); err != nil || valid {
		t.Fatalf("AuthenticateExposureToken(wrong) valid=%v err=%v", valid, err)
	}

	// Re-inspection refreshes schema/enabled state but never silently rotates.
	beforeHash := append([]byte(nil), exposure.TokenHash...)
	if gotToken, created, err := store.CreateExposure(ctx, PipelineExposure{
		PipelineName: "deploy-flow", SchemaVersion: 1,
		SchemaJSON: `{"version":1,"fields":{"draft":{"type":"boolean"}}}`, SchemaHash: "sha256:updated",
		Enabled: true, BucketUpdatedAt: now,
	}); err != nil || created || gotToken != "" {
		t.Fatalf("refresh CreateExposure token=%q created=%v err=%v", gotToken, created, err)
	}
	refreshed, _, _ := store.GetExposure(ctx, "deploy-flow")
	if string(refreshed.TokenHash) != string(beforeHash) || refreshed.SchemaHash != "sha256:updated" {
		t.Fatalf("refresh rotated token or missed schema: %+v", refreshed)
	}

	rotated, err := store.RotateExposureToken(ctx, "deploy-flow")
	if err != nil || rotated == "" || rotated == token {
		t.Fatalf("RotateExposureToken token=%q err=%v", rotated, err)
	}
	if _, valid, _ := store.AuthenticateExposureToken(ctx, "deploy-flow", token); valid {
		t.Fatal("old token authenticated after rotation")
	}
	if _, valid, err := store.AuthenticateExposureToken(ctx, "deploy-flow", rotated); err != nil || !valid {
		t.Fatalf("new token valid=%v err=%v", valid, err)
	}
	if err := store.SetExposureEnabled(ctx, "deploy-flow", false); err != nil {
		t.Fatal(err)
	}
	disabled, _, _ := store.GetExposure(ctx, "deploy-flow")
	if disabled.Enabled {
		t.Fatal("SetExposureEnabled(false) did not persist")
	}

	if err := store.CreatePipelineRun(ctx, PipelineRun{ID: "prun-service", Pipeline: "deploy-flow", Trigger: "service", State: "running", StartedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateServiceRun(ctx, PipelineServiceRun{RunID: "prun-service", PipelineName: "deploy-flow", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	deleted, err := store.DeletePipeline(ctx, "deploy-flow")
	if err != nil || !deleted {
		t.Fatalf("DeletePipeline deleted=%v err=%v", deleted, err)
	}
	if _, ok, err := store.GetExposure(ctx, "deploy-flow"); err != nil || ok {
		t.Fatalf("exposure survived pipeline cascade: ok=%v err=%v", ok, err)
	}
	if _, ok, err := store.GetServiceRun(ctx, "prun-service"); err != nil || !ok {
		t.Fatalf("service receipt did not survive exposure deletion: ok=%v err=%v", ok, err)
	}
}

func TestDebitExposureBucketDeterministic(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	t0 := time.Date(2026, 7, 17, 11, 0, 0, 0, time.UTC)
	seedPipelineExposure(t, store, t0)

	remaining, allowed, err := store.DebitExposureBucket(ctx, "deploy-flow", t0, 5, 1, 1)
	if err != nil || !allowed || remaining != 1 {
		t.Fatalf("initial debit remaining=%v allowed=%v err=%v", remaining, allowed, err)
	}
	remaining, allowed, err = store.DebitExposureBucket(ctx, "deploy-flow", t0.Add(2*time.Second), 5, 1, 2)
	if err != nil || !allowed || math.Abs(remaining-1) > 1e-9 {
		t.Fatalf("refill debit remaining=%v allowed=%v err=%v", remaining, allowed, err)
	}
	remaining, allowed, err = store.DebitExposureBucket(ctx, "deploy-flow", t0.Add(2*time.Second), 5, 1, 5)
	if err != nil || allowed || math.Abs(remaining-1) > 1e-9 {
		t.Fatalf("denied debit remaining=%v allowed=%v err=%v", remaining, allowed, err)
	}
	exposure, _, _ := store.GetExposure(ctx, "deploy-flow")
	if math.Abs(exposure.BucketTokens-1) > 1e-9 || !exposure.BucketUpdatedAt.Equal(t0.Add(2*time.Second)) {
		t.Fatalf("persisted bucket = %+v", exposure)
	}
	if err := store.SetExposureEnabled(ctx, "deploy-flow", false); err != nil {
		t.Fatal(err)
	}
	if _, allowed, err := store.DebitExposureBucket(ctx, "deploy-flow", t0.Add(3*time.Second), 5, 1, 1); !errors.Is(err, ErrExposureDisabled) || allowed {
		t.Fatalf("disabled debit allowed=%v err=%v", allowed, err)
	}
}

func TestPipelineServiceRunCreateGet(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	if err := store.CreatePipelineRun(ctx, PipelineRun{ID: "prun-receipt", Pipeline: "kit", Trigger: "service", State: "succeeded", StartedAt: now}); err != nil {
		t.Fatal(err)
	}
	verifiedAt := now.Add(time.Minute)
	want := PipelineServiceRun{
		RunID: "prun-receipt", PipelineName: "kit", ArtifactRelpath: "artifacts/kit.tar.gz",
		ArtifactSHA256: "sha256:artifact", ProofID: "sha256:proof", ProofVerifiedAt: verifiedAt, CreatedAt: now,
	}
	if err := store.CreateServiceRun(ctx, want); err != nil {
		t.Fatalf("CreateServiceRun: %v", err)
	}
	got, ok, err := store.GetServiceRun(ctx, want.RunID)
	if err != nil || !ok {
		t.Fatalf("GetServiceRun ok=%v err=%v", ok, err)
	}
	if got.RunID != want.RunID || got.PipelineName != want.PipelineName || got.ArtifactRelpath != want.ArtifactRelpath || got.ArtifactSHA256 != want.ArtifactSHA256 || got.ProofID != want.ProofID || !got.ProofVerifiedAt.Equal(verifiedAt) || !got.CreatedAt.Equal(now) {
		t.Fatalf("service run roundtrip = %+v, want %+v", got, want)
	}
	if err := store.CreateServiceRun(ctx, PipelineServiceRun{RunID: "missing", PipelineName: "kit", CreatedAt: now}); err == nil {
		t.Fatal("CreateServiceRun accepted a missing pipeline_runs row")
	}
	if err := store.CreateServiceRun(ctx, PipelineServiceRun{RunID: want.RunID, PipelineName: "wrong", CreatedAt: now}); err == nil {
		t.Fatal("CreateServiceRun accepted a mismatched pipeline name")
	}
}

func TestAdmitServiceRunAtomicGuardsAndFinalize(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	now := time.Date(2026, 7, 17, 14, 0, 0, 0, time.UTC)
	seedPipelineExposure(t, store, now)
	admission := ServiceRunAdmission{
		Run:        PipelineRun{ID: "psr-00000000000000000000000000000001", Pipeline: "deploy-flow", Trigger: "service", PayloadJSON: `{"count":3}`, SpecHash: "sha256:spec", State: "running", StartedAt: now},
		Stages:     []PipelineRunStage{{RunID: "psr-00000000000000000000000000000001", StageID: "build", State: "pending"}},
		ServiceRun: PipelineServiceRun{RunID: "psr-00000000000000000000000000000001", PipelineName: "deploy-flow", CreatedAt: now},
		Now:        now, BucketCapacity: 5, RefillPerSecond: 1, BucketCost: 1, MaxActive: 2,
	}
	if err := store.AdmitServiceRun(ctx, admission); err != nil {
		t.Fatalf("AdmitServiceRun: %v", err)
	}
	if run, ok, err := store.GetPipelineRun(ctx, admission.Run.ID); err != nil || !ok || run.Trigger != "service" || run.PayloadJSON != `{"count":3}` {
		t.Fatalf("admitted run=%+v ok=%v err=%v", run, ok, err)
	}
	if stages, err := store.ListPipelineRunStages(ctx, admission.Run.ID); err != nil || len(stages) != 1 || stages[0].StageID != "build" {
		t.Fatalf("admitted stages=%v err=%v", stages, err)
	}
	if receipt, ok, err := store.GetServiceRun(ctx, admission.Run.ID); err != nil || !ok || receipt.PipelineName != "deploy-flow" {
		t.Fatalf("admitted receipt=%+v ok=%v err=%v", receipt, ok, err)
	}
	exposure, _, _ := store.GetExposure(ctx, "deploy-flow")
	if exposure.BucketTokens != 1 {
		t.Fatalf("admission bucket=%v, want 1", exposure.BucketTokens)
	}

	conflict := admission
	conflict.Run.ID = "psr-00000000000000000000000000000002"
	conflict.ServiceRun.RunID = conflict.Run.ID
	conflict.Stages[0].RunID = conflict.Run.ID
	if err := store.AdmitServiceRun(ctx, conflict); !errors.Is(err, ErrServiceRunPipelineActive) {
		t.Fatalf("overlap admission err=%v", err)
	}
	if _, ok, _ := store.GetPipelineRun(ctx, conflict.Run.ID); ok {
		t.Fatal("overlap denial left a partial run")
	}
	exposure, _, _ = store.GetExposure(ctx, "deploy-flow")
	if exposure.BucketTokens != 1 {
		t.Fatalf("overlap denial consumed a token: %v", exposure.BucketTokens)
	}
	other := samplePipeline()
	other.Name = "other-flow"
	if err := store.CreateOrUpdatePipeline(ctx, other); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateExposure(ctx, PipelineExposure{
		PipelineName: other.Name, SchemaVersion: 1, SchemaJSON: `{"version":1,"fields":{}}`, SchemaHash: "schema",
		Enabled: true, BucketTokens: 2, BucketUpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	global := admission
	global.Run.ID, global.Run.Pipeline = "psr-00000000000000000000000000000003", other.Name
	global.ServiceRun.RunID, global.ServiceRun.PipelineName = global.Run.ID, other.Name
	global.Stages = []PipelineRunStage{{RunID: global.Run.ID, StageID: "build", State: "pending"}}
	global.MaxActive = 1
	if err := store.AdmitServiceRun(ctx, global); !errors.Is(err, ErrServiceRunGlobalLimit) {
		t.Fatalf("global limit admission err=%v", err)
	}
	if _, ok, _ := store.GetPipelineRun(ctx, global.Run.ID); ok {
		t.Fatal("global-limit denial left a partial run")
	}
	otherExposure, _, _ := store.GetExposure(ctx, other.Name)
	if otherExposure.BucketTokens != 2 {
		t.Fatalf("global-limit denial consumed a token: %v", otherExposure.BucketTokens)
	}

	verifiedAt := now.Add(time.Minute)
	finalized := PipelineServiceRun{
		RunID: admission.Run.ID, ArtifactRelpath: "pipeline-service-runs/run/bundle.zip",
		ArtifactSHA256: "sha256:artifact", ProofID: "sha256:proof", ProofVerifiedAt: verifiedAt,
	}
	if err := store.FinalizeServiceRun(ctx, finalized); err != nil {
		t.Fatalf("FinalizeServiceRun: %v", err)
	}
	if err := store.FinalizeServiceRun(ctx, finalized); err != nil {
		t.Fatalf("idempotent FinalizeServiceRun: %v", err)
	}
	conflictingFinal := finalized
	conflictingFinal.ProofID = "sha256:other"
	if err := store.FinalizeServiceRun(ctx, conflictingFinal); err == nil {
		t.Fatal("FinalizeServiceRun accepted conflicting metadata")
	}
}

func TestCheckServiceRunAdmissionIsReadOnlyAndMatchesGuards(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	now := time.Date(2026, 7, 17, 15, 0, 0, 0, time.UTC)
	seedPipelineExposure(t, store, now)
	check := ServiceRunAdmissionCheck{
		PipelineName: "deploy-flow", Now: now, BucketCapacity: 5,
		RefillPerSecond: 1, BucketCost: 1, MaxActive: 2,
	}
	before, _, err := store.GetExposure(ctx, check.PipelineName)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CheckServiceRunAdmission(ctx, check); err != nil {
		t.Fatalf("eligible pre-admission check: %v", err)
	}
	after, _, err := store.GetExposure(ctx, check.PipelineName)
	if err != nil || after.BucketTokens != before.BucketTokens || !after.BucketUpdatedAt.Equal(before.BucketUpdatedAt) {
		t.Fatalf("read-only check changed bucket: before=%+v after=%+v err=%v", before, after, err)
	}

	if _, allowed, err := store.DebitExposureBucket(ctx, check.PipelineName, now, 5, 0, 2); err != nil || !allowed {
		t.Fatalf("drain bucket allowed=%v err=%v", allowed, err)
	}
	if err := store.CheckServiceRunAdmission(ctx, check); !errors.Is(err, ErrExposureRateLimited) {
		t.Fatalf("empty-bucket precheck err=%v", err)
	}
	empty, _, err := store.GetExposure(ctx, check.PipelineName)
	if err != nil || empty.BucketTokens != 0 {
		t.Fatalf("rate precheck mutated empty bucket: %+v err=%v", empty, err)
	}

	if err := store.CreatePipelineRun(ctx, PipelineRun{
		ID: "psr-55555555555555555555555555555555", Pipeline: check.PipelineName, Trigger: "service", State: "running", StartedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	check.Now = now.Add(time.Second)
	if err := store.CheckServiceRunAdmission(ctx, check); !errors.Is(err, ErrServiceRunPipelineActive) {
		t.Fatalf("active-pipeline precheck err=%v", err)
	}
}

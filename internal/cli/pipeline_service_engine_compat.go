package cli

import (
	"context"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/pipeline"
	"github.com/gitmoot/gitmoot/internal/proof"
)

const (
	pipelineServiceArtifactsDir = pipeline.PipelineServiceArtifactsDir
	pipelineServiceRunsDir      = pipeline.PipelineServiceRunsDir
)

var pipelineServiceRunIDPattern = pipeline.PipelineServiceRunIDPattern

type verifiedPipelineRunProof = pipeline.VerifiedPipelineRunProof

var (
	errPipelineServiceArtifactCollectionFailed = pipeline.ErrPipelineServiceArtifactCollectionFailed
	errPipelineServiceArtifactBundleTooLarge   = pipeline.ErrPipelineServiceArtifactBundleTooLarge
)

func pipelineServiceRunPaths(paths config.Paths, runID string) (root, base, sourceSpec, archive string, err error) {
	return pipeline.PipelineServiceRunPaths(paths, runID)
}

func advancePipelineRun(ctx context.Context, store *db.Store, enqueue pipeline.PipelineStageEnqueuer, rec db.Pipeline, spec pipeline.Spec, run db.PipelineRun, now time.Time) (db.PipelineRun, error) {
	return pipeline.AdvancePipelineRun(ctx, store, enqueue, rec, spec, run, now)
}

func verifyPipelineRunFromStore(ctx context.Context, store *db.Store, paths config.Paths, runID string) (verifiedPipelineRunProof, error) {
	return pipeline.VerifyPipelineRunFromStore(ctx, store, paths, runID)
}

func loadPipelineServiceArtifacts(paths config.Paths, runID string) ([]proof.ArtifactEvidence, error) {
	return pipeline.LoadPipelineServiceArtifacts(paths, runID)
}

func verifyPipelineServiceArtifactProof(manifest proof.Manifest, actual []proof.ArtifactEvidence) error {
	return pipeline.VerifyPipelineServiceArtifactProof(manifest, actual)
}

func verifyPipelineServiceArchiveArtifacts(archivePath string, manifest proof.Manifest) error {
	return pipeline.VerifyPipelineServiceArchiveArtifacts(archivePath, manifest)
}

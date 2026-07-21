package cli

import (
	"context"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/pipeline"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

type pipelineStageEnvAccess = pipeline.PipelineStageEnvAccess
type pipelineEnvUnresolved = pipeline.PipelineEnvUnresolved
type pipelineEnvironmentResolution = pipeline.PipelineEnvironmentResolution
type pipelineEnvFileInspection = pipeline.PipelineEnvFileInspection

const (
	pipelineKeySourceOwn     = pipeline.PipelineKeySourceOwn
	pipelineKeySourceShared  = pipeline.PipelineKeySourceShared
	pipelineKeySourceDefault = pipeline.PipelineKeySourceDefault

	pipelineEnvFileStatusNone        = pipeline.PipelineEnvFileStatusNone
	pipelineEnvFileStatusOK          = pipeline.PipelineEnvFileStatusOK
	pipelineEnvFileStatusMissing     = pipeline.PipelineEnvFileStatusMissing
	pipelineEnvFileStatusBadMode     = pipeline.PipelineEnvFileStatusBadMode
	pipelineEnvFileStatusBadOwner    = pipeline.PipelineEnvFileStatusBadOwner
	pipelineEnvFileStatusBadLocation = pipeline.PipelineEnvFileStatusBadLocation
	pipelineEnvFileStatusInvalid     = pipeline.PipelineEnvFileStatusInvalid
)

func resolvePipelineEnvironment(ctx context.Context, store *db.Store, home string, spec pipeline.Spec) (pipelineEnvironmentResolution, error) {
	return pipeline.ResolvePipelineEnvironment(ctx, store, home, spec)
}

func pipelineEnvironmentResolutionError(spec pipeline.Spec, unresolved []pipelineEnvUnresolved) error {
	return pipeline.PipelineEnvironmentResolutionError(spec, unresolved)
}

func resolvePipelineStageEnvAccess(ctx context.Context, store *db.Store, home string, spec pipeline.Spec, stage pipeline.Stage) (pipelineStageEnvAccess, error) {
	return pipeline.ResolvePipelineStageEnvAccess(ctx, store, home, spec, stage)
}

func wrapPipelineEnvDeliveryAdapter(store *db.Store, home string, payload workflow.JobPayload, inner workflow.DeliveryAdapter) workflow.DeliveryAdapter {
	return pipeline.WrapPipelineEnvDeliveryAdapter(store, home, payload, inner)
}

func configPathsForPipelineStore(store *db.Store, home string) (config.Paths, error) {
	return pipeline.ConfigPathsForPipelineStore(store, home)
}

func classifyPipelineEnvFile(ctx context.Context, store *db.Store, home, declared string) pipelineEnvFileInspection {
	return pipeline.ClassifyPipelineEnvFile(ctx, store, home, declared)
}

func resolveKeychainPath(store *db.Store, home string) (string, error) {
	return pipeline.ResolveKeychainPath(store, home)
}

func loadValidatedKeychainFile(ctx context.Context, store *db.Store, home string) (string, map[string]string, error) {
	return pipeline.LoadValidatedKeychainFile(ctx, store, home)
}

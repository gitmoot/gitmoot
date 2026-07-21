package cli

import (
	"context"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/pipeline"
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

func classifyPipelineEnvFile(ctx context.Context, store *db.Store, home, declared string) pipelineEnvFileInspection {
	return pipeline.ClassifyPipelineEnvFile(ctx, store, home, declared)
}

func resolveKeychainPath(store *db.Store, home string) (string, error) {
	return pipeline.ResolveKeychainPath(store, home)
}

func loadValidatedKeychainFile(ctx context.Context, store *db.Store, home string) (string, map[string]string, error) {
	return pipeline.LoadValidatedKeychainFile(ctx, store, home)
}

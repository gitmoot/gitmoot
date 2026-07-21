package cli

import (
	"context"
	"io"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/pipeline"
)

const pipelineProgressLineBytes = pipeline.PipelineProgressLineBytes

const (
	pipelineProgressThreshold = pipeline.PipelineProgressThreshold
	pipelineProgressInterval  = pipeline.PipelineProgressInterval
)

type pipelineProgressEventPayload = pipeline.PipelineProgressEventPayload
type pipelineProgressLineTracker = pipeline.PipelineProgressLineTracker

func sanitizePipelineProgressLine(value string) string {
	return pipeline.SanitizePipelineProgressLine(value)
}

func runtimeOutputWriter(writers ...io.Writer) io.Writer {
	return pipeline.RuntimeOutputWriter(writers...)
}

func pipelineProgressTicks(ctx context.Context, threshold, interval time.Duration) <-chan time.Time {
	return pipeline.PipelineProgressTicks(ctx, threshold, interval)
}

func emitPipelineProgress(ctx context.Context, store *db.Store, stdout io.Writer, jobID string, startedAt time.Time, tracker *pipelineProgressLineTracker, ticks <-chan time.Time) {
	pipeline.EmitPipelineProgress(ctx, store, stdout, jobID, startedAt, tracker, ticks)
}

func marshalPipelineNeeds(needs []string) string {
	return pipeline.MarshalPipelineNeeds(needs)
}

func decodePipelineNeeds(value string) []string {
	return pipeline.DecodePipelineNeeds(value)
}

func pipelineStageJobID(runID, stageID string, attempt int) string {
	return pipeline.PipelineStageJobID(runID, stageID, attempt)
}

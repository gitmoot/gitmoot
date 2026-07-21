package cli

const (
	defaultMemoryIngestSweepPipeline             = "memory-ingest-sweep"
	defaultMemoryGroomProposePipeline            = "memory-groom-propose"
	defaultMemoryPipelineGroup                   = "Gitmoot System"
	defaultMemoryIngestSweepPipelineDescription  = "Sweeps configured note sources into staged memory observations."
	defaultMemoryGroomProposePipelineDescription = "Proposes memory dedupe/merge/expiry actions for human review."
	defaultMemoryPipelineBinEnv                  = "GITMOOT_PIPELINE_BIN"
)

func installDefaultsEnabledLabel(enabled bool) string {
	if enabled {
		return "enabled"
	}
	return "manual-only"
}

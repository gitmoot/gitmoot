package config

// PipelineRemotePolicy is the optional default GitHub remote used by pipeline
// publish/pull. It shares the template remote's repo/ref/path config shape.
type PipelineRemotePolicy GitHubRemotePolicy

const (
	DefaultPipelineRemoteRef  = "main"
	DefaultPipelineRemotePath = "pipelines"
)

func LoadPipelineRemote(paths Paths) (PipelineRemotePolicy, error) {
	policy, err := loadGitHubRemote(paths, "pipeline_remote")
	return PipelineRemotePolicy(policy), err
}

func EnsurePipelineRemoteSection(paths Paths) error {
	return ensureGitHubRemoteSection(paths, "pipeline_remote")
}

package config

// PipelineRemotePolicy is the optional default GitHub remote used by pipeline
// publish/pull. It shares the template remote's repo/ref/path config shape.
type PipelineRemotePolicy GitHubRemotePolicy

const (
	DefaultPipelineRemoteRef  = "main"
	DefaultPipelineRemotePath = "pipelines"
)

func DefaultPipelineRemotePolicy() PipelineRemotePolicy {
	return PipelineRemotePolicy{}
}

func (p PipelineRemotePolicy) Configured() bool {
	return GitHubRemotePolicy(p).Configured()
}

func (p PipelineRemotePolicy) ResolvedRef() string {
	return GitHubRemotePolicy(p).ResolvedRef(DefaultPipelineRemoteRef)
}

func (p PipelineRemotePolicy) ResolvedPath() string {
	return GitHubRemotePolicy(p).ResolvedPath(DefaultPipelineRemotePath)
}

func LoadPipelineRemote(paths Paths) (PipelineRemotePolicy, error) {
	policy, err := loadGitHubRemote(paths, "pipeline_remote")
	return PipelineRemotePolicy(policy), err
}

func EnsurePipelineRemoteSection(paths Paths) error {
	return ensureGitHubRemoteSection(paths, "pipeline_remote")
}

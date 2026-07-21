package config

// TemplateRemotePolicy is the host-level default GitHub repo the agent-template
// publish/pull/add commands fall back to when --repo is omitted, read from the
// [template_remote] section of the gitmoot config (#476). It is OFF BY DEFAULT:
// with no [template_remote] section (or an empty repo) Configured() is false and
// those commands require an explicit --repo, so behavior is byte-identical to a
// config without the section. It follows the LoadEventsPolicy line-parser
// pattern in orchestrate.go.
type TemplateRemotePolicy GitHubRemotePolicy

const (
	// DefaultTemplateRemoteRef is the ref used when [template_remote].ref is unset.
	DefaultTemplateRemoteRef = "main"
	// DefaultTemplateRemotePath is the subdir used when [template_remote].path is
	// unset: the directory holding the template .md files.
	DefaultTemplateRemotePath = "templates"
)

func LoadTemplateRemote(paths Paths) (TemplateRemotePolicy, error) {
	policy, err := loadGitHubRemote(paths, "template_remote")
	return TemplateRemotePolicy(policy), err
}

// EnsureTemplateRemoteSection appends an empty [template_remote] section to the
// config when it is absent, so SetConfigScalar (which only edits keys that
// already exist) can drive `agent template remote set` on a config created
// before this feature. It is idempotent: when the section already exists it is a
// no-op. Fresh configs (DefaultConfig) ship the section, so this only ever fires
// for older configs.
func EnsureTemplateRemoteSection(paths Paths) error {
	return ensureGitHubRemoteSection(paths, "template_remote")
}

package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/runtime"
	"github.com/jerryfane/gitmoot/internal/subprocess"
)

var curatedBaseEnvNames = []string{
	"PATH",
	"HOME",
	"USER",
	"LOGNAME",
	"SHELL",
	"TMPDIR",
	"TMP",
	"TEMP",
	"TZ",
	"LANG",
	"LANGUAGE",
	"TERM",
	"COLORTERM",
	"NO_COLOR",
	"XDG_CONFIG_HOME",
	"XDG_CACHE_HOME",
	"XDG_DATA_HOME",
	"XDG_STATE_HOME",
	"GOTOOLCHAIN",
	"GIT_AUTHOR_NAME",
	"GIT_AUTHOR_EMAIL",
	"GIT_COMMITTER_NAME",
	"GIT_COMMITTER_EMAIL",
	"GITMOOT_HOME",
}

var curatedRuntimeEnvNames = map[string][]string{
	runtime.CodexRuntime: {"CODEX_HOME"},
	// Transitional P1 exception: Claude still receives its ambient auth and
	// config location. Moving state and removing ambient auth belong to P2/P3.
	runtime.ClaudeRuntime:  {"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CONFIG_DIR"},
	runtime.KimiRuntime:    {},
	runtime.KimiCLIRuntime: {},
	runtime.ShellRuntime:   {},
}

func curatedJobRunner(home string, runtimeName string) (subprocess.Runner, error) {
	paths, err := pathsFromFlag(home)
	if err != nil {
		return nil, err
	}
	cfg, err := config.LoadCredentialsConfig(paths)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("load credentials config: %w", err)
	}
	if !cfg.EnvCuration {
		return nil, nil
	}
	var scratch string
	if cfg.GitHub == config.CredentialsGitHubDeny {
		scratch, err = os.MkdirTemp("", "gitmoot-gh-config-*")
		if err != nil {
			return nil, fmt.Errorf("create GitHub credential scratch: %w", err)
		}
		if err := os.Chmod(scratch, 0o700); err != nil {
			_ = os.RemoveAll(scratch)
			return nil, fmt.Errorf("chmod GitHub credential scratch: %w", err)
		}
	}
	baseEnv := curatedRuntimeBaseEnv(cfg, runtimeName, os.Environ(), scratch)
	runner := subprocess.CuratedGroupRunner{BaseEnv: baseEnv}
	if scratch != "" {
		runner.ScratchDirs = []string{scratch}
	}
	return runner, nil
}

func curatedRuntimeBaseEnv(cfg config.CredentialsConfig, runtimeName string, environ []string, githubScratch string) []string {
	allowed := make(map[string]struct{}, len(curatedBaseEnvNames)+4)
	for _, name := range curatedBaseEnvNames {
		allowed[name] = struct{}{}
	}
	for _, name := range curatedRuntimeEnvNames[runtimeName] {
		allowed[name] = struct{}{}
	}
	base := make([]string, 0, len(environ)+2)
	for _, entry := range environ {
		name, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		githubEnv := strings.HasPrefix(name, "GH_") || strings.HasPrefix(name, "GITHUB_")
		if githubEnv && cfg.GitHub == config.CredentialsGitHubDeny {
			continue
		}
		_, exact := allowed[name]
		if exact || strings.HasPrefix(name, "LC_") || matchesCredentialPassthrough(name, cfg.EnvPassthrough) || (githubEnv && cfg.GitHub == config.CredentialsGitHubInherit) {
			base = append(base, entry)
		}
	}
	if cfg.GitHub == config.CredentialsGitHubDeny {
		base = append(base, "GH_CONFIG_DIR="+githubScratch, "GH_PROMPT_DISABLED=1")
	}
	return base
}

func matchesCredentialPassthrough(name string, patterns []string) bool {
	for _, pattern := range patterns {
		if strings.HasSuffix(pattern, "*") {
			if strings.HasPrefix(name, strings.TrimSuffix(pattern, "*")) {
				return true
			}
			continue
		}
		if name == pattern {
			return true
		}
	}
	return false
}

func runtimeJobRunner(home string, runtimeName string, outer subprocess.Runner) (subprocess.Runner, error) {
	curated, err := curatedJobRunner(home, runtimeName)
	if err != nil || curated == nil {
		return outer, err
	}
	if outer == nil {
		return curated, nil
	}
	switch runner := outer.(type) {
	case subprocess.GroupRunner:
		return curated, nil
	case *subprocess.GroupRunner:
		return curated, nil
	case subprocess.TeeRunner:
		if runner.Inner == nil {
			runner.Inner = curated.(subprocess.StreamRunner)
		} else if _, ok := runner.Inner.(subprocess.GroupRunner); ok {
			runner.Inner = curated.(subprocess.StreamRunner)
		}
		return runner, nil
	case *subprocess.TeeRunner:
		if runner.Inner == nil {
			runner.Inner = curated.(subprocess.StreamRunner)
		} else if _, ok := runner.Inner.(subprocess.GroupRunner); ok {
			runner.Inner = curated.(subprocess.StreamRunner)
		}
		return runner, nil
	case subprocess.EnvInjectingRunner:
		if runner.Inner == nil {
			runner.Inner = curated
		} else if _, ok := runner.Inner.(subprocess.GroupRunner); ok {
			runner.Inner = curated
		}
		return runner, nil
	case *subprocess.EnvInjectingRunner:
		if runner.Inner == nil {
			runner.Inner = curated
		} else if _, ok := runner.Inner.(subprocess.GroupRunner); ok {
			runner.Inner = curated
		}
		return runner, nil
	case subprocess.WrappingRunner:
		if runner.Inner == nil {
			runner.Inner = curated
		} else if _, ok := runner.Inner.(subprocess.GroupRunner); ok {
			runner.Inner = curated
		}
		return runner, nil
	case *subprocess.WrappingRunner:
		if runner.Inner == nil {
			runner.Inner = curated
		} else if _, ok := runner.Inner.(subprocess.GroupRunner); ok {
			runner.Inner = curated
		}
		return runner, nil
	default:
		// Explicit custom/fake runners remain authoritative test and extension seams.
		return outer, nil
	}
}

func runtimeFactoryFor(home string, runtimeName string) (runtime.Factory, error) {
	factory := newRuntimeFactory()
	if factory.Runner != nil {
		return factory, nil
	}
	runner, err := runtimeJobRunner(home, runtimeName, factory.Runner)
	if err != nil {
		return runtime.Factory{}, err
	}
	factory.Runner = runner
	return factory, nil
}

func runtimeAdapterFor(home string, runtimeName string, checkout string) (runtime.Adapter, error) {
	factory, err := runtimeFactoryFor(home, runtimeName)
	if err != nil {
		return nil, err
	}
	return runtimeStartAdapterFor(factory, runtimeName, checkout)
}

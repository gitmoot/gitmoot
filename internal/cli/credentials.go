package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/credgw"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/subprocess"
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
	// Claude's ambient auth names remain available only for the explicit-empty
	// authoritative-file fallback. A populated runtime-auth.env overlay is
	// appended later and wins, including explicit blanks for absent names.
	runtime.ClaudeRuntime:  {"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CONFIG_DIR"},
	runtime.KimiRuntime:    {},
	runtime.KimiCLIRuntime: {},
	// omp is a multi-provider ROUTING harness, so its curated set is ROUTING
	// PLUMBING ONLY — which profile to run under, where the coding-agent state
	// lives, which model each routing tier resolves to, and the optional auth
	// broker endpoint. NO raw provider key (ANTHROPIC_*, OPENAI_*, GEMINI_*, the
	// other 38) is listed on purpose.
	//
	// Scope of that claim, precisely: WHEN `[credentials] env_curation = true`,
	// curation is fail-closed, so an unlisted name is dropped from the child env and
	// a raw provider key can only reach omp through omp's own profile auth storage
	// or an EXPLICIT credentials env_passthrough entry — the owner's lever, an act,
	// not an inheritance. WITH CURATION OFF — which is the DEFAULT, `[credentials]`
	// ships commented out (internal/config/init.go) and curatedJobRunner returns a
	// nil runner below — this map is never consulted and omp inherits the daemon's
	// whole environment, including any ambient ANTHROPIC_*/OPENAI_* keys, exactly
	// like every other runtime. Curation is the owner's opt-in, not a property of
	// registering omp.
	//
	// One deliberate exception even under curation: the OMP_AUTH_BROKER_* pair below
	// IS a credential-acquisition path, not just plumbing. omp's
	// resolveAuthBrokerConfig keys on exactly OMP_AUTH_BROKER_URL +
	// OMP_AUTH_BROKER_TOKEN and then discovers a BROKER-BACKED auth storage instead
	// of the local store, so an operator who exports that pair for the daemon has
	// omp authenticate through the broker with no further act. It is listed on
	// purpose (broker mode is how a fleet supplies omp auth without scattering raw
	// keys), and the pair is INDIVISIBLE: omp throws when the URL is set and no
	// token is available, so dropping only the token converts an inherited URL into
	// a hard omp failure. Drop BOTH (and let env_passthrough carry them) if the
	// owner wants omp strictly fail-closed.
	//
	// Caveat worth stating where the plumbing lives: `--profile` selects omp's
	// auth/state store, it does NOT isolate process env, so a passthrough'd key is
	// visible to every omp profile this daemon runs.
	runtime.OmpRuntime: {
		"OMP_PROFILE",
		"PI_PROFILE",
		"PI_CODING_AGENT_DIR",
		"PI_SMOL_MODEL",
		"PI_SLOW_MODEL",
		"PI_PLAN_MODEL",
		"OMP_AUTH_BROKER_URL",
		"OMP_AUTH_BROKER_TOKEN",
	},
	runtime.ShellRuntime: {},
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
		// Only the random path name is reserved here; the runner recreates the
		// directory 0700 for each subprocess and removes it afterwards. Removing
		// it now means an adapter that is built but never run leaks nothing.
		if err := os.RemoveAll(scratch); err != nil {
			return nil, fmt.Errorf("reset GitHub credential scratch: %w", err)
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
	runner, _, _, err := runtimeJobRunnerWithAuth(home, runtimeName, outer)
	return runner, err
}

// runtimeJobRunnerWithAuth is runtimeJobRunner plus the resolved Claude auth
// state/source used by `auth probe` and the one-shot doctor. Production adapter
// construction uses runtimeJobRunner and therefore shares this exact path.
func runtimeJobRunnerWithAuth(home string, runtimeName string, outer subprocess.Runner) (subprocess.Runner, runtimeAuthFile, string, error) {
	var authState runtimeAuthFile
	var authSource string
	var credentialsCfg config.CredentialsConfig
	var resolvedHome string
	if runtimeName == runtime.ClaudeRuntime {
		authSource = runtimeAuthFileName
		paths, err := pathsFromFlag(home)
		if err != nil {
			return nil, authState, authSource, err
		}
		if _, err := bootstrapRuntimeAuth(paths.Home, runtimeAuthEnvLookup, runtimeAuthLogf); err != nil {
			return nil, authState, authSource, fmt.Errorf("bootstrap Claude runtime auth: %w", err)
		}
		authState, err = loadRuntimeAuthFile(paths.Home)
		if err != nil {
			return nil, authState, authSource, err
		}
		authSource = runtimeAuthSource(authState, runtimeAuthEnvLookup)
		warnRuntimeAuthConflicts(authState, runtimeAuthEnvLookup, runtimeAuthLogf)
		credentialsCfg = config.DefaultCredentialsConfig()
		loadedCredentials, loadErr := config.LoadCredentialsConfig(paths)
		if loadErr == nil {
			credentialsCfg = loadedCredentials
		} else if !errors.Is(loadErr, os.ErrNotExist) {
			return nil, authState, authSource, fmt.Errorf("load credentials config: %w", loadErr)
		}
		resolvedHome = paths.Home
	}

	curated, err := curatedJobRunner(home, runtimeName)
	if err != nil {
		return nil, authState, authSource, err
	}
	if runtimeName == runtime.ClaudeRuntime && credentialsCfg.ModelGateway {
		base := outer
		if curated != nil {
			base = graftRuntimeBaseRunner(outer, curated)
		}
		gatewayRunner, err := buildModelGatewayRunner(resolvedHome, credentialsCfg, authState, base)
		if err != nil {
			return nil, authState, authSource, err
		}
		return gatewayRunner, authState, runtimeAuthFileName + " via model gateway", nil
	}
	if runtimeName == runtime.ClaudeRuntime {
		authEnv := runtimeAuthInjectionEnv(authState)
		if curated != nil {
			base := curated.(subprocess.CuratedGroupRunner)
			base.BaseEnv = append(base.BaseEnv, authEnv...)
			curated = base
		} else if len(authEnv) > 0 {
			baseEnv := append([]string{}, os.Environ()...)
			baseEnv = append(baseEnv, authEnv...)
			curated = subprocess.CuratedGroupRunner{BaseEnv: baseEnv}
		}
	}
	if curated == nil {
		return outer, authState, authSource, nil
	}
	return graftRuntimeBaseRunner(outer, curated), authState, authSource, nil
}

func graftRuntimeBaseRunner(outer subprocess.Runner, curated subprocess.Runner) subprocess.Runner {
	if outer == nil {
		return curated
	}
	switch runner := outer.(type) {
	case subprocess.GroupRunner:
		return curated
	case *subprocess.GroupRunner:
		return curated
	case subprocess.TeeRunner:
		inner := subprocess.Runner(runner.Inner)
		if inner == nil {
			inner = subprocess.GroupRunner{}
		}
		if grafted, ok := graftRuntimeBaseRunner(inner, curated).(subprocess.StreamRunner); ok {
			runner.Inner = grafted
		}
		return runner
	case *subprocess.TeeRunner:
		inner := subprocess.Runner(runner.Inner)
		if inner == nil {
			inner = subprocess.GroupRunner{}
		}
		if grafted, ok := graftRuntimeBaseRunner(inner, curated).(subprocess.StreamRunner); ok {
			runner.Inner = grafted
		}
		return runner
	case subprocess.EnvInjectingRunner:
		if runner.Inner == nil {
			runner.Inner = subprocess.GroupRunner{}
		}
		runner.Inner = graftRuntimeBaseRunner(runner.Inner, curated)
		return runner
	case *subprocess.EnvInjectingRunner:
		if runner.Inner == nil {
			runner.Inner = subprocess.GroupRunner{}
		}
		runner.Inner = graftRuntimeBaseRunner(runner.Inner, curated)
		return runner
	case subprocess.WrappingRunner:
		if runner.Inner == nil {
			runner.Inner = subprocess.GroupRunner{}
		}
		runner.Inner = graftRuntimeBaseRunner(runner.Inner, curated)
		return runner
	case *subprocess.WrappingRunner:
		if runner.Inner == nil {
			runner.Inner = subprocess.GroupRunner{}
		}
		runner.Inner = graftRuntimeBaseRunner(runner.Inner, curated)
		return runner
	default:
		// Explicit custom/fake runners remain authoritative test and extension seams.
		return outer
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
	adapter, err := runtimeStartAdapterFor(factory, runtimeName, checkout)
	if err != nil {
		return nil, err
	}
	gatewayRunner, _ := factory.Runner.(*credgw.Runner)
	return wrapModelGatewayAdapter(adapter, gatewayRunner), nil
}

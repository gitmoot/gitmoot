package plugincontext

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/buildinfo"
	"github.com/gitmoot/gitmoot/internal/config"
	gitutil "github.com/gitmoot/gitmoot/internal/git"
	"github.com/gitmoot/gitmoot/internal/presence"
	"github.com/gitmoot/gitmoot/internal/subprocess"
)

const DefaultHookEventName = "SessionStart"

type HookInput struct {
	CWD           string
	HookEventName string
}

type BuildOptions struct {
	Input          HookInput
	Info           buildinfo.Info
	Paths          config.Paths
	Runner         subprocess.Runner
	SnapshotLoader func(context.Context, config.Paths, string) (presence.Snapshot, error)
}

type HookOutput struct {
	HookSpecificOutput *HookSpecificOutput `json:"hookSpecificOutput,omitempty"`
}

type HookSpecificOutput struct {
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext"`
}

type repoInfo struct {
	Root     string
	Remote   string
	Repo     string
	Detected bool
}

func ReadHookInput(r io.Reader, fallbackCWD string) HookInput {
	fallback := strings.TrimSpace(fallbackCWD)
	input := HookInput{
		CWD:           fallback,
		HookEventName: DefaultHookEventName,
	}
	if r == nil {
		return input
	}
	content, err := io.ReadAll(r)
	if err != nil || len(bytes.TrimSpace(content)) == 0 {
		return input
	}

	var raw struct {
		CWD           string `json:"cwd"`
		HookEventName string `json:"hook_event_name"`
	}
	if err := json.Unmarshal(content, &raw); err != nil {
		return input
	}
	if cwd := strings.TrimSpace(raw.CWD); cwd != "" {
		input.CWD = cwd
	}
	if event := strings.TrimSpace(raw.HookEventName); event != "" {
		input.HookEventName = event
	}
	return input
}

func Build(ctx context.Context, opts BuildOptions) (string, error) {
	cwd := strings.TrimSpace(opts.Input.CWD)
	if cwd == "" {
		return "", errors.New("cwd is required")
	}

	info := opts.Info
	if info.Version == "" {
		info = buildinfo.Current()
	}
	paths := opts.Paths
	if strings.TrimSpace(paths.Home) == "" {
		defaultPaths, err := config.DefaultPaths()
		if err != nil {
			return "", err
		}
		paths = defaultPaths
	}

	repo := detectRepo(ctx, cwd, opts.Runner)
	var b strings.Builder
	fmt.Fprintln(&b, "Gitmoot presence context")
	fmt.Fprintf(&b, "- Gitmoot: %s (commit %s, built %s, %s)\n", info.Version, info.Commit, info.Date, info.Go)
	fmt.Fprintf(&b, "- cwd: %s\n", quoteContextValue(cwd))
	fmt.Fprintf(&b, "- Gitmoot home: %s\n", quoteContextValue(paths.Home))
	if repo.Detected {
		fmt.Fprintf(&b, "- repo: %s (root: %s)\n", quoteContextValue(repo.Repo), quoteContextValue(repo.Root))
	} else if repo.Root != "" {
		fmt.Fprintf(&b, "- repo: not detected (git root: %s)\n", quoteContextValue(repo.Root))
	} else {
		fmt.Fprintln(&b, "- repo: not detected")
	}
	fmt.Fprintln(&b, "- dashboard command: `gitmoot dashboard`")
	if repo.Detected {
		fmt.Fprintln(&b)
		snapshotLoader := opts.SnapshotLoader
		if snapshotLoader == nil {
			snapshotLoader = presence.BuildSnapshot
		}
		snapshotCtx, cancel := context.WithTimeout(ctx, time.Second)
		snapshot, err := snapshotLoader(snapshotCtx, paths, repo.Repo)
		cancel()
		if err != nil {
			fmt.Fprintln(&b, presence.FormatUnavailable())
		} else {
			fmt.Fprintln(&b, presence.FormatSnapshot(snapshot))
		}
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "Guidance")
	fmt.Fprintln(&b, "- For Gitmoot health or status questions, answer from the current snapshot when it is sufficient; otherwise run relevant read-only Gitmoot CLI checks and answer directly.")
	fmt.Fprintln(&b, "- Mention `gitmoot dashboard` only as live monitoring follow-up after the direct answer.")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "Guardrails")
	fmt.Fprintln(&b, "- This hook provides read-only startup context only.")
	fmt.Fprintln(&b, "- Do not call GitHub, check `gh auth status`, start daemons, inspect remote PRs, poll jobs or locks automatically, create or subscribe agents, update templates, release locks, or mutate state unless the user explicitly asks.")
	return strings.TrimRight(b.String(), "\n"), nil
}

func OutputForContext(contextText string) HookOutput {
	contextText = strings.TrimSpace(contextText)
	if contextText == "" {
		return HookOutput{}
	}
	return HookOutput{
		HookSpecificOutput: &HookSpecificOutput{
			HookEventName:     DefaultHookEventName,
			AdditionalContext: contextText,
		},
	}
}

func WriteOutput(w io.Writer, contextText string) error {
	return json.NewEncoder(w).Encode(OutputForContext(contextText))
}

func quoteContextValue(value string) string {
	return strconv.Quote(strings.TrimSpace(value))
}

func detectRepo(ctx context.Context, cwd string, runner subprocess.Runner) repoInfo {
	client := gitutil.NewClient(cwd, runner)
	root, err := client.Root(ctx)
	if err != nil {
		return repoInfo{}
	}
	remote, err := client.OriginRemote(ctx)
	if err != nil {
		return repoInfo{Root: root}
	}
	repo, err := gitutil.ParseGitHubRemote(remote)
	if err != nil {
		return repoInfo{Root: root, Remote: remote}
	}
	return repoInfo{
		Root:     root,
		Remote:   remote,
		Repo:     repo.String(),
		Detected: true,
	}
}

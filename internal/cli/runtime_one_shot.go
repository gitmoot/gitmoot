package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/gitmoot/gitmoot/internal/execbackend"
	"github.com/gitmoot/gitmoot/internal/runtime"
)

// deliverOneShotRuntimePrompt starts a fresh throwaway runtime conversation and
// returns its answer. RuntimeRef must stay empty so no registered/live session
// can be resumed or mutated.
func deliverOneShotRuntimePrompt(ctx context.Context, agent runtime.Agent, prompt string) (string, error) {
	if strings.TrimSpace(agent.RuntimeRef) != "" {
		return "", fmt.Errorf("one-shot runtime delivery requires an empty runtime ref")
	}
	adapterDir := strings.TrimSpace(agent.WorkingDir)
	if adapterDir == "" {
		adapterDir = agent.RepoScope
	}
	backend, err := execBackendForRuntimeAgent(agent)
	if err != nil {
		return "", err
	}
	adapter, err := startRuntimeAdapterForBackend(backend, agent.ConfigHome, agent.Runtime, adapterDir)
	if err != nil {
		return "", err
	}
	started, err := adapter.Start(ctx, runtime.StartRequest{Agent: agent, Prompt: prompt})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(started.Raw), nil
}

func execBackendForRuntimeAgent(agent runtime.Agent) (execbackend.Backend, error) {
	if strings.TrimSpace(agent.ExecBackend) == "" {
		return execbackend.Local, nil
	}
	return execbackend.ParseImplemented(agent.ExecBackend)
}

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
	backend := execbackend.Local
	if strings.TrimSpace(agent.ExecBackend) != "" {
		var err error
		backend, err = execbackend.ParseImplemented(agent.ExecBackend)
		if err != nil {
			return "", err
		}
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

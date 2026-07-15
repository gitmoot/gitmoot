package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/pipeline"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

const pipelineEnvFileMode os.FileMode = 0o600

type pipelineStageEnvAccess struct {
	File     string
	Keys     []string
	Defaults map[string]string
}

// validatePipelineEnvironment is the add-time #968 preflight. This first
// increment resolves only pipeline-owned env_file and inline defaults. The
// future named-key registry/grants phase can add another source here without
// changing the names-only payload or delivery adapter.
func validatePipelineEnvironment(ctx context.Context, store *db.Store, home string, spec pipeline.Spec) error {
	sources, err := loadPipelineEnvSources(ctx, store, home, spec.EnvFile, spec.Env)
	if err != nil {
		return err
	}
	for _, stage := range spec.Stages {
		if len(stage.EnvKeys) == 0 {
			continue
		}
		if _, err := pipeline.ResolveEnvKeys(stage.EnvKeys, sources.available); err != nil {
			return fmt.Errorf("stage %q: %w", stage.ID, err)
		}
	}
	return nil
}

func resolvePipelineStageEnvAccess(ctx context.Context, store *db.Store, home string, spec pipeline.Spec, stage pipeline.Stage) (pipelineStageEnvAccess, error) {
	if len(stage.EnvKeys) == 0 {
		return pipelineStageEnvAccess{}, nil
	}
	sources, err := loadPipelineEnvSources(ctx, store, home, spec.EnvFile, spec.Env)
	if err != nil {
		return pipelineStageEnvAccess{}, err
	}
	keys, err := pipeline.ResolveEnvKeys(stage.EnvKeys, sources.available)
	if err != nil {
		return pipelineStageEnvAccess{}, fmt.Errorf("stage %q: %w", stage.ID, err)
	}
	defaults := make(map[string]string)
	for _, key := range keys {
		if value, ok := spec.Env[key]; ok {
			defaults[key] = value
		}
	}
	return pipelineStageEnvAccess{File: spec.EnvFile, Keys: keys, Defaults: defaults}, nil
}

type pipelineEnvSources struct {
	available map[string]string
}

func loadPipelineEnvSources(ctx context.Context, store *db.Store, home, envFile string, inline map[string]string) (pipelineEnvSources, error) {
	available := make(map[string]string, len(inline))
	for name, value := range inline {
		available[name] = value
	}
	if strings.TrimSpace(envFile) == "" {
		return pipelineEnvSources{available: available}, nil
	}
	fileValues, err := loadValidatedPipelineEnvFile(ctx, store, home, envFile)
	if err != nil {
		return pipelineEnvSources{}, err
	}
	// The pipeline-owned file is the most-specific source and wins over inline
	// non-secret defaults. There is no shared registry in this increment.
	for name, value := range fileValues {
		available[name] = value
	}
	return pipelineEnvSources{available: available}, nil
}

func loadValidatedPipelineEnvFile(ctx context.Context, store *db.Store, home, declared string) (map[string]string, error) {
	declared = strings.TrimSpace(declared)
	if !filepath.IsAbs(declared) {
		return nil, fmt.Errorf("env_file %q must be absolute", declared)
	}
	file, err := os.Open(declared)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("env_file %s does not exist", declared)
		}
		return nil, fmt.Errorf("open env_file %s: %w", declared, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat env_file %s: %w", declared, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("env_file %s is not a regular file", declared)
	}
	if info.Mode().Perm() != pipelineEnvFileMode {
		return nil, fmt.Errorf("env_file %s has mode %04o; want 0600", declared, info.Mode().Perm())
	}
	owner, err := pipelineEnvOwnerUID(info)
	if err != nil {
		return nil, fmt.Errorf("env_file %s: %w", declared, err)
	}
	if owner != pipelineEnvCurrentUID() {
		return nil, fmt.Errorf("env_file %s is owned by uid %d; want operator uid %d", declared, owner, pipelineEnvCurrentUID())
	}
	if err := validatePipelineEnvFileLocation(ctx, store, home, declared); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("read env_file %s: %w", declared, err)
	}
	values, err := pipeline.ParseEnv(declared, data)
	if err != nil {
		return nil, err
	}
	for name := range values {
		if pipeline.ReservedEnvName(name) {
			return nil, fmt.Errorf("env_file %s key %q uses reserved GITMOOT_* namespace", declared, name)
		}
	}
	return values, nil
}

func validatePipelineEnvFileLocation(ctx context.Context, store *db.Store, home, declared string) error {
	if store == nil {
		return errors.New("pipeline env_file validation requires a store")
	}
	resolved, err := resolveProduceSafetyPath(declared)
	if err != nil {
		return fmt.Errorf("resolve env_file %s: %w", declared, err)
	}
	gitmootHome := ""
	if databasePath := strings.TrimSpace(store.DatabasePath()); databasePath != "" && databasePath != ":memory:" {
		gitmootHome = filepath.Dir(databasePath)
	} else {
		paths, err := pathsFromFlag(home)
		if err != nil {
			return err
		}
		gitmootHome = paths.Home
	}
	protected := []struct{ label, path string }{{"Gitmoot home", gitmootHome}}
	repos, err := store.ListRepos(ctx)
	if err != nil {
		return err
	}
	for _, repo := range repos {
		label := "managed checkout " + repo.Owner + "/" + repo.Name
		for _, checkout := range []string{repo.CheckoutPath, repo.PrimaryCheckoutPath} {
			if strings.TrimSpace(checkout) != "" {
				protected = append(protected, struct{ label, path string }{label, checkout})
			}
		}
	}
	for _, item := range protected {
		protectedPath, err := resolveProduceSafetyPath(item.path)
		if err != nil {
			return fmt.Errorf("resolve %s %q: %w", item.label, item.path, err)
		}
		if pathWithin(resolved, protectedPath) {
			return fmt.Errorf("env_file %s resolves inside %s %q", declared, item.label, protectedPath)
		}
	}
	return nil
}

func pathWithin(path, directory string) bool {
	rel, err := filepath.Rel(directory, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

type pipelineEnvDeliveryAdapter struct {
	inner workflow.DeliveryAdapter
	store *db.Store
	home  string
	file  string
	keys  []string
	env   map[string]string
}

func wrapPipelineEnvDeliveryAdapter(store *db.Store, home string, payload workflow.JobPayload, inner workflow.DeliveryAdapter) workflow.DeliveryAdapter {
	if inner == nil || len(payload.PipelineEnvKeys) == 0 {
		return inner
	}
	return pipelineEnvDeliveryAdapter{
		inner: inner, store: store, home: home, file: payload.PipelineEnvFile,
		keys: append([]string(nil), payload.PipelineEnvKeys...), env: payload.PipelineEnv,
	}
}

func (a pipelineEnvDeliveryAdapter) Deliver(ctx context.Context, agent runtime.Agent, job runtime.Job) (runtime.Result, error) {
	sources, err := loadPipelineEnvSources(ctx, a.store, a.home, a.file, a.env)
	if err != nil {
		return runtime.Result{}, fmt.Errorf("load pipeline stage environment: %w", err)
	}
	entries := make([]string, 0, len(a.keys))
	for _, key := range a.keys {
		value, ok := sources.available[key]
		if !ok {
			return runtime.Result{}, fmt.Errorf("load pipeline stage environment: key %q is no longer available", key)
		}
		entries = append(entries, key+"="+value)
	}
	job.ShellEnv = prependPipelineEnvironment(entries, job.ShellEnv)
	return a.inner.Deliver(ctx, agent, job)
}

// prependPipelineEnvironment puts selected values before the existing shell
// environment. exec's last-wins behavior therefore preserves Gitmoot's own
// GITMOOT_* metadata even for a defense-in-depth, validation-bypassing payload.
func prependPipelineEnvironment(selected, internal []string) []string {
	merged := make([]string, 0, len(selected)+len(internal))
	merged = append(merged, selected...)
	return append(merged, internal...)
}

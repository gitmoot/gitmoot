package pipeline

import (
	"context"
	"errors"
	"fmt"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"os"
	"path/filepath"
	"strings"
)

// ValidatePipelineTriggerCycle overlays candidate on the stored trigger graph
// and walks downstream->upstream edges from the candidate. Missing upstreams are
// leaves (the add path warns separately); only a closed chain is rejected.
func ValidatePipelineTriggerCycle(records []db.Pipeline, candidate Spec) error {
	edges := make(map[string]string, len(records)+1)
	for _, rec := range records {
		spec, err := Load([]byte(rec.SpecYAML))
		if err != nil || spec.Trigger == nil || spec.Trigger.Kind != "pipeline" {
			continue
		}
		edges[spec.Name] = spec.Trigger.Pipeline
	}
	delete(edges, candidate.Name)
	if candidate.Trigger != nil && candidate.Trigger.Kind == "pipeline" {
		edges[candidate.Name] = candidate.Trigger.Pipeline
	} else {
		return nil
	}

	const (
		unvisited = iota
		visiting
		done
	)
	state := make(map[string]int, len(edges))
	stack := make([]string, 0, len(edges)+1)
	var visit func(string) []string
	visit = func(name string) []string {
		state[name] = visiting
		stack = append(stack, name)
		upstream := edges[name]
		if upstream != "" {
			switch state[upstream] {
			case visiting:
				start := 0
				for i, item := range stack {
					if item == upstream {
						start = i
						break
					}
				}
				cycle := append([]string(nil), stack[start:]...)
				return append(cycle, upstream)
			case unvisited:
				if cycle := visit(upstream); cycle != nil {
					return cycle
				}
			}
		}
		stack = stack[:len(stack)-1]
		state[name] = done
		return nil
	}
	if cycle := visit(candidate.Name); cycle != nil {
		return fmt.Errorf("pipeline trigger cycle: %s", strings.Join(cycle, " -> "))
	}
	return nil
}

// CanonicalizePipelineProducePaths is the single filesystem safety check used
// both at pipeline-add time and immediately before a produce delivery. It returns
// resolved canonical targets so the runtime grant cannot follow a symlink that
// changed after validation.
func CanonicalizePipelineProducePaths(ctx context.Context, store *db.Store, homeFlag, subject string, writes []string) ([]string, error) {
	if store == nil {
		return nil, errors.New("produce path validation requires a store")
	}
	paths, err := pathsFromFlag(homeFlag)
	if err != nil {
		return nil, err
	}
	protected := []struct {
		label string
		path  string
	}{{label: "gitmoot home", path: paths.Home}}
	repos, err := store.ListRepos(ctx)
	if err != nil {
		return nil, err
	}
	for _, repo := range repos {
		if strings.TrimSpace(repo.CheckoutPath) != "" {
			protected = append(protected, struct {
				label string
				path  string
			}{label: "managed checkout " + repo.Owner + "/" + repo.Name, path: repo.CheckoutPath})
		}
	}
	resolvedWrites := make([]string, 0, len(writes))
	for _, declared := range writes {
		resolved, err := ResolveProduceSafetyPath(declared)
		if err != nil {
			return nil, fmt.Errorf("%s writes path %q: %w", subject, declared, err)
		}
		if resolved == string(filepath.Separator) {
			return nil, fmt.Errorf("%s writes path %q resolves to filesystem root, which is not allowed", subject, declared)
		}
		for _, item := range protected {
			protectedPath, err := ResolveProduceSafetyPath(item.path)
			if err != nil {
				return nil, fmt.Errorf("resolve %s %q: %w", item.label, item.path, err)
			}
			if pathsOverlap(resolved, protectedPath) {
				return nil, fmt.Errorf("%s writes path %q resolves to %q and overlaps %s %q", subject, declared, resolved, item.label, protectedPath)
			}
		}
		resolvedWrites = append(resolvedWrites, resolved)
	}
	return resolvedWrites, nil
}

// CanonicalizePipelineProduceReadPaths is the read-only counterpart to the
// write checker. It resolves symlinks at both add and delivery time, prevents a
// broad read grant from exposing Gitmoot state or declared credential files,
// and rejects a read root that would contain an already-readable write root.
func CanonicalizePipelineProduceReadPaths(ctx context.Context, store *db.Store, homeFlag, subject string, reads, resolvedWrites []string, envFile string) ([]string, error) {
	if len(reads) == 0 {
		return nil, nil
	}
	if store == nil {
		return nil, errors.New("produce read path validation requires a store")
	}
	protected, err := ResolveProduceReadProtectedPaths(ctx, store, homeFlag, envFile)
	if err != nil {
		return nil, err
	}

	resolvedReads := make([]string, 0, len(reads))
	for _, declared := range reads {
		resolved, err := ResolveProduceSafetyPath(declared)
		if err != nil {
			return nil, fmt.Errorf("%s reads path %q: %w", subject, declared, err)
		}
		if resolved == string(filepath.Separator) {
			return nil, fmt.Errorf("%s reads path resolves to filesystem root, which is not allowed", subject)
		}
		if label, excluded := protected.Exclusion(resolved); excluded {
			return nil, fmt.Errorf("%s reads path overlaps %s", subject, label)
		}
		for _, writePath := range resolvedWrites {
			if PathWithin(writePath, resolved) {
				return nil, fmt.Errorf("%s reads path equals or contains a declared writes path", subject)
			}
		}
		resolvedReads = append(resolvedReads, resolved)
	}
	return resolvedReads, nil
}

type ProduceReadProtectedPaths struct {
	gitmootHome string
	keychain    string
	envFile     string
}

func ResolveProduceReadProtectedPaths(ctx context.Context, store *db.Store, homeFlag, envFile string) (ProduceReadProtectedPaths, error) {
	paths, err := pathsFromFlag(homeFlag)
	if err != nil {
		return ProduceReadProtectedPaths{}, err
	}
	gitmootHome, err := ResolveProduceSafetyPath(paths.Home)
	if err != nil {
		return ProduceReadProtectedPaths{}, fmt.Errorf("resolve gitmoot home: %w", err)
	}
	keychainPath, err := ResolveKeychainPath(store, homeFlag)
	if err != nil {
		return ProduceReadProtectedPaths{}, err
	}
	keychainPath, err = ResolveProduceSafetyPath(keychainPath)
	if err != nil {
		return ProduceReadProtectedPaths{}, fmt.Errorf("resolve configured keychain_path: %w", err)
	}
	resolvedEnvFile := ""
	if strings.TrimSpace(envFile) != "" {
		resolvedEnvFile, err = ResolveProduceSafetyPath(envFile)
		if err != nil {
			return ProduceReadProtectedPaths{}, fmt.Errorf("resolve pipeline env_file: %w", err)
		}
	}
	return ProduceReadProtectedPaths{gitmootHome: gitmootHome, keychain: keychainPath, envFile: resolvedEnvFile}, nil
}

func (p ProduceReadProtectedPaths) Exclusion(path string) (string, bool) {
	switch {
	case pathsOverlap(path, p.gitmootHome):
		return "the Gitmoot home", true
	case pathsOverlap(path, p.keychain):
		return "the configured keychain_path", true
	case p.envFile != "" && pathsOverlap(path, p.envFile):
		return "the pipeline env_file", true
	default:
		return "", false
	}
}

func ResolveProduceSafetyPath(path string) (string, error) {
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("path must be absolute")
	}
	probe := path
	var suffix []string
	for {
		resolved, err := filepath.EvalSymlinks(probe)
		if err == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return "", err
		}
		suffix = append(suffix, filepath.Base(probe))
		probe = parent
	}
}

func pathsOverlap(left, right string) bool {
	contains := func(parent, child string) bool {
		rel, err := filepath.Rel(parent, child)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	return contains(left, right) || contains(right, left)
}

// PipelineRunnerAgent builds the hidden shell agent that owns a pipeline's stage
// jobs. The stage command travels per-job (via the stage job's runtime-override
// ref), NOT on this agent's runtime_ref, so one runner serves every stage. It is
// least-privilege read-only (the shell adapter runs the command verbatim; the
// policy is nominal for shell) and holds only the "ask" capability, matching the
// stage jobs' Action.
func PipelineRunnerAgent(name, repo string) db.Agent {
	return db.Agent{
		Name:           name,
		Role:           "pipeline-runner",
		Runtime:        runtime.ShellRuntime,
		RepoScope:      repo,
		Capabilities:   []string{"ask"},
		AutonomyPolicy: runtime.AutonomyPolicyReadOnly,
		HealthStatus:   "unknown",
	}
}

package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/credgw"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// Test-only seam for proxied shell-stage httptest upstreams. Production
// keycard proxy delivery remains HTTPS-only.
var PipelineProxyAllowLoopbackHTTP bool

const pipelineEnvFileMode os.FileMode = 0o600

const (
	pipelineKeySourceOwn     = "own"
	pipelineKeySourceShared  = "shared"
	pipelineKeySourceDefault = "default"

	PipelineKeySourceOwn     = pipelineKeySourceOwn
	PipelineKeySourceShared  = pipelineKeySourceShared
	PipelineKeySourceDefault = pipelineKeySourceDefault
)

const (
	pipelineEnvFileStatusNone        = "none"
	pipelineEnvFileStatusOK          = "ok"
	pipelineEnvFileStatusMissing     = "missing"
	pipelineEnvFileStatusBadMode     = "bad_mode"
	pipelineEnvFileStatusBadOwner    = "bad_owner"
	pipelineEnvFileStatusBadLocation = "bad_location"
	pipelineEnvFileStatusInvalid     = "invalid"

	PipelineEnvFileStatusNone        = pipelineEnvFileStatusNone
	PipelineEnvFileStatusOK          = pipelineEnvFileStatusOK
	PipelineEnvFileStatusMissing     = pipelineEnvFileStatusMissing
	PipelineEnvFileStatusBadMode     = pipelineEnvFileStatusBadMode
	PipelineEnvFileStatusBadOwner    = pipelineEnvFileStatusBadOwner
	PipelineEnvFileStatusBadLocation = pipelineEnvFileStatusBadLocation
	PipelineEnvFileStatusInvalid     = pipelineEnvFileStatusInvalid
)

type PipelineStageEnvAccess struct {
	File     string
	Keys     []string
	Defaults map[string]string
	Access   []workflow.PipelineKeyAccess
}

type PipelineEnvUnresolved struct {
	Stage    string
	Selector string
}

type PipelineEnvironmentResolution struct {
	Access     []workflow.PipelineKeyAccess
	Unresolved []PipelineEnvUnresolved
}

type pipelineSharedKeys struct {
	pipeline map[string]db.KeychainKey
	agents   map[string]map[string]db.KeychainKey
}

type PipelineEnvFileInspection struct {
	Path   string
	Status string
	Names  map[string]struct{}
}

func InspectPipelineEnvironment(ctx context.Context, store *db.Store, home string, spec Spec) (PipelineEnvFileInspection, PipelineEnvironmentResolution, error) {
	inspection := ClassifyPipelineEnvFile(ctx, store, home, spec.EnvFile)
	sharedCandidates, err := pipelineSharedKeyCandidates(ctx, store, spec, inspection.Names)
	if err != nil {
		return inspection, PipelineEnvironmentResolution{}, err
	}
	availableShared := pipelineSharedKeys{pipeline: map[string]db.KeychainKey{}, agents: map[string]map[string]db.KeychainKey{}}
	if sharedCandidates.any() {
		if names, loadErr := loadValidatedKeychainNames(ctx, store, home); loadErr == nil {
			availableShared = sharedCandidates.available(names)
		}
	}
	return inspection, projectPipelineEnvironment(spec, inspection.Names, availableShared), nil
}

// ResolvePipelineEnvironment is the single names-only projection used by add
// validation, run preflight, payload audit, and `pipeline show --json`.
func ResolvePipelineEnvironment(ctx context.Context, store *db.Store, home string, spec Spec) (PipelineEnvironmentResolution, error) {
	ownNames := make(map[string]struct{})
	if strings.TrimSpace(spec.EnvFile) != "" {
		own, err := loadValidatedSecretEnvNames(ctx, store, home, "env_file", spec.EnvFile)
		if err != nil {
			return PipelineEnvironmentResolution{}, err
		}
		for name := range own {
			ownNames[name] = struct{}{}
		}
	}

	sharedCandidates, err := pipelineSharedKeyCandidates(ctx, store, spec, ownNames)
	if err != nil {
		return PipelineEnvironmentResolution{}, err
	}
	availableShared := pipelineSharedKeys{pipeline: map[string]db.KeychainKey{}, agents: map[string]map[string]db.KeychainKey{}}
	if sharedCandidates.any() {
		shared, err := loadValidatedKeychainNames(ctx, store, home)
		if err != nil {
			return PipelineEnvironmentResolution{}, err
		}
		availableShared = sharedCandidates.available(shared)
	}
	return projectPipelineEnvironment(spec, ownNames, availableShared), nil
}

func projectPipelineEnvironment(spec Spec, ownNames map[string]struct{}, sharedKeys pipelineSharedKeys) PipelineEnvironmentResolution {
	defaultNames := make(map[string]struct{}, len(spec.Env))
	for name := range spec.Env {
		defaultNames[name] = struct{}{}
	}
	sharedInjected := make(map[string]struct{})
	sharedProxied := make(map[string]struct{})
	for name, key := range sharedKeys.pipeline {
		switch key.Mode {
		case db.KeychainModeInjected:
			sharedInjected[name] = struct{}{}
		case db.KeychainModeProxied:
			sharedProxied[name] = struct{}{}
		}
	}
	shellSources := []EnvKeySource{
		{Source: pipelineKeySourceOwn, Mode: db.KeychainModeInjected, Names: ownNames},
		{Source: pipelineKeySourceShared, Mode: db.KeychainModeInjected, Names: sharedInjected},
		{Source: pipelineKeySourceShared, Mode: db.KeychainModeProxied, Names: sharedProxied},
		{Source: pipelineKeySourceDefault, Mode: db.KeychainModeInjected, Names: defaultNames},
	}
	resolution := PipelineEnvironmentResolution{}
	for _, stage := range spec.Stages {
		sources := shellSources
		if stage.Kind() != StageKindShell {
			agentProxied := make(map[string]struct{})
			for name, key := range sharedKeys.agents[stage.Agent] {
				if key.Mode == db.KeychainModeProxied && key.ProxyConfigured() {
					agentProxied[name] = struct{}{}
				}
			}
			sources = []EnvKeySource{{Source: pipelineKeySourceShared, Mode: db.KeychainModeProxied, Names: agentProxied}}
		}
		projected, unresolved := ProjectEnvKeys(stage.EnvKeys, sources)
		for _, key := range projected {
			resolution.Access = append(resolution.Access, workflow.PipelineKeyAccess{
				Stage: stage.ID, Name: key.Name, Source: key.Source, Mode: key.Mode,
			})
		}
		for _, selector := range unresolved {
			resolution.Unresolved = append(resolution.Unresolved, PipelineEnvUnresolved{Stage: stage.ID, Selector: selector})
		}
	}
	return resolution
}

func pipelineSharedKeyCandidates(ctx context.Context, store *db.Store, spec Spec, ownNames map[string]struct{}) (pipelineSharedKeys, error) {
	candidates := pipelineSharedKeys{pipeline: make(map[string]db.KeychainKey), agents: make(map[string]map[string]db.KeychainKey)}
	if strings.TrimSpace(spec.Name) == "" {
		return candidates, nil
	}
	grants, err := store.ListKeychainGrantsForConsumer(ctx, db.KeychainConsumerPipeline, spec.Name)
	if err != nil {
		return pipelineSharedKeys{}, err
	}
	for _, grant := range grants {
		if _, owned := ownNames[grant.KeyName]; owned {
			continue
		}
		if !pipelineShellSelectorUsed(spec, grant.KeyName) {
			continue
		}
		key, found, err := store.GetGrantedKey(ctx, db.KeychainConsumerPipeline, spec.Name, grant.KeyName)
		if err != nil {
			return pipelineSharedKeys{}, err
		}
		if !found {
			continue
		}
		if key.Mode == db.KeychainModeProxied && !key.ProxyConfigured() {
			return pipelineSharedKeys{}, fmt.Errorf("key %s uses proxied mode but is not configured; run %s", key.Name, keyConfigureCommand(key.Name))
		}
		if key.Mode == db.KeychainModeInjected || key.Mode == db.KeychainModeProxied {
			candidates.pipeline[key.Name] = key
		}
	}
	seenAgents := make(map[string]struct{})
	for _, stage := range spec.Stages {
		seat := strings.TrimSpace(stage.Agent)
		if seat == "" || len(stage.EnvKeys) == 0 {
			continue
		}
		if _, seen := seenAgents[seat]; seen {
			continue
		}
		seenAgents[seat] = struct{}{}
		grants, err := store.ListKeychainGrantsForConsumer(ctx, db.KeychainConsumerAgent, seat)
		if err != nil {
			return pipelineSharedKeys{}, err
		}
		for _, grant := range grants {
			if !pipelineAgentSelectorUsed(spec, seat, grant.KeyName) {
				continue
			}
			key, found, err := store.GetGrantedKey(ctx, db.KeychainConsumerAgent, seat, grant.KeyName)
			if err != nil {
				return pipelineSharedKeys{}, err
			}
			if !found || key.Mode != db.KeychainModeProxied {
				continue
			}
			if !key.ProxyConfigured() {
				return pipelineSharedKeys{}, fmt.Errorf("key %s uses proxied mode but is not configured; run %s", key.Name, keyConfigureCommand(key.Name))
			}
			if candidates.agents[seat] == nil {
				candidates.agents[seat] = make(map[string]db.KeychainKey)
			}
			candidates.agents[seat][key.Name] = key
		}
	}
	return candidates, nil
}

func pipelineShellSelectorUsed(spec Spec, name string) bool {
	for _, stage := range spec.Stages {
		if stage.Kind() != StageKindShell {
			continue
		}
		if pipelineStageSelectorUsed(stage, name) {
			return true
		}
	}
	return false
}

func pipelineAgentSelectorUsed(spec Spec, seat, name string) bool {
	for _, stage := range spec.Stages {
		if strings.TrimSpace(stage.Agent) != seat {
			continue
		}
		if pipelineStageSelectorUsed(stage, name) {
			return true
		}
	}
	return false
}

func pipelineStageSelectorUsed(stage Stage, name string) bool {
	for _, selector := range stage.EnvKeys {
		if selector == name {
			return true
		}
		if strings.ContainsAny(selector, "*?[") {
			matched, _ := path.Match(selector, name)
			if matched {
				return true
			}
		}
	}
	return false
}

func (k pipelineSharedKeys) any() bool {
	if len(k.pipeline) > 0 {
		return true
	}
	for _, keys := range k.agents {
		if len(keys) > 0 {
			return true
		}
	}
	return false
}

func (k pipelineSharedKeys) available(names map[string]struct{}) pipelineSharedKeys {
	out := pipelineSharedKeys{pipeline: make(map[string]db.KeychainKey), agents: make(map[string]map[string]db.KeychainKey)}
	for name, key := range k.pipeline {
		if _, present := names[name]; present {
			out.pipeline[name] = key
		}
	}
	for seat, keys := range k.agents {
		for name, key := range keys {
			if _, present := names[name]; !present {
				continue
			}
			if out.agents[seat] == nil {
				out.agents[seat] = make(map[string]db.KeychainKey)
			}
			out.agents[seat][name] = key
		}
	}
	return out
}

func PipelineEnvironmentResolutionError(spec Spec, unresolved []PipelineEnvUnresolved) error {
	if len(unresolved) == 0 {
		return nil
	}
	item := unresolved[0]
	name := item.Selector
	if strings.ContainsAny(name, "*?[") {
		name = "<name>"
	}
	for _, stage := range spec.Stages {
		if stage.ID == item.Stage && strings.TrimSpace(stage.Agent) != "" {
			return fmt.Errorf("stage %q env_keys entry %q is unresolved; register a configured proxied key and run gitmoot key grant %s --agent %s", item.Stage, item.Selector, name, stage.Agent)
		}
	}
	return fmt.Errorf("stage %q env_keys entry %q is unresolved; register a matching key and run gitmoot key grant %s --pipeline %s", item.Stage, item.Selector, name, spec.Name)
}

func ResolvePipelineStageEnvAccess(ctx context.Context, store *db.Store, home string, spec Spec, stage Stage) (PipelineStageEnvAccess, error) {
	if len(stage.EnvKeys) == 0 {
		return PipelineStageEnvAccess{}, nil
	}
	resolutionSpec := spec
	foundStage := false
	for _, candidate := range resolutionSpec.Stages {
		if candidate.ID == stage.ID {
			foundStage = true
			break
		}
	}
	if !foundStage {
		resolutionSpec.Stages = append(append([]Stage(nil), resolutionSpec.Stages...), stage)
	}
	resolution, err := ResolvePipelineEnvironment(ctx, store, home, resolutionSpec)
	if err != nil {
		return PipelineStageEnvAccess{}, err
	}
	for _, unresolved := range resolution.Unresolved {
		if unresolved.Stage == stage.ID {
			return PipelineStageEnvAccess{}, PipelineEnvironmentResolutionError(spec, []PipelineEnvUnresolved{unresolved})
		}
	}
	access := PipelineStageEnvAccess{File: spec.EnvFile}
	for _, row := range resolution.Access {
		if row.Stage != stage.ID {
			continue
		}
		access.Access = append(access.Access, row)
		access.Keys = append(access.Keys, row.Name)
		if row.Source == pipelineKeySourceDefault {
			if access.Defaults == nil {
				access.Defaults = make(map[string]string)
			}
			access.Defaults[row.Name] = spec.Env[row.Name]
		}
	}
	return access, nil
}

func loadValidatedPipelineEnvFile(ctx context.Context, store *db.Store, home, declared string) (map[string]string, error) {
	return loadValidatedSecretEnvFile(ctx, store, home, "env_file", declared)
}

func LoadValidatedKeychainFile(ctx context.Context, store *db.Store, home string) (string, map[string]string, error) {
	path, err := ResolveKeychainPath(store, home)
	if err != nil {
		return "", nil, err
	}
	values, err := loadValidatedSecretEnvFile(ctx, store, home, "keychain", path)
	return path, values, err
}

func loadValidatedKeychainNames(ctx context.Context, store *db.Store, home string) (map[string]struct{}, error) {
	path, err := ResolveKeychainPath(store, home)
	if err != nil {
		return nil, err
	}
	return loadValidatedSecretEnvNames(ctx, store, home, "keychain", path)
}

func ResolveKeychainPath(store *db.Store, home string) (string, error) {
	paths, err := configPathsForPipelineStore(store, home)
	if err != nil {
		return "", err
	}
	cfg, err := config.LoadCredentialsConfig(paths)
	if err != nil {
		return "", fmt.Errorf("load credentials config: %w", err)
	}
	if cfg.KeychainPath != "" {
		return cfg.KeychainPath, nil
	}
	baseHome := filepath.Dir(paths.Home)
	return filepath.Join(baseHome, ".config", "gitmoot", "keychain.env"), nil
}

func configPathsForPipelineStore(store *db.Store, home string) (config.Paths, error) {
	if strings.TrimSpace(home) != "" {
		return pathsFromFlag(home)
	}
	if store != nil {
		databasePath := strings.TrimSpace(store.DatabasePath())
		if databasePath != "" && databasePath != ":memory:" {
			gitmootHome := filepath.Dir(databasePath)
			return config.PathsForHome(filepath.Dir(gitmootHome)), nil
		}
	}
	return pathsFromFlag(home)
}

// ConfigPathsForPipelineStore exposes the engine's store-aware path resolution.
var ConfigPathsForPipelineStore = configPathsForPipelineStore

func loadValidatedSecretEnvFile(ctx context.Context, store *db.Store, home, label, declared string) (map[string]string, error) {
	file, _, err := openValidatedSecretEnvFile(ctx, store, home, label, declared)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("read %s %s: %w", label, strings.TrimSpace(declared), err)
	}
	values, err := ParseEnv(strings.TrimSpace(declared), data)
	if err != nil {
		return nil, err
	}
	for name := range values {
		if ReservedEnvName(name) {
			return nil, fmt.Errorf("%s %s key %q uses reserved GITMOOT_* namespace", label, strings.TrimSpace(declared), name)
		}
	}
	return values, nil
}

func loadValidatedSecretEnvNames(ctx context.Context, store *db.Store, home, label, declared string) (map[string]struct{}, error) {
	file, _, err := openValidatedSecretEnvFile(ctx, store, home, label, declared)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("read %s %s: %w", label, strings.TrimSpace(declared), err)
	}
	names, err := ParseEnvNames(strings.TrimSpace(declared), data)
	if err != nil {
		return nil, err
	}
	for name := range names {
		if ReservedEnvName(name) {
			return nil, fmt.Errorf("%s %s key %q uses reserved GITMOOT_* namespace", label, strings.TrimSpace(declared), name)
		}
	}
	return names, nil
}

// ClassifyPipelineEnvFile returns the live advisory status and names needed by
// the dashboard. It never returns file contents or an error string.
func ClassifyPipelineEnvFile(ctx context.Context, store *db.Store, home, declared string) PipelineEnvFileInspection {
	declared = strings.TrimSpace(declared)
	inspection := PipelineEnvFileInspection{Path: declared, Status: pipelineEnvFileStatusNone, Names: map[string]struct{}{}}
	if declared == "" {
		return inspection
	}
	file, status, err := openValidatedSecretEnvFile(ctx, store, home, "env_file", declared)
	if err != nil {
		inspection.Status = status
		return inspection
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		inspection.Status = pipelineEnvFileStatusInvalid
		return inspection
	}
	names, err := ParseEnvNames(declared, data)
	if err != nil {
		inspection.Status = pipelineEnvFileStatusInvalid
		return inspection
	}
	for name := range names {
		if ReservedEnvName(name) {
			inspection.Status = pipelineEnvFileStatusInvalid
			return inspection
		}
	}
	inspection.Status = pipelineEnvFileStatusOK
	inspection.Names = names
	return inspection
}

// openValidatedSecretEnvFile is the shared safety gate for value-loading
// delivery and value-free status inspection. Its check order is fixed so unsafe
// files are never read or parsed after an earlier failure.
func openValidatedSecretEnvFile(ctx context.Context, store *db.Store, home, label, declared string) (*os.File, string, error) {
	declared = strings.TrimSpace(declared)
	if !filepath.IsAbs(declared) {
		return nil, pipelineEnvFileStatusInvalid, fmt.Errorf("%s %q must be absolute", label, declared)
	}
	file, err := os.Open(declared)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, pipelineEnvFileStatusMissing, fmt.Errorf("%s %s does not exist", label, declared)
		}
		return nil, pipelineEnvFileStatusInvalid, fmt.Errorf("open %s %s: %w", label, declared, err)
	}
	closeWith := func(status string, err error) (*os.File, string, error) {
		file.Close()
		return nil, status, err
	}
	info, err := file.Stat()
	if err != nil {
		return closeWith(pipelineEnvFileStatusInvalid, fmt.Errorf("stat %s %s: %w", label, declared, err))
	}
	if !info.Mode().IsRegular() {
		return closeWith(pipelineEnvFileStatusInvalid, fmt.Errorf("%s %s is not a regular file", label, declared))
	}
	if info.Mode().Perm() != pipelineEnvFileMode {
		return closeWith(pipelineEnvFileStatusBadMode, fmt.Errorf("%s %s has mode %04o; want 0600", label, declared, info.Mode().Perm()))
	}
	owner, err := pipelineEnvOwnerUID(info)
	if err != nil {
		return closeWith(pipelineEnvFileStatusInvalid, fmt.Errorf("%s %s: %w", label, declared, err))
	}
	if owner != PipelineEnvCurrentUID() {
		return closeWith(pipelineEnvFileStatusBadOwner, fmt.Errorf("%s %s is owned by uid %d; want operator uid %d", label, declared, owner, PipelineEnvCurrentUID()))
	}
	if err := validateSecretEnvFileLocation(ctx, store, home, label, declared); err != nil {
		return closeWith(pipelineEnvFileStatusBadLocation, err)
	}
	return file, pipelineEnvFileStatusOK, nil
}

func validateSecretEnvFileLocation(ctx context.Context, store *db.Store, home, label, declared string) error {
	if store == nil {
		return fmt.Errorf("%s validation requires a store", label)
	}
	resolved, err := ResolveProduceSafetyPath(declared)
	if err != nil {
		return fmt.Errorf("resolve %s %s: %w", label, declared, err)
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
		protectedPath, err := ResolveProduceSafetyPath(item.path)
		if err != nil {
			return fmt.Errorf("resolve %s %q: %w", item.label, item.path, err)
		}
		if PathWithin(resolved, protectedPath) {
			return fmt.Errorf("%s %s resolves inside %s %q", label, declared, item.label, protectedPath)
		}
	}
	return nil
}

func PathWithin(path, directory string) bool {
	rel, err := filepath.Rel(directory, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

type pipelineEnvDeliveryAdapter struct {
	inner        workflow.DeliveryAdapter
	store        *db.Store
	home         string
	pipelineName string
	consumerKind string
	consumerID   string
	access       []workflow.PipelineKeyAccess
	file         string
	keys         []string
	env          map[string]string
}

func WrapPipelineEnvDeliveryAdapter(store *db.Store, home string, payload workflow.JobPayload, inner workflow.DeliveryAdapter) workflow.DeliveryAdapter {
	if inner == nil || (len(payload.PipelineKeyAccess) == 0 && len(payload.PipelineEnvKeys) == 0) {
		return inner
	}
	consumerKind, consumerID := db.KeychainConsumerPipeline, payload.PipelineName
	if seat := strings.TrimSpace(payload.PipelineKeyAgent); seat != "" {
		consumerKind, consumerID = db.KeychainConsumerAgent, seat
	}
	return pipelineEnvDeliveryAdapter{
		inner: inner, store: store, home: home, pipelineName: payload.PipelineName,
		consumerKind: consumerKind, consumerID: strings.TrimSpace(consumerID),
		access: append([]workflow.PipelineKeyAccess(nil), payload.PipelineKeyAccess...),
		file:   payload.PipelineEnvFile, keys: append([]string(nil), payload.PipelineEnvKeys...), env: payload.PipelineEnv,
	}
}

func (a pipelineEnvDeliveryAdapter) Deliver(ctx context.Context, agent runtime.Agent, job runtime.Job) (runtime.Result, error) {
	if len(a.access) == 0 {
		return a.deliverLegacy(ctx, agent, job)
	}
	var own, shared map[string]string
	entries := make([]string, 0, len(a.access))
	var leases []*credgw.Lease
	defer func() {
		for i := len(leases) - 1; i >= 0; i-- {
			leases[i].Revoke()
		}
	}()
	var proxyGateway *credgw.Gateway
	for _, access := range a.access {
		if access.Mode == db.KeychainModeProxied {
			if access.Source != pipelineKeySourceShared {
				return runtime.Result{}, fmt.Errorf("load pipeline stage environment: proxied key %q must use shared source", access.Name)
			}
			if strings.TrimSpace(job.ID) == "" {
				return runtime.Result{}, fmt.Errorf("load pipeline stage environment: proxied key %q requires a job id", access.Name)
			}
			key, granted, err := a.store.GetGrantedKey(ctx, a.consumerKind, a.consumerID, access.Name)
			if err != nil {
				return runtime.Result{}, fmt.Errorf("load pipeline stage environment: re-check proxied grant for key %q: %w", access.Name, err)
			}
			if !granted || key.Mode != db.KeychainModeProxied || !key.ProxyConfigured() {
				return runtime.Result{}, fmt.Errorf("load pipeline stage environment: proxied grant for key %q was revoked, changed, or is unconfigured", access.Name)
			}
			policy, _, err := credgw.ValidateProxyPolicy(credgw.ProxyPolicy{
				Upstream: key.ProxyUpstream, AuthKind: credgw.ProxyAuthKind(key.ProxyAuthKind), Header: key.ProxyHeader,
				AllowLoopbackHTTP: PipelineProxyAllowLoopbackHTTP,
			})
			if err != nil {
				return runtime.Result{}, fmt.Errorf("load pipeline stage environment: invalid proxy configuration for key %q: %w", access.Name, err)
			}
			if proxyGateway == nil {
				paths, err := configPathsForPipelineStore(a.store, a.home)
				if err != nil {
					return runtime.Result{}, fmt.Errorf("load pipeline stage environment: resolve credential gateway home: %w", err)
				}
				proxyGateway, err = credgw.DefaultRegistry.Gateway(paths.Home, credgw.DefaultLogf)
				if err != nil {
					return runtime.Result{}, fmt.Errorf("load pipeline stage environment: start credential gateway: %w", err)
				}
			}
			lease, err := proxyGateway.RegisterProxy(job.ID, policy, a.proxyResolver(access.Name))
			if err != nil {
				return runtime.Result{}, fmt.Errorf("load pipeline stage environment: mint proxied lease for key %q: %w", access.Name, err)
			}
			leases = append(leases, lease)
			entries = append(entries, access.Name+"="+lease.Placeholder())
			entries = append(entries, "GITMOOT_PROXY_"+access.Name+"_URL="+lease.URL())
			continue
		}
		if a.consumerKind == db.KeychainConsumerAgent {
			return runtime.Result{}, fmt.Errorf("load pipeline stage environment: agent key %q must use proxied mode", access.Name)
		}
		if access.Mode != db.KeychainModeInjected {
			return runtime.Result{}, fmt.Errorf("load pipeline stage environment: key %q has unsupported mode %q", access.Name, access.Mode)
		}
		var (
			value string
			ok    bool
		)
		switch access.Source {
		case pipelineKeySourceOwn:
			if own == nil {
				var err error
				own, err = loadValidatedPipelineEnvFile(ctx, a.store, a.home, a.file)
				if err != nil {
					return runtime.Result{}, fmt.Errorf("load pipeline stage environment: %w", err)
				}
			}
			value, ok = own[access.Name]
		case pipelineKeySourceShared:
			key, granted, err := a.store.GetGrantedKey(ctx, db.KeychainConsumerPipeline, a.pipelineName, access.Name)
			if err != nil {
				return runtime.Result{}, fmt.Errorf("load pipeline stage environment: re-check grant for key %q: %w", access.Name, err)
			}
			if !granted || key.Mode != db.KeychainModeInjected {
				return runtime.Result{}, fmt.Errorf("load pipeline stage environment: grant for key %q was revoked or changed", access.Name)
			}
			if shared == nil {
				_, shared, err = LoadValidatedKeychainFile(ctx, a.store, a.home)
				if err != nil {
					return runtime.Result{}, fmt.Errorf("load pipeline stage environment: %w", err)
				}
			}
			value, ok = shared[access.Name]
			ok = ok && strings.TrimSpace(value) != ""
		case pipelineKeySourceDefault:
			value, ok = a.env[access.Name]
		default:
			return runtime.Result{}, fmt.Errorf("load pipeline stage environment: key %q has unknown source %q", access.Name, access.Source)
		}
		if !ok {
			return runtime.Result{}, fmt.Errorf("load pipeline stage environment: key %q is no longer available from source %s", access.Name, access.Source)
		}
		entries = append(entries, access.Name+"="+value)
	}
	if a.consumerKind == db.KeychainConsumerAgent {
		job.AgentEnv = prependPipelineEnvironment(entries, job.AgentEnv)
	} else {
		job.ShellEnv = prependPipelineEnvironment(entries, job.ShellEnv)
	}
	return a.inner.Deliver(ctx, agent, job)
}

func (a pipelineEnvDeliveryAdapter) proxyResolver(name string) credgw.CredentialResolver {
	return func(ctx context.Context) (credgw.ResolvedCredential, error) {
		key, granted, err := a.store.GetGrantedKey(ctx, a.consumerKind, a.consumerID, name)
		if err != nil {
			return credgw.ResolvedCredential{}, err
		}
		if !granted || key.Mode != db.KeychainModeProxied || !key.ProxyConfigured() {
			return credgw.ResolvedCredential{}, errors.New("proxied grant is unavailable")
		}
		_, values, err := LoadValidatedKeychainFile(ctx, a.store, a.home)
		if err != nil {
			return credgw.ResolvedCredential{}, err
		}
		value := values[name]
		if strings.TrimSpace(value) == "" {
			return credgw.ResolvedCredential{}, errors.New("proxied key value is unavailable")
		}
		return credgw.ResolvedCredential{
			Value: value, Upstream: key.ProxyUpstream,
			AuthKind: credgw.ProxyAuthKind(key.ProxyAuthKind), Header: key.ProxyHeader,
		}, nil
	}
}

func (a pipelineEnvDeliveryAdapter) deliverLegacy(ctx context.Context, agent runtime.Agent, job runtime.Job) (runtime.Result, error) {
	available := make(map[string]string, len(a.env))
	for name, value := range a.env {
		available[name] = value
	}
	if strings.TrimSpace(a.file) != "" {
		values, err := loadValidatedPipelineEnvFile(ctx, a.store, a.home, a.file)
		if err != nil {
			return runtime.Result{}, fmt.Errorf("load pipeline stage environment: %w", err)
		}
		for name, value := range values {
			available[name] = value
		}
	}
	entries := make([]string, 0, len(a.keys))
	for _, key := range a.keys {
		value, ok := available[key]
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

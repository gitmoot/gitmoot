package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gitmoot/gitmoot/internal/subprocess"
)

// RuntimeContractState is deliberately tri-state. Only unsupported blocks a
// dispatch; unknown means the probe could not establish an answer and MUST run.
type RuntimeContractState string

const (
	RuntimeContractSupported   RuntimeContractState = "supported"
	RuntimeContractUnsupported RuntimeContractState = "unsupported"
	RuntimeContractUnknown     RuntimeContractState = "unknown"
)

type RuntimeRequirementKind string

const (
	RuntimeRequirementFlag        RuntimeRequirementKind = "flag"
	RuntimeRequirementNonRootEUID RuntimeRequirementKind = "non-root-euid"
)

// RuntimeRequirement declares one fact an adapter's argv depends on.
type RuntimeRequirement struct {
	Kind     RuntimeRequirementKind
	Name     string
	Flag     string
	Source   string
	Remedy   string
	Policies []string
	ChatSeat bool
	// PlanMode scopes a requirement to deliveries whose request enables plan
	// mode. It is request-scoped rather than inferred from the agent: older CLIs
	// that lack an optional plan flag must still run ordinary jobs.
	PlanMode   bool
	Instrument string
}

// RuntimeContract is immutable compiled adapter metadata. Dispatch evaluates it;
// operator metadata overrides cannot alter the adapter's actual requirements.
type RuntimeContract struct {
	Binary       string
	Requirements []RuntimeRequirement
}

// RuntimeContractRequest carries request-scoped axes used to evaluate adapter
// requirements before delivery. It is deliberately not runtime.Job:
// constructing the delivered job remains the mailbox gate's responsibility.
type RuntimeContractRequest struct {
	Plan bool
	// EffectiveUIDKnown means the delivery target will run with EffectiveUID
	// instead of the checker process identity. Only the resolved execution
	// backend may set this override.
	EffectiveUID      int
	EffectiveUIDKnown bool
}

func (c RuntimeContract) clone() RuntimeContract {
	out := c
	out.Requirements = append([]RuntimeRequirement(nil), c.Requirements...)
	for i := range out.Requirements {
		out.Requirements[i].Policies = append([]string(nil), c.Requirements[i].Policies...)
	}
	return out
}

// RuntimeRequirementResult preserves which instrument answered and why. The
// serialized states cannot collapse unknown into unsupported.
type RuntimeRequirementResult struct {
	Kind       RuntimeRequirementKind `json:"kind"`
	Name       string                 `json:"name"`
	Flag       string                 `json:"flag,omitempty"`
	Source     string                 `json:"source"`
	Remedy     string                 `json:"remedy"`
	State      RuntimeContractState   `json:"state"`
	Instrument string                 `json:"instrument"`
	Detail     string                 `json:"detail,omitempty"`
}

type RuntimeContractResult struct {
	Runtime      string                     `json:"runtime"`
	Binary       string                     `json:"binary,omitempty"`
	ResolvedPath string                     `json:"resolved_path,omitempty"`
	Version      string                     `json:"version"`
	State        RuntimeContractState       `json:"state"`
	Instrument   string                     `json:"instrument"`
	Requirements []RuntimeRequirementResult `json:"requirements,omitempty"`
}

type binaryIdentity struct {
	Path          string
	Size          int64
	ModTimeUnixNS int64
}

type binaryProbe struct {
	path       string
	version    string
	help       string
	helpParsed bool
	instrument string
	detail     string
}

type binaryProbeCacheEntry struct {
	probe     binaryProbe
	expiresAt time.Time
}

const unknownBinaryProbeTTL = time.Minute

// RuntimeContractChecker probes lazily and caches by executable identity. A CLI
// update changes path, size, or mtime and therefore forces the next dispatch to
// ask the new binary again.
type RuntimeContractChecker struct {
	Runner       subprocess.Runner
	Registry     Registry
	Timeout      time.Duration
	EffectiveUID func() (int, bool)
	now          func() time.Time

	mu    sync.Mutex
	cache map[binaryIdentity]binaryProbeCacheEntry
}

func NewRuntimeContractChecker(runner subprocess.Runner, registry Registry) *RuntimeContractChecker {
	if runner == nil {
		runner = subprocess.GroupRunner{}
	}
	return &RuntimeContractChecker{
		Runner:   runner,
		Registry: registry,
		Timeout:  3 * time.Second,
		EffectiveUID: func() (int, bool) {
			return os.Geteuid(), true
		},
		now:   time.Now,
		cache: make(map[binaryIdentity]binaryProbeCacheEntry),
	}
}

var defaultRuntimeContractChecker = NewRuntimeContractChecker(subprocess.GroupRunner{}, BuiltinRuntimeRegistry())

func DefaultRuntimeContractChecker() *RuntimeContractChecker { return defaultRuntimeContractChecker }

// Check evaluates requirements used by an ordinary, non-plan delivery.
func (c *RuntimeContractChecker) Check(ctx context.Context, agent Agent) RuntimeContractResult {
	return c.CheckRequest(ctx, agent, RuntimeContractRequest{})
}

// CheckRequest evaluates only requirements used by this agent and request's
// concrete argv. Request-scoped features must not be inferred from static agent
// metadata.
func (c *RuntimeContractChecker) CheckRequest(ctx context.Context, agent Agent, request RuntimeContractRequest) RuntimeContractResult {
	meta, ok := c.registry().Metadata(agent.Runtime)
	if !ok {
		return RuntimeContractResult{Runtime: agent.Runtime, Version: "unknown", State: RuntimeContractUnknown, Instrument: "runtime-registry"}
	}
	return c.check(ctx, meta, request, func(req RuntimeRequirement) bool { return requirementApplies(req, agent, request) })
}

// Inspect evaluates a runtime's requirements for doctor. It deliberately SKIPS
// request-scoped requirements (PlanMode), because doctor answers "can this host
// run this runtime", not "could this host serve every optional capability".
//
// Including them made doctor cry wolf: on a host whose omp predates --plan-yolo,
// Inspect reported the omp contract UNSUPPORTED even though every ordinary omp job
// dispatches and runs — the dispatch preflight scopes those flags to plan requests,
// so the two instruments disagreed about the same host. The operator-facing remedy
// then told them to install a newer CLI they did not need, which is worse than
// silence: a red that is not a defect teaches people to ignore the instrument.
//
// A host that cannot serve plan mode learns it when a plan job is refused, loudly,
// naming the flag. That refusal is the right place for it, because it is the only
// place the capability is actually required.
func (c *RuntimeContractChecker) Inspect(ctx context.Context, runtimeName string) RuntimeContractResult {
	meta, ok := c.registry().Metadata(runtimeName)
	if !ok {
		return RuntimeContractResult{Runtime: runtimeName, Version: "unknown", State: RuntimeContractUnknown, Instrument: "runtime-registry"}
	}
	return c.check(ctx, meta, RuntimeContractRequest{}, func(req RuntimeRequirement) bool { return !req.PlanMode })
}

func (c *RuntimeContractChecker) registry() Registry {
	if len(c.Registry.order) == 0 {
		return BuiltinRuntimeRegistry()
	}
	return c.Registry
}

func (c *RuntimeContractChecker) check(ctx context.Context, meta RuntimeMetadata, request RuntimeContractRequest, applies func(RuntimeRequirement) bool) RuntimeContractResult {
	result := RuntimeContractResult{Runtime: meta.Name, Binary: meta.Contract.Binary, Version: "unknown", State: RuntimeContractSupported, Instrument: "declaration"}
	var flagRequirements []RuntimeRequirement
	for _, req := range meta.Contract.Requirements {
		if !applies(req) {
			continue
		}
		if req.Kind == RuntimeRequirementFlag {
			flagRequirements = append(flagRequirements, req)
			continue
		}
		rr := c.evaluatePrecondition(req, request)
		result.Requirements = append(result.Requirements, rr)
		mergeContractState(&result, rr)
	}
	if len(flagRequirements) == 0 {
		return result
	}
	probe := c.probeBinary(ctx, meta.Contract.Binary)
	result.ResolvedPath = probe.path
	result.Version = probe.version
	for _, req := range flagRequirements {
		rr := RuntimeRequirementResult{Kind: req.Kind, Name: req.Name, Flag: req.Flag, Source: req.Source, Remedy: req.Remedy, Instrument: probe.instrument}
		switch {
		case !probe.helpParsed:
			rr.State = RuntimeContractUnknown
			rr.Detail = probe.detail
		case helpContainsFlag(probe.help, req.Flag):
			rr.State = RuntimeContractSupported
			rr.Detail = "installed binary help lists " + req.Flag
		default:
			rr.State = RuntimeContractUnsupported
			rr.Detail = "installed binary help was parsed and does not list " + req.Flag
		}
		result.Requirements = append(result.Requirements, rr)
		mergeContractState(&result, rr)
	}
	if result.State == RuntimeContractSupported {
		result.Instrument = probe.instrument
	}
	return result
}

func (c *RuntimeContractChecker) evaluatePrecondition(req RuntimeRequirement, request RuntimeContractRequest) RuntimeRequirementResult {
	rr := RuntimeRequirementResult{Kind: req.Kind, Name: req.Name, Flag: req.Flag, Source: req.Source, Remedy: req.Remedy, Instrument: req.Instrument}
	switch req.Kind {
	case RuntimeRequirementNonRootEUID:
		euid, known := request.EffectiveUID, request.EffectiveUIDKnown
		if !known {
			lookup := c.EffectiveUID
			if lookup == nil {
				rr.State = RuntimeContractUnknown
				rr.Detail = "effective uid is unavailable"
				return rr
			}
			euid, known = lookup()
		}
		if !known {
			rr.State = RuntimeContractUnknown
			rr.Detail = "effective uid is unavailable"
		} else if euid == 0 {
			rr.State = RuntimeContractUnsupported
			rr.Detail = "effective uid is 0"
		} else {
			rr.State = RuntimeContractSupported
			rr.Detail = fmt.Sprintf("effective uid is %d", euid)
		}
	default:
		rr.State = RuntimeContractUnknown
		rr.Detail = "precondition evaluator is unavailable"
	}
	return rr
}

func (c *RuntimeContractChecker) probeBinary(ctx context.Context, binary string) binaryProbe {
	runner := c.Runner
	if runner == nil {
		runner = subprocess.GroupRunner{}
	}
	path, err := runner.LookPath(binary)
	if err != nil {
		return binaryProbe{version: "unknown", instrument: "look-path", detail: fmt.Sprintf("resolve %s: %v", binary, err)}
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return binaryProbe{path: path, version: "unknown", instrument: "binary-identity", detail: fmt.Sprintf("resolve executable identity: %v", err)}
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return binaryProbe{path: resolved, version: "unknown", instrument: "binary-identity", detail: fmt.Sprintf("stat executable identity: %v", err)}
	}
	identity := binaryIdentity{Path: resolved, Size: info.Size(), ModTimeUnixNS: info.ModTime().UnixNano()}
	now := time.Now
	if c.now != nil {
		now = c.now
	}
	checkedAt := now()
	c.mu.Lock()
	entry, ok := c.cache[identity]
	if ok && !entry.expiresAt.IsZero() && !checkedAt.Before(entry.expiresAt) {
		delete(c.cache, identity)
		ok = false
	}
	c.mu.Unlock()
	if ok {
		return entry.probe
	}
	probe := c.runBinaryProbe(ctx, resolved)
	entry = binaryProbeCacheEntry{probe: probe}
	if !probe.helpParsed {
		// Unknown is transient, but a timed-out or unparseable help response should
		// not impose the full probe cost on every dispatch. Retry it after a short
		// bounded interval.
		entry.expiresAt = now().Add(unknownBinaryProbeTTL)
	}
	c.mu.Lock()
	c.cache[identity] = entry
	c.mu.Unlock()
	return probe
}

func (c *RuntimeContractChecker) runBinaryProbe(ctx context.Context, path string) binaryProbe {
	runner := c.Runner
	if runner == nil {
		runner = subprocess.GroupRunner{}
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	helpCtx, cancel := context.WithTimeout(ctx, timeout)
	helpResult, helpErr := runner.Run(helpCtx, "", path, "--help")
	helpCtxErr := helpCtx.Err()
	cancel()
	help := strings.TrimSpace(strings.Join([]string{helpResult.Stdout, helpResult.Stderr}, "\n"))
	probe := binaryProbe{path: path, version: "unknown", help: help, instrument: "binary-help"}
	if helpCtxErr != nil {
		probe.detail = "binary help probe timed out"
	} else if !helpLooksParseable(help) {
		// The probe reports what the CLI SAYS, and a CLI that says nothing has not
		// said no. Unparseable output is unknown even when the process exited 0.
		probe.detail = "binary help output was not parseable"
		if helpErr != nil {
			probe.detail += ": " + helpErr.Error()
		}
	} else {
		probe.helpParsed = true
	}
	versionCtx, versionCancel := context.WithTimeout(ctx, timeout)
	versionResult, versionErr := runner.Run(versionCtx, "", path, "--version")
	versionCtxErr := versionCtx.Err()
	versionCancel()
	if versionCtxErr == nil && versionErr == nil {
		if version := firstNonEmptyLine(versionResult.Stdout, versionResult.Stderr); version != "" {
			probe.version = version
		}
	}
	return probe
}

func requirementApplies(req RuntimeRequirement, agent Agent, request RuntimeContractRequest) bool {
	if req.PlanMode && !request.Plan {
		return false
	}
	if req.ChatSeat && agent.ChatSeat {
		return true
	}
	if len(req.Policies) == 0 {
		return !req.ChatSeat
	}
	policy := NormalizeStoredAutonomyPolicy(agent.AutonomyPolicy)
	for _, candidate := range req.Policies {
		if policy == candidate {
			return true
		}
	}
	return false
}

func mergeContractState(result *RuntimeContractResult, requirement RuntimeRequirementResult) {
	if requirement.State == RuntimeContractUnsupported || (requirement.State == RuntimeContractUnknown && result.State == RuntimeContractSupported) {
		result.State = requirement.State
		result.Instrument = requirement.Instrument
	}
}

func helpLooksParseable(help string) bool {
	lower := strings.ToLower(help)
	return strings.Contains(lower, "usage") && helpOptionLine.FindStringIndex(help) != nil
}

var helpOptionLine = regexp.MustCompile(`(?m)^\s*(?:-[A-Za-z0-9],\s*)?--[A-Za-z0-9][A-Za-z0-9-]*`)

func helpContainsFlag(help, flag string) bool {
	if strings.TrimSpace(flag) == "" {
		return false
	}
	pattern := `(^|[^A-Za-z0-9-])` + regexp.QuoteMeta(flag) + `([^A-Za-z0-9-]|$)`
	return regexp.MustCompile(pattern).FindStringIndex(help) != nil
}

func firstNonEmptyLine(values ...string) string {
	for _, value := range values {
		for _, line := range strings.Split(value, "\n") {
			if line = strings.TrimSpace(line); line != "" {
				return line
			}
		}
	}
	return ""
}

// RuntimeContractDispatchError blocks only a positively unsupported contract.
// Unknown is evidence to record, not authority to refuse a working job.
func RuntimeContractDispatchError(agent Agent, result RuntimeContractResult) error {
	if result.State != RuntimeContractUnsupported {
		return nil
	}
	for _, requirement := range result.Requirements {
		if requirement.State != RuntimeContractUnsupported {
			continue
		}
		return fmt.Errorf("runtime preflight blocked agent %q: runtime %q installed version %q does not satisfy %s required by %s; remedy: %s",
			agent.Name, result.Runtime, result.Version, requirement.Name, requirement.Source, requirement.Remedy)
	}
	return fmt.Errorf("runtime preflight blocked agent %q: runtime %q installed version %q has an unsupported contract; remedy: run the job on a runtime whose installed CLI satisfies its declared contract", agent.Name, result.Runtime, result.Version)
}

func RuntimeContractEventMessage(jobID string, agent Agent, result RuntimeContractResult) string {
	payload := struct {
		JobID string `json:"job_id"`
		Agent string `json:"agent"`
		RuntimeContractResult
	}{JobID: jobID, Agent: agent.Name, RuntimeContractResult: result}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Sprintf("runtime=%s state=%s instrument=%s", result.Runtime, result.State, result.Instrument)
	}
	return string(encoded)
}

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
	Kind       RuntimeRequirementKind
	Name       string
	Flag       string
	Source     string
	Remedy     string
	Policies   []string
	ChatSeat   bool
	Instrument string
}

// RuntimeContract is immutable compiled adapter metadata. Dispatch evaluates it;
// operator metadata overrides cannot alter the adapter's actual requirements.
type RuntimeContract struct {
	Binary       string
	Requirements []RuntimeRequirement
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

// RuntimeContractChecker probes lazily and caches by executable identity. A CLI
// update changes path, size, or mtime and therefore forces the next dispatch to
// ask the new binary again.
type RuntimeContractChecker struct {
	Runner       subprocess.Runner
	Registry     Registry
	Timeout      time.Duration
	EffectiveUID func() (int, bool)

	mu    sync.Mutex
	cache map[binaryIdentity]binaryProbe
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
		cache: make(map[binaryIdentity]binaryProbe),
	}
}

var defaultRuntimeContractChecker = NewRuntimeContractChecker(subprocess.GroupRunner{}, BuiltinRuntimeRegistry())

func DefaultRuntimeContractChecker() *RuntimeContractChecker { return defaultRuntimeContractChecker }

// Check evaluates only requirements used by this agent's concrete argv.
func (c *RuntimeContractChecker) Check(ctx context.Context, agent Agent) RuntimeContractResult {
	meta, ok := c.registry().Metadata(agent.Runtime)
	if !ok {
		return RuntimeContractResult{Runtime: agent.Runtime, Version: "unknown", State: RuntimeContractUnknown, Instrument: "runtime-registry"}
	}
	return c.check(ctx, meta, func(req RuntimeRequirement) bool { return requirementApplies(req, agent) })
}

// Inspect evaluates every declared requirement for a runtime for doctor.
func (c *RuntimeContractChecker) Inspect(ctx context.Context, runtimeName string) RuntimeContractResult {
	meta, ok := c.registry().Metadata(runtimeName)
	if !ok {
		return RuntimeContractResult{Runtime: runtimeName, Version: "unknown", State: RuntimeContractUnknown, Instrument: "runtime-registry"}
	}
	return c.check(ctx, meta, func(RuntimeRequirement) bool { return true })
}

func (c *RuntimeContractChecker) registry() Registry {
	if len(c.Registry.order) == 0 {
		return BuiltinRuntimeRegistry()
	}
	return c.Registry
}

func (c *RuntimeContractChecker) check(ctx context.Context, meta RuntimeMetadata, applies func(RuntimeRequirement) bool) RuntimeContractResult {
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
		rr := c.evaluatePrecondition(req)
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

func (c *RuntimeContractChecker) evaluatePrecondition(req RuntimeRequirement) RuntimeRequirementResult {
	rr := RuntimeRequirementResult{Kind: req.Kind, Name: req.Name, Flag: req.Flag, Source: req.Source, Remedy: req.Remedy, Instrument: req.Instrument}
	switch req.Kind {
	case RuntimeRequirementNonRootEUID:
		lookup := c.EffectiveUID
		if lookup == nil {
			rr.State = RuntimeContractUnknown
			rr.Detail = "effective uid is unavailable"
			return rr
		}
		euid, known := lookup()
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
	c.mu.Lock()
	probe, ok := c.cache[identity]
	c.mu.Unlock()
	if ok {
		return probe
	}
	probe = c.runBinaryProbe(ctx, resolved)
	// Unknown is a transient observation, not a durable capability verdict. A
	// timeout or unparseable response must be retried on the next dispatch rather
	// than disabling preflight until the executable identity changes.
	if probe.helpParsed {
		c.mu.Lock()
		c.cache[identity] = probe
		c.mu.Unlock()
	}
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

func requirementApplies(req RuntimeRequirement, agent Agent) bool {
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

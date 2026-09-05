package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/transcript"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// phaseProfileEventKind is the job-event kind carrying a review's command-phase
// decomposition (#1824). Event kinds are free-form strings, so this needs no
// store schema change and no edit to internal/db.
const phaseProfileEventKind = "phase_profile"

// Phase buckets. These classify a command's OWN measured time. They do not
// partition the run on their own - see phaseProfile for the identity that does.
const (
	phaseBucketTest  = "test"
	phaseBucketBuild = "build"
	phaseBucketVCS   = "vcs"
	phaseBucketMixed = "mixed"
	phaseBucketOther = "other"
)

// Coverage tells a reader whether an EMPTY decomposition is a measurement or a
// blind spot. Claude's stream carries one final envelope and no tool events at
// all (internal/transcript/translate.go: claudeTranslator), so a claude run
// yields no commands however long it takes. Without this tag a claude profile
// would read as "residual dominates", which is a claim about the instrument
// rather than about the run - and claude is 26% of review runs at roughly twice
// codex's median, so that misreading would land exactly where it hurts.
const (
	phaseCoverageDecomposed = "decomposed"
	phaseCoverageOpaque     = "opaque_runtime"
)

// runtimeEmitsToolEvents reports whether a runtime's translator emits per-tool
// events at all. Anything else is opaque and is reported as such.
func runtimeEmitsToolEvents(runtimeName string) bool {
	switch strings.ToLower(strings.TrimSpace(runtimeName)) {
	case "codex", "kimi":
		return true
	default:
		return false
	}
}

// shellToolNames are the tool names whose payload is a shell command. Only
// these contribute to the command decomposition; a file_change or a web fetch
// is a tool event but not a command, and counting it as one inflates the
// command count with rows the buckets cannot describe (#1824 review F2).
func isShellToolName(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "bash", "sh", "shell", "zsh", "powershell", "command", "command_execution", "exec", "exec_command", "terminal":
		return true
	default:
		return false
	}
}

// extractToolCommand pulls the shell command out of a tool's input payload.
// Codex sends the command as a bare string; kimi sends function arguments as
// JSON, e.g. {"command":"go test ./..."}, and handing that JSON to a
// leading-token classifier lands every kimi command in `other` (#1824 review
// F2). Unknown shapes return the raw text, which classifies as other honestly.
func extractToolCommand(input string) string {
	trimmed := strings.TrimSpace(input)
	if !strings.HasPrefix(trimmed, "{") {
		return trimmed
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(trimmed), &decoded); err != nil {
		return trimmed
	}
	for _, key := range []string{"command", "cmd", "script", "shell_command"} {
		switch value := decoded[key].(type) {
		case string:
			if strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		case []any:
			parts := make([]string, 0, len(value))
			for _, item := range value {
				if text, ok := item.(string); ok {
					parts = append(parts, text)
				}
			}
			if len(parts) > 0 {
				return strings.Join(parts, " ")
			}
		}
	}
	return trimmed
}

// classifyPhaseCommand maps a shell command to a bucket by classifying EVERY
// segment of it. A single leading token cannot describe `go test ./... && go
// build ./...` (which bills build time to test) or `cd repo && go test ./...`
// and `time go test ./...` (which land in other) - all three measured in #1824
// review F3. Segments that disagree yield `mixed`, which says "this command
// spans phases" rather than picking one and being wrong.
func classifyPhaseCommand(command string) string {
	segments := splitCommandSegments(extractToolCommand(command))
	seen := ""
	for _, segment := range segments {
		bucket := classifyCommandSegment(segment)
		// A pure `cd`/`export` prefix is plumbing, not a phase: it must not
		// turn `cd repo && go test` into mixed.
		if bucket == "" {
			continue
		}
		switch {
		case seen == "":
			seen = bucket
		case seen != bucket:
			return phaseBucketMixed
		}
	}
	if seen == "" {
		return phaseBucketOther
	}
	return seen
}

// splitCommandSegments splits on shell sequencing operators. It is deliberately
// naive about quoting: a `&&` inside a quoted string over-splits into segments
// that classify as other, which degrades a label rather than inventing time.
func splitCommandSegments(command string) []string {
	fields := strings.FieldsFunc(command, func(r rune) bool { return r == '\n' || r == ';' })
	segments := make([]string, 0, len(fields))
	for _, field := range fields {
		for _, part := range strings.Split(strings.ReplaceAll(field, "||", "&&"), "&&") {
			for _, piped := range strings.Split(part, "|") {
				if trimmed := strings.TrimSpace(piped); trimmed != "" {
					segments = append(segments, trimmed)
				}
			}
		}
	}
	return segments
}

// classifyCommandSegment classifies one segment, or returns "" for pure
// plumbing (cd, export, an env-only line) that should not colour the result.
func classifyCommandSegment(segment string) string {
	normalize := func(token string) string {
		token = strings.Trim(token, "'\"")
		if idx := strings.LastIndex(token, "/"); idx >= 0 && idx+1 < len(token) {
			token = token[idx+1:]
		}
		return token
	}
	fields := strings.Fields(strings.ToLower(strings.TrimSpace(segment)))
	plumbing := false
	for len(fields) > 0 {
		raw := strings.Trim(fields[0], "'\"")
		if strings.Contains(raw, "=") && !strings.HasPrefix(raw, "-") {
			fields = fields[1:]
			plumbing = true
			continue
		}
		switch normalize(fields[0]) {
		case "bash", "sh", "zsh", "env", "-c", "-lc", "-lic", "time", "nice", "nohup", "sudo", "xargs":
			fields = fields[1:]
			continue
		case "cd", "export", "pushd", "popd", "source", "mkdir", "rm", "cp", "mv", "echo", "set":
			return ""
		case "grep", "rg", "awk", "sed", "head", "tail", "cut", "tr", "sort", "uniq", "wc", "tee", "jq", "cat":
			// A pipeline consumer is a FILTER on another command's output, not
			// a phase of its own: `go test ./... | grep FAIL` is a test run,
			// and calling it mixed would label most real invocations
			// unclassifiable.
			return ""
		}
		break
	}
	if len(fields) == 0 {
		if plumbing {
			return ""
		}
		return phaseBucketOther
	}
	head := normalize(fields[0])
	sub := ""
	if len(fields) > 1 {
		sub = normalize(fields[1])
	}
	switch head {
	case "git", "gh":
		return phaseBucketVCS
	case "go":
		switch sub {
		case "test":
			return phaseBucketTest
		case "build", "vet", "generate", "install":
			return phaseBucketBuild
		}
		return phaseBucketOther
	case "gofmt":
		return phaseBucketBuild
	case "gotestsum":
		return phaseBucketTest
	}
	return phaseBucketOther
}

// phaseProfile is the emitted payload. Durations are milliseconds so the row is
// readable without unit guessing.
//
// TWO IDENTITIES, and the difference is the point (#1824 review F1). Command
// intervals can OVERLAP, so their durations do not partition anything:
//
//	CoveredMS + ResidualMS == WallMS          (exact, always)
//	sum(BucketMS)          == CoveredMS + OverlapMS
//
// Covered is the UNION of command intervals, so residual - time with no command
// in flight - is non-negative by construction with nothing clamped. An earlier
// version subtracted the SUM and clamped a negative result to zero, which
// silently absorbed 24ms of a measured 76ms run and made the documented
// partition false exactly when overlap occurred.
type phaseProfile struct {
	Coverage   string `json:"coverage"`
	Runtime    string `json:"runtime"`
	Attempt    int64  `json:"attempt"`
	WallMS     int64  `json:"wall_ms"`
	CoveredMS  int64  `json:"covered_ms"`
	ResidualMS int64  `json:"residual_ms"`
	OverlapMS  int64  `json:"overlap_ms"`
	Commands   int    `json:"commands"`
	// Unpaired counts tool results with no matching call id. They contribute
	// no time; reporting them keeps a stream this instrument cannot follow
	// visible instead of silently short-measuring the run.
	Unpaired int `json:"unpaired"`
	// ToolEvents counts non-shell tool results (file_change and friends).
	ToolEvents  int              `json:"tool_events"`
	BucketMS    map[string]int64 `json:"bucket_ms"`
	BucketCount map[string]int   `json:"bucket_count"`
}

type pendingCommand struct {
	command string
	tool    string
	started time.Time
}

type commandInterval struct {
	start time.Time
	end   time.Time
}

// retainedTranscript is the handle every production caller writes through. It
// is a CONCRETE pointer type on purpose: callers compare it against nil, and
// returning an interface would hand them a non-nil interface holding a nil
// pointer on the capture-disabled path.
type retainedTranscript struct {
	// sink is an io.Writer rather than *os.File so a test can inject a short
	// or failing write. Without that the observe-before-write mutant is
	// unkillable, which is how it survived while being reported as killed
	// (#1824 review F4).
	sink   io.Writer
	closer io.Closer

	store   *db.Store
	jobID   string
	runtime string
	attempt int64

	translator transcript.Translator
	pending    map[string]pendingCommand
	intervals  []commandInterval
	partial    []byte
	started    time.Time

	bucketNS    map[string]int64
	bucketCount map[string]int
	commands    int
	unpaired    int
	toolEvents  int
}

// newRetainedTranscript attaches the phase instrument to an open transcript
// file. A job whose runtime is unknown, or whose runtime emits no tool events,
// still gets a profile row - tagged opaque, because a silent absence is
// indistinguishable from a fast run.
func newRetainedTranscript(file *os.File, jobID, jobType, runtimeName string, attempt int64, store *db.Store) *retainedTranscript {
	handle := &retainedTranscript{
		sink:        file,
		closer:      file,
		store:       store,
		jobID:       jobID,
		attempt:     attempt,
		started:     time.Now(),
		pending:     map[string]pendingCommand{},
		bucketNS:    map[string]int64{},
		bucketCount: map[string]int{},
	}
	if file == nil {
		handle.sink = nil
		handle.closer = nil
	}
	// #1824 asks where a REVIEW's wall time goes, and the profile is appended
	// after a job's terminal events. Emitting it for every job type would change
	// the observable event sequence of jobs this work has no business touching -
	// three exec-backend E2Es assert their sequence exactly, as their acceptance
	// contract. Scope the instrument to the population it was built to measure.
	if !strings.EqualFold(strings.TrimSpace(jobType), "review") {
		return handle
	}
	handle.runtime = strings.TrimSpace(runtimeName)
	if store == nil {
		return handle
	}
	if runtimeEmitsToolEvents(handle.runtime) {
		if translator, err := transcript.NewTranslator(handle.runtime); err == nil {
			handle.translator = translator
		}
	}
	if handle.translator == nil {
		// Opaque runtimes still emit a row, so Close must have something to
		// drive the emission. A no-op translator keeps Write on the cheap path
		// while leaving Close's single emit intact.
		handle.translator = opaqueTranslator{}
	}
	return handle
}

// opaqueTranslator yields no events. It exists so a runtime with no tool stream
// still produces a profile tagged opaque_runtime rather than no row at all.
type opaqueTranslator struct{}

func (opaqueTranslator) Translate(string) []transcript.Event { return nil }
func (opaqueTranslator) Flush() []transcript.Event           { return nil }

// Write persists the bytes FIRST and observes only what was actually persisted.
// Instrumentation must never be able to lose, reorder or double-count a
// transcript byte, so the sink write is the only operation whose error is
// returned, and a short write observes p[:n] rather than all of p.
func (r *retainedTranscript) Write(p []byte) (int, error) {
	if r == nil || r.sink == nil {
		return len(p), nil
	}
	n, err := r.sink.Write(p)
	if r.translator != nil && n > 0 {
		r.observe(p[:n])
	}
	return n, err
}

func (r *retainedTranscript) observe(p []byte) {
	r.partial = append(r.partial, p...)
	// Scan the BYTES. An earlier form called string(r.partial) once per
	// iteration, copying the whole pending buffer per line - measured at 25x
	// the bare write path. An instrument built to measure cost must not be the
	// cost.
	for {
		idx := bytes.IndexByte(r.partial, '\n')
		if idx < 0 {
			return
		}
		line := string(r.partial[:idx])
		r.partial = r.partial[idx+1:]
		r.consume(r.translator.Translate(line))
	}
}

func (r *retainedTranscript) consume(events []transcript.Event) {
	for _, event := range events {
		switch event.Kind {
		case transcript.KindToolCall:
			if !isShellToolName(event.Name) {
				continue
			}
			// Pair by TOOL ID. Arrival order is wrong the moment two tools
			// overlap: a probe where vcs finished first swapped the two
			// durations outright (#1824 review F1).
			r.pending[event.ToolID] = pendingCommand{
				command: extractToolCommand(event.InputDigest),
				tool:    event.Name,
				started: time.Now(),
			}
		case transcript.KindToolResult:
			if !isShellToolName(event.Name) {
				r.toolEvents++
				continue
			}
			call, ok := r.pending[event.ToolID]
			if !ok {
				r.unpaired++
				continue
			}
			delete(r.pending, event.ToolID)
			end := time.Now()
			// Time the interval HERE rather than reading event.Duration: the
			// duration cannot be attributed to a command without the pairing
			// this branch just did, and owning both ends keeps the interval
			// available for the union that residual needs.
			bucket := classifyPhaseCommand(call.command)
			r.bucketNS[bucket] += end.Sub(call.started).Nanoseconds()
			r.bucketCount[bucket]++
			r.intervals = append(r.intervals, commandInterval{start: call.started, end: end})
			r.commands++
		}
	}
}

// Close flushes the translator, emits one bounded profile row, and closes the
// file. One row per ATTEMPT rather than per job: RetryJob preserves prior
// job_events and re-delivers the same job id, so a retried review would
// otherwise double-count against a one-row-per-job contract (#1824 review F5).
func (r *retainedTranscript) Close() error {
	if r == nil {
		return nil
	}
	if r.translator != nil {
		if len(r.partial) > 0 {
			// The final line of a stream that ends without a newline is a real
			// command result; dropping it silently short-measures the run.
			r.consume(r.translator.Translate(string(r.partial)))
			r.partial = nil
		}
		r.consume(r.translator.Flush())
		r.emit()
	}
	if r.closer == nil {
		return nil
	}
	return r.closer.Close()
}

// unionMS returns the total wall time during which at least one command was in
// flight, merging overlapping intervals.
func unionNS(intervals []commandInterval) int64 {
	if len(intervals) == 0 {
		return 0
	}
	sorted := make([]commandInterval, len(intervals))
	copy(sorted, intervals)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].start.Before(sorted[j].start) })
	var total time.Duration
	current := sorted[0]
	for _, interval := range sorted[1:] {
		if interval.start.After(current.end) {
			total += current.end.Sub(current.start)
			current = interval
			continue
		}
		if interval.end.After(current.end) {
			current.end = interval.end
		}
	}
	total += current.end.Sub(current.start)
	return total.Nanoseconds()
}

func (r *retainedTranscript) emit() {
	if r.store == nil || strings.TrimSpace(r.jobID) == "" {
		return
	}
	// ARITHMETIC IN NANOSECONDS, rounded once at the edge. Truncating each
	// interval to milliseconds first loses sub-millisecond remainders, and the
	// bucket sum then disagrees with the union by a millisecond per command -
	// small, but it makes a documented identity false, which is the same defect
	// class as the clamp this rewrite removed.
	wallNS := time.Since(r.started).Nanoseconds()
	var summedNS int64
	for _, ns := range r.bucketNS {
		summedNS += ns
	}
	coveredNS := unionNS(r.intervals)
	if coveredNS > wallNS {
		// Cannot happen while both ends come from the same monotonic clock, but
		// a covered span longer than the run would make residual negative, and
		// a negative residual is exactly what this rewrite removes.
		coveredNS = wallNS
	}
	roundMS := func(ns int64) int64 {
		return (ns + int64(time.Millisecond)/2) / int64(time.Millisecond)
	}
	bucketMS := make(map[string]int64, len(r.bucketNS))
	for bucket, ns := range r.bucketNS {
		bucketMS[bucket] = roundMS(ns)
	}
	wall := roundMS(wallNS)
	covered := roundMS(coveredNS)
	if covered > wall {
		covered = wall
	}
	coverage := phaseCoverageDecomposed
	if !runtimeEmitsToolEvents(r.runtime) {
		coverage = phaseCoverageOpaque
	}
	profile := phaseProfile{
		Coverage:    coverage,
		Runtime:     strings.TrimSpace(r.runtime),
		Attempt:     r.attempt,
		WallMS:      wall,
		CoveredMS:   covered,
		ResidualMS:  wall - covered,
		OverlapMS:   roundMS(summedNS - coveredNS),
		Commands:    r.commands,
		Unpaired:    r.unpaired,
		ToolEvents:  r.toolEvents,
		BucketMS:    bucketMS,
		BucketCount: r.bucketCount,
	}
	if profile.OverlapMS < 0 {
		profile.OverlapMS = 0
	}
	encoded, err := json.Marshal(profile)
	if err != nil {
		return
	}
	// Best-effort: a profile is diagnostic, so a store error must never fail the
	// job whose transcript this is.
	_ = r.store.AddJobEvent(context.Background(), db.JobEvent{
		JobID:   r.jobID,
		Kind:    phaseProfileEventKind,
		Message: string(encoded),
	})
}

// sortedBuckets is used by tests and by any reader that wants deterministic
// ordering out of the map.
func sortedBuckets(buckets map[string]int64) []string {
	names := make([]string, 0, len(buckets))
	for name := range buckets {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// effectiveTranscriptRuntime mirrors resolveTranscriptRuntime's precedence -
// payload override first, then the agent's registered runtime - without the
// store lookup that function needs when it starts from only a job id. Callers
// on the dispatch path already hold both, so the answer is free there.
func effectiveTranscriptRuntime(payload workflow.JobPayload, agent runtime.Agent) string {
	if override := strings.TrimSpace(payload.RuntimeOverride); override != "" {
		return override
	}
	return strings.TrimSpace(agent.Runtime)
}

var _ io.Writer = (*retainedTranscript)(nil)

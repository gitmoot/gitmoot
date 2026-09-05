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

// phaseProfileEventKind is the job-event kind carrying a run's command-phase
// decomposition (#1824). Event kinds are free-form strings, so this needs no
// store schema change and no edit to internal/db.
const phaseProfileEventKind = "phase_profile"

// Phase buckets. These partition a run's OBSERVED command time; whatever is
// left of the run's wall clock is reported as residual rather than being
// distributed across them, so the buckets can never sum to more than the run.
const (
	phaseBucketTest  = "test"
	phaseBucketBuild = "build"
	phaseBucketVCS   = "vcs"
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

// runtimesWithToolEvents lists the runtimes whose translators emit per-tool
// events with durations. Anything else is opaque and is reported as such.
func runtimeEmitsToolEvents(runtimeName string) bool {
	switch strings.ToLower(strings.TrimSpace(runtimeName)) {
	case "codex", "kimi":
		return true
	default:
		return false
	}
}

// classifyPhaseCommand maps a shell command to a bucket. It reads the FIRST
// meaningful token sequence rather than searching anywhere in the string, so
// `git commit -m "go test is slow"` is vcs and not test.
func classifyPhaseCommand(command string) string {
	// Real codex commands arrive as `/bin/bash -lc 'go test ./...'`, so tokens
	// carry quoting and the interpreter carries a path. Both are stripped
	// before matching, or every wrapped command silently lands in other -
	// which is the whole population of a codex review.
	normalize := func(token string) string {
		token = strings.Trim(token, "'\"")
		if idx := strings.LastIndex(token, "/"); idx >= 0 && idx+1 < len(token) {
			token = token[idx+1:]
		}
		return token
	}
	fields := strings.Fields(strings.ToLower(strings.TrimSpace(command)))
	// Step past env-var assignments and a leading interpreter wrapper so
	// `GOFLAGS=-mod=mod go test ./...` classifies as test.
	for len(fields) > 0 {
		// The env-assignment test reads the RAW token: normalizing first strips
		// the path out of `GOCACHE=/tmp/x` and with it the `=` that identifies
		// an assignment.
		raw := strings.Trim(fields[0], "'\"")
		if strings.Contains(raw, "=") && !strings.HasPrefix(raw, "-") {
			fields = fields[1:]
			continue
		}
		switch normalize(fields[0]) {
		case "bash", "sh", "zsh", "env", "-c", "-lc", "-lic":
			fields = fields[1:]
			continue
		}
		break
	}
	if len(fields) == 0 {
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
type phaseProfile struct {
	Coverage    string           `json:"coverage"`
	Runtime     string           `json:"runtime"`
	WallMS      int64            `json:"wall_ms"`
	ResidualMS  int64            `json:"residual_ms"`
	Commands    int              `json:"commands"`
	BucketMS    map[string]int64 `json:"bucket_ms"`
	BucketCount map[string]int   `json:"bucket_count"`
}

// retainedTranscript is the handle every production caller writes through. It
// is a CONCRETE pointer type on purpose: callers compare it against nil, and
// returning an interface would hand them a non-nil interface holding a nil
// pointer on the capture-disabled path.
type retainedTranscript struct {
	file *os.File

	store *db.Store
	jobID string

	runtime string

	translator transcript.Translator
	pending    []string
	partial    []byte
	started    time.Time

	bucketMS    map[string]int64
	bucketCount map[string]int
	commands    int
}

// newRetainedTranscript attaches the phase instrument to an open transcript
// file. A job whose runtime is unknown, or whose runtime emits no tool events,
// still gets a profile row - tagged opaque, because a silent absence is
// indistinguishable from a fast run.
func newRetainedTranscript(file *os.File, jobID, runtimeName string, store *db.Store) *retainedTranscript {
	handle := &retainedTranscript{
		file:        file,
		store:       store,
		jobID:       jobID,
		started:     time.Now(),
		bucketMS:    map[string]int64{},
		bucketCount: map[string]int{},
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

func (opaqueTranslator) Translate(string) []transcript.Event { return nil }
func (opaqueTranslator) Flush() []transcript.Event           { return nil }

// Write persists the bytes FIRST and observes them second. Instrumentation must
// never be able to lose or reorder a transcript byte, so the file write is the
// only operation whose error is returned; a parse that goes wrong degrades the
// profile and nothing else.
func (r *retainedTranscript) Write(p []byte) (int, error) {
	if r == nil || r.file == nil {
		return len(p), nil
	}
	n, err := r.file.Write(p)
	if r.translator != nil && n > 0 {
		r.observe(p[:n])
	}
	return n, err
}

func (r *retainedTranscript) consume(events []transcript.Event) {
	for _, event := range events {
		switch event.Kind {
		case transcript.KindToolCall:
			// The emitted result event carries the tool NAME but not its input,
			// so the command text is remembered here and paired in arrival
			// order. Reviews run their tools sequentially, so order is the
			// pairing the stream actually supports.
			r.pending = append(r.pending, event.InputDigest)
		case transcript.KindToolResult:
			command := ""
			if len(r.pending) > 0 {
				command = r.pending[0]
				r.pending = r.pending[1:]
			}
			bucket := classifyPhaseCommand(command)
			r.bucketMS[bucket] += event.Duration.Milliseconds()
			r.bucketCount[bucket]++
			r.commands++
		}
	}
}

// Close flushes the translator, emits one bounded profile row, and closes the
// file. One row per job rather than one per command is deliberate: job_events
// already reached ~1.8M rows once from per-tick writes, and the analysis needs
// totals, not a per-command timeline.
func (r *retainedTranscript) Close() error {
	if r == nil {
		return nil
	}
	if r.translator != nil {
		if len(r.partial) > 0 {
			r.consume(r.translator.Translate(string(r.partial)))
			r.partial = nil
		}
		r.consume(r.translator.Flush())
		r.emit()
	}
	if r.file == nil {
		return nil
	}
	return r.file.Close()
}

func (r *retainedTranscript) emit() {
	if r.store == nil || strings.TrimSpace(r.jobID) == "" {
		return
	}
	wall := time.Since(r.started)
	var observed int64
	for _, ms := range r.bucketMS {
		observed += ms
	}
	coverage := phaseCoverageDecomposed
	if !runtimeEmitsToolEvents(r.runtime) {
		coverage = phaseCoverageOpaque
	}
	residual := wall.Milliseconds() - observed
	if residual < 0 {
		// Tool durations are measured from arrival and can overlap if a runtime
		// ever reports concurrent tools. Clamp rather than emit a negative, and
		// never let the clamp inflate a bucket.
		residual = 0
	}
	profile := phaseProfile{
		Coverage:    coverage,
		Runtime:     strings.TrimSpace(r.runtime),
		WallMS:      wall.Milliseconds(),
		ResidualMS:  residual,
		Commands:    r.commands,
		BucketMS:    r.bucketMS,
		BucketCount: r.bucketCount,
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

var _ io.Writer = (*retainedTranscript)(nil)

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

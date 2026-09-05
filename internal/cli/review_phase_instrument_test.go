package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/transcript"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// #1824. The instrument's whole purpose is to answer where a review's wall time
// goes, so these tests attack the two ways such an instrument lies: it can
// misattribute time to the wrong bucket, and it can report an EMPTY
// decomposition that reads like a measurement when it is really blindness.

func TestClassifyPhaseCommandReadsTheLeadingCommandNotTheWholeString(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command string
		want    string
	}{
		{"go test", "go test ./... -count=1", phaseBucketTest},
		{"go build", "go build -buildvcs=false ./...", phaseBucketBuild},
		{"go vet is build-side", "go vet ./internal/cli/", phaseBucketBuild},
		{"git", "git rev-parse HEAD", phaseBucketVCS},
		{"gh", "gh pr view 1824 --json state", phaseBucketVCS},
		// The trap: a vcs command whose ARGUMENTS name a test command. A
		// substring search anywhere in the line would bill this to test and
		// would silently inflate exactly the bucket under investigation.
		{"git commit mentioning go test", `git commit -m "go test is slow"`, phaseBucketVCS},
		{"env prefix before go test", "GOFLAGS=-mod=mod GOCACHE=/tmp/x go test ./...", phaseBucketTest},
		{"shell wrapper before go test", `bash -c "go test ./..."`, phaseBucketTest},
		{"absolute path to go", "/root/.local/toolchains/go1.26.4/bin/go test ./...", phaseBucketTest},
		{"go run is neither", "go run ./cmd/gitmoot", phaseBucketOther},
		{"unrelated", "sleep 30", phaseBucketOther},
		{"empty", "   ", phaseBucketOther},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyPhaseCommand(tc.command); got != tc.want {
				t.Fatalf("classifyPhaseCommand(%q) = %q, want %q", tc.command, got, tc.want)
			}
		})
	}
}

// codexToolStream renders a codex command exchange in the REAL shape, taken
// from internal/transcript/testdata/codex_tool_run.jsonl: item.started opens
// the command and item.completed closes it, and the command arrives wrapped as
// `/bin/bash -lc '...'`. An invented shape produced three results with no
// calls, which billed every command to other - the fixture lied, not the code.
// codexToolStream keeps the single-write form for tests that do not need a
// measurable duration.
func codexToolStream(callID, command string) string {
	wrapped := fmt.Sprintf("/bin/bash -lc %q", command)
	line := func(kind, status string, exit any) string {
		encoded, _ := json.Marshal(map[string]any{
			"type": kind,
			"item": map[string]any{
				"id": callID, "type": "command_execution", "command": wrapped,
				"aggregated_output": "", "exit_code": exit, "status": status,
			},
		})
		return string(encoded) + "\n"
	}
	return line("item.started", "in_progress", nil) + line("item.completed", "completed", 0)
}

// codexToolLines returns the started and completed lines separately so a test
// can put REAL elapsed time between them: the translator measures a tool's
// duration from arrival, so the gap between these two writes is the duration
// under test.
func codexToolLines(callID, command string) (string, string) {
	wrapped := fmt.Sprintf("/bin/bash -lc %q", command)
	line := func(kind, status string, exit any) string {
		encoded, _ := json.Marshal(map[string]any{
			"type": kind,
			"item": map[string]any{
				"id": callID, "type": "command_execution", "command": wrapped,
				"aggregated_output": "", "exit_code": exit, "status": status,
			},
		})
		return string(encoded) + "\n"
	}
	return line("item.started", "in_progress", nil), line("item.completed", "completed", 0)
}

func openInstrumentedTranscript(t *testing.T, home, jobID, runtimeName string, store *db.Store) *retainedTranscript {
	t.Helper()
	handle, err := openRetainedTranscriptLog(home, jobID, runtimeName, store)
	if err != nil {
		t.Fatalf("open retained transcript: %v", err)
	}
	if handle == nil {
		t.Fatal("retained transcript is nil with capture enabled")
	}
	return handle
}

func seedInstrumentJob(t *testing.T, paths config.Paths, jobID, agent, runtimeName string) *db.Store {
	t.Helper()
	if err := os.MkdirAll(paths.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.UpsertAgent(context.Background(), db.Agent{
		Name: agent, Role: "reviewer", Runtime: runtimeName, RepoScope: "gitmoot/gitmoot",
	}); err != nil {
		t.Fatalf("upsert agent: %v", err)
	}
	payload, err := json.Marshal(workflow.JobPayload{Repo: "gitmoot/gitmoot", PullRequest: 1824})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateJobWithEvent(context.Background(), db.Job{
		ID: jobID, Agent: agent, Type: "review",
		State: string(workflow.JobRunning), Payload: string(payload),
	}, db.JobEvent{Kind: "running", Message: "dispatched"}); err != nil {
		t.Fatalf("create job: %v", err)
	}
	return store
}

func readPhaseProfile(t *testing.T, store *db.Store, jobID string) phaseProfile {
	t.Helper()
	events, err := store.ListJobEvents(context.Background(), jobID)
	if err != nil {
		t.Fatalf("list job events: %v", err)
	}
	var found []string
	for _, event := range events {
		if event.Kind == phaseProfileEventKind {
			found = append(found, event.Message)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one %s event, got %d (%v)", phaseProfileEventKind, len(found), found)
	}
	var profile phaseProfile
	if err := json.Unmarshal([]byte(found[0]), &profile); err != nil {
		t.Fatalf("decode profile %q: %v", found[0], err)
	}
	return profile
}

// TestPhaseInstrumentNeverAltersTheTranscriptBytes is the safety test. The
// instrument observes a stream that is retained evidence; if it can drop,
// reorder or duplicate a byte it is worse than having no instrument.
func TestPhaseInstrumentNeverAltersTheTranscriptBytes(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	store := seedInstrumentJob(t, paths, "instrumented", "codex-reviewer", "codex")

	stream := codexToolStream("c1", "go test ./...") +
		codexToolStream("c2", "git status --porcelain") +
		"a trailing line with no newline"

	handle := openInstrumentedTranscript(t, home, "instrumented", "codex", store)
	// Write in awkward chunks so line reassembly is exercised: a split inside a
	// JSON line is the case a naive per-Write parser corrupts.
	for i := 0; i < len(stream); i += 7 {
		end := i + 7
		if end > len(stream) {
			end = len(stream)
		}
		if _, err := handle.Write([]byte(stream[i:end])); err != nil {
			t.Fatalf("write chunk: %v", err)
		}
	}
	if err := handle.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	onDisk, err := os.ReadFile(filepath.Join(paths.Logs, "jobs", "instrumented.log"))
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	if string(onDisk) != stream {
		t.Fatalf("transcript bytes changed:\n got %q\nwant %q", string(onDisk), stream)
	}
}

// TestPhaseInstrumentTagsAnOpaqueRuntimeRatherThanReportingZeroCommands is the
// test that matters most to the ANALYSIS. Claude emits no tool events at all,
// so an untagged claude profile is indistinguishable from a run that executed
// nothing - and claude is a quarter of review runs at roughly twice codex's
// median, so the misreading would land on the slow half of the population.
func TestPhaseInstrumentTagsAnOpaqueRuntimeRatherThanReportingZeroCommands(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	store := seedInstrumentJob(t, paths, "opaque", "claude-reviewer", "claude")

	handle := openInstrumentedTranscript(t, home, "opaque", "claude", store)
	if _, err := handle.Write([]byte(`{"type":"result","result":"done","duration_ms":1234}` + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := handle.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	profile := readPhaseProfile(t, store, "opaque")
	if profile.Coverage != phaseCoverageOpaque {
		t.Fatalf("coverage = %q, want %q - an empty decomposition MUST be marked as blindness", profile.Coverage, phaseCoverageOpaque)
	}
	if profile.Commands != 0 {
		t.Fatalf("commands = %d, want 0 for a runtime with no tool stream", profile.Commands)
	}
	if profile.Runtime != "claude" {
		t.Fatalf("runtime = %q, want claude", profile.Runtime)
	}
}

// TestPhaseInstrumentAttributesCommandsAndLeavesTheRestAsResidual is the
// arithmetic contract: buckets plus residual equal the run, so a dominant
// residual is visible rather than distributed away. Refuting the
// mutation-cost premise depends on residual being trustworthy.
//
// The first version of this test was VACUOUS: it wrote both ends of each
// command in the same instant, so every bucket was 0ms and the wall was 0ms,
// and `buckets + residual == wall` held as 0 == 0. A mutant that hard-coded
// residual to zero survived it. Real elapsed time is now forced on BOTH sides
// of the equation - inside a command and outside every command - so each term
// is non-zero and the identity has something to constrain.
func TestPhaseInstrumentAttributesCommandsAndLeavesTheRestAsResidual(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	store := seedInstrumentJob(t, paths, "decomposed", "codex-reviewer", "codex")

	handle := openInstrumentedTranscript(t, home, "decomposed", "codex", store)
	// Durations are measured from arrival, so the gap between the started and
	// completed lines IS the command's measured time.
	for i, command := range []string{"go test ./internal/cli/", `git commit -m "go test"`} {
		started, completed := codexToolLines(fmt.Sprintf("c%d", i), command)
		if _, err := handle.Write([]byte(started)); err != nil {
			t.Fatalf("write start: %v", err)
		}
		time.Sleep(40 * time.Millisecond)
		if _, err := handle.Write([]byte(completed)); err != nil {
			t.Fatalf("write completion: %v", err)
		}
	}
	// Time with no command in flight is what residual must capture.
	time.Sleep(60 * time.Millisecond)
	if err := handle.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	profile := readPhaseProfile(t, store, "decomposed")
	if profile.Coverage != phaseCoverageDecomposed {
		t.Fatalf("coverage = %q, want %q", profile.Coverage, phaseCoverageDecomposed)
	}
	var summed int64
	for _, name := range sortedBuckets(profile.BucketMS) {
		summed += profile.BucketMS[name]
	}
	// Guard the guard: if any term is zero the identity below proves nothing.
	if summed <= 0 || profile.ResidualMS <= 0 || profile.WallMS <= 0 {
		t.Fatalf("vacuous measurement - buckets=%d residual=%d wall=%d must all be non-zero (profile: %+v)",
			summed, profile.ResidualMS, profile.WallMS, profile)
	}
	if summed+profile.ResidualMS != profile.WallMS {
		t.Fatalf("buckets(%d) + residual(%d) = %d, want wall %d - the partition must be exact",
			summed, profile.ResidualMS, summed+profile.ResidualMS, profile.WallMS)
	}
	if profile.BucketMS[phaseBucketTest] <= 0 {
		t.Fatalf("test bucket = %dms, want the measured command time (profile: %+v)", profile.BucketMS[phaseBucketTest], profile)
	}
	// The vcs command must not have been billed to test, which is the
	// misattribution that would fake the very result being investigated.
	if profile.BucketCount[phaseBucketVCS] != 1 {
		t.Fatalf("vcs count = %d, want 1 (profile: %+v)", profile.BucketCount[phaseBucketVCS], profile)
	}
}

// TestPhaseInstrumentCloseWithoutAStoreDoesNotPanic defends the emit guard
// itself. The storeless path above is protected by the CONSTRUCTOR returning
// before a translator is attached, so it never reaches emit and cannot notice
// if emit's own nil check is removed. This drives that line directly.
func TestPhaseInstrumentCloseWithoutAStoreDoesNotPanic(t *testing.T) {
	handle := &retainedTranscript{
		jobID:       "no-store",
		runtime:     "codex",
		translator:  opaqueTranslator{},
		started:     time.Now(),
		bucketMS:    map[string]int64{},
		bucketCount: map[string]int{},
	}
	if err := handle.Close(); err != nil {
		t.Fatalf("close with no store: %v", err)
	}
}

// TestPhaseInstrumentWithoutAStoreWritesNoEventAndStillRetains proves the
// capture path is unchanged where there is no daemon store to write to: the
// transcript is still retained, and nothing is emitted.
func TestPhaseInstrumentWithoutAStoreWritesNoEventAndStillRetains(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)

	handle := openInstrumentedTranscript(t, home, "storeless", "codex", nil)
	if _, err := handle.Write([]byte("stdout stream\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := handle.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	onDisk, err := os.ReadFile(filepath.Join(paths.Logs, "jobs", "storeless.log"))
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	if strings.TrimSpace(string(onDisk)) != "stdout stream" {
		t.Fatalf("transcript = %q, want the written bytes", string(onDisk))
	}
}

// BenchmarkRetainedTranscriptWrite measures the instrument's cost on the write
// path, which is the only hot path it touches. The design row committed to
// measuring overhead rather than assuming it: "instrumented" parses every line,
// "bare" is the same handle with no translator, which is byte-for-byte the
// pre-#1824 behavior.
func BenchmarkRetainedTranscriptWrite(b *testing.B) {
	started, completed := codexToolLines("bench", "go test ./internal/cli/")
	payload := []byte(started + completed)
	for _, tc := range []struct {
		name       string
		translator transcript.Translator
	}{
		{"bare", nil},
		{"instrumented", mustBenchTranslator(b)},
	} {
		b.Run(tc.name, func(b *testing.B) {
			file, err := os.CreateTemp(b.TempDir(), "transcript-*.log")
			if err != nil {
				b.Fatal(err)
			}
			defer func() { _ = file.Close() }()
			handle := &retainedTranscript{
				file: file, translator: tc.translator, started: time.Now(),
				bucketMS: map[string]int64{}, bucketCount: map[string]int{},
			}
			b.SetBytes(int64(len(payload)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := handle.Write(payload); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func mustBenchTranslator(b *testing.B) transcript.Translator {
	b.Helper()
	translator, err := transcript.NewTranslator("codex")
	if err != nil {
		b.Fatal(err)
	}
	return translator
}

// TestEffectiveTranscriptRuntimePrefersThePayloadOverride pins the precedence
// the store-backed resolver already uses. It matters to the PROFILE, not just
// to tidiness: a claude-registered agent dispatched with a codex override
// emits a codex tool stream, and reading the registered runtime instead would
// tag that run opaque_runtime and discard a decomposition that is really there.
func TestEffectiveTranscriptRuntimePrefersThePayloadOverride(t *testing.T) {
	agent := runtime.Agent{Name: "gm-review-opus", Runtime: "claude"}
	if got := effectiveTranscriptRuntime(workflow.JobPayload{RuntimeOverride: "codex"}, agent); got != "codex" {
		t.Fatalf("runtime = %q, want the payload override codex", got)
	}
	if got := effectiveTranscriptRuntime(workflow.JobPayload{}, agent); got != "claude" {
		t.Fatalf("runtime = %q, want the registered runtime claude", got)
	}
	if got := effectiveTranscriptRuntime(workflow.JobPayload{RuntimeOverride: "   "}, agent); got != "claude" {
		t.Fatalf("runtime = %q, want blank override to fall through to claude", got)
	}
}

package cli

import (
	"encoding/json"
	"errors"
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

// #1824. The instrument's purpose is to answer where a review's wall time goes,
// so these tests attack the ways such an instrument lies: it can misattribute
// time to the wrong bucket or the wrong command, it can report a partition that
// does not hold, and it can report an EMPTY decomposition that reads like a
// measurement when it is really blindness.

func TestClassifyPhaseCommandClassifiesEverySegment(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command string
		want    string
	}{
		{"go test", "go test ./... -count=1", phaseBucketTest},
		{"go build", "go build -buildvcs=false ./...", phaseBucketBuild},
		{"go vet is build-side", "go vet ./internal/cli/", phaseBucketBuild},
		{"git", "git rev-parse HEAD", phaseBucketVCS},
		{"gh", "gh pr view 1930 --json state", phaseBucketVCS},
		// A vcs command whose ARGUMENTS name a test command: a substring
		// search anywhere in the line would bill this to test and inflate
		// exactly the bucket under investigation.
		{"git commit mentioning go test", `git commit -m "go test is slow"`, phaseBucketVCS},
		{"env prefix", "GOFLAGS=-mod=mod GOCACHE=/tmp/x go test ./...", phaseBucketTest},
		{"shell wrapper", `bash -c "go test ./..."`, phaseBucketTest},
		{"absolute interpreter", "/bin/bash -lc 'go test ./...'", phaseBucketTest},
		{"absolute go", "/root/.local/toolchains/go1.26.4/bin/go test ./...", phaseBucketTest},
		// #1824 review F3, all three measured by the reviewer.
		{"cd prefix is plumbing", "cd /root/gm-1824 && go test ./...", phaseBucketTest},
		// #1824 review F8: all three were classified `other`, and the first is
		// this repository's own documented gate shape - so the bucket under
		// investigation was the one being undercounted.
		{"timeout consumes its duration", "timeout 25m go test ./...", phaseBucketTest},
		{"timeout with seconds", "timeout 90s go build ./...", phaseBucketBuild},
		{"timeout with a bare number", "timeout 600 go test ./...", phaseBucketTest},
		{"timeout with a signal flag", "timeout -s KILL -k 10s 25m go test ./...", phaseBucketTest},
		{"time -p takes no value", "time -p go test ./...", phaseBucketTest},
		{"gnu time -f takes a value", `time -f %e go test ./...`, phaseBucketTest},
		{"sudo -u consumes the user", "sudo -u nobody go test ./...", phaseBucketTest},
		{"nice -n consumes the level", "nice -n 5 go test ./...", phaseBucketTest},
		{"wrapper must not swallow the command", "timeout 25m git status", phaseBucketVCS},
		{"a wrapper alone is not a phase", "timeout 25m", phaseBucketOther},
		// #1930 review F12: whitespace-only splitting consumed just the first
		// quoted word of a flag value, leaving its tail looking like the
		// wrapped command - which classified as other.
		// #1930 review f19, all seven measured by the reviewer: `N>&M` is ONE
		// token (a file-descriptor duplication), not two commands joined by a
		// background operator, and `2>&1` is the most common suffix on a real
		// test command. My quote-aware rewrite flushed on any unquoted '&',
		// so every one of these returned mixed.
		{"stderr redirect", "go test ./... 2>&1", phaseBucketTest},
		{"stdout to null plus stderr redirect", "go build ./... > /dev/null 2>&1", phaseBucketBuild},
		{"redirect then pipe", "go test ./... 2>&1 | tee test.log", phaseBucketTest},
		{"stdout into stderr", "go test ./... 1>&2", phaseBucketTest},
		{"nested shell with redirects", `bash -c "go test ./..." > test.log 2>&1`, phaseBucketTest},
		{"combined redirect", "go test ./... &> test.log", phaseBucketTest},
		{"backgrounded command still classifies", "go test ./... &", phaseBucketTest},
		// And the operator must still separate when it really is one.
		{"real background-and-then sequence", "go build ./... && go test ./...", phaseBucketMixed},
		{"quoted multi-word time format", `time -f "%e %M" go test ./...`, phaseBucketTest},
		{"quoted multi-word sudo user", `sudo -u "display name" go test ./...`, phaseBucketTest},
		{"quoted value with single quotes", `time -f '%e %M' go build ./...`, phaseBucketBuild},
		{"nested shell command string", `bash -c "cd repo && go test ./..."`, phaseBucketTest},
		{"nested shell with a mixed body", `bash -c "go test ./... && go build ./..."`, phaseBucketMixed},
		{"time wrapper", "time go test ./...", phaseBucketTest},
		{"test AND build is mixed, not test", "go test ./... && go build ./...", phaseBucketMixed},
		{"build then vcs is mixed", "go build ./... && git status", phaseBucketMixed},
		{"same phase twice stays that phase", "go test ./internal/cli/ && go test ./internal/db/", phaseBucketTest},
		{"pipe into grep keeps the phase", "go test ./... | grep -E FAIL", phaseBucketTest},
		{"semicolon sequencing", "cd repo; git status", phaseBucketVCS},
		// #1824 review F2: kimi sends function arguments as JSON.
		{"kimi json payload", `{"command":"go test ./..."}`, phaseBucketTest},
		{"kimi json argv payload", `{"command":["go","build","./..."]}`, phaseBucketBuild},
		{"kimi json unrelated payload", `{"path":"/tmp/x"}`, phaseBucketOther},
		{"go run is neither", "go run ./cmd/gitmoot", phaseBucketOther},
		{"unrelated", "sleep 30", phaseBucketOther},
		{"pure plumbing", "cd /root/gm-1824", phaseBucketOther},
		{"empty", "   ", phaseBucketOther},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyPhaseCommand(tc.command); got != tc.want {
				t.Fatalf("classifyPhaseCommand(%q) = %q, want %q", tc.command, got, tc.want)
			}
		})
	}
}

func TestExtractToolCommandReadsJSONAndBarePayloads(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"bare", "go test ./...", "go test ./..."},
		{"json command", `{"command":"go test ./..."}`, "go test ./..."},
		{"json argv", `{"command":["go","test","./..."]}`, "go test ./..."},
		{"json alternate key", `{"cmd":"git status"}`, "git status"},
		{"malformed json stays raw", `{"command":`, `{"command":`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractToolCommand(tc.input); got != tc.want {
				t.Fatalf("extractToolCommand(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// codexToolLines returns the started and completed lines separately so a test
// can put REAL elapsed time between them, and can INTERLEAVE two tools to
// reproduce the overlap that broke arrival-order pairing.
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
	handle, err := openRetainedTranscriptLog(home, jobID, "review", runtimeName, 0, store)
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
	if err := store.UpsertAgent(t.Context(), db.Agent{
		Name: agent, Role: "reviewer", Runtime: runtimeName, RepoScope: "gitmoot/gitmoot",
	}); err != nil {
		t.Fatalf("upsert agent: %v", err)
	}
	payload, err := json.Marshal(workflow.JobPayload{Repo: "gitmoot/gitmoot", PullRequest: 1824})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateJobWithEvent(t.Context(), db.Job{
		ID: jobID, Agent: agent, Type: "review",
		State: string(workflow.JobRunning), Payload: string(payload),
	}, db.JobEvent{Kind: "running", Message: "dispatched"}); err != nil {
		t.Fatalf("create job: %v", err)
	}
	return store
}

func readPhaseProfiles(t *testing.T, store *db.Store, jobID string) []phaseProfile {
	t.Helper()
	events, err := store.ListJobEvents(t.Context(), jobID)
	if err != nil {
		t.Fatalf("list job events: %v", err)
	}
	var profiles []phaseProfile
	for _, event := range events {
		if event.Kind != phaseProfileEventKind {
			continue
		}
		var profile phaseProfile
		if err := json.Unmarshal([]byte(event.Message), &profile); err != nil {
			t.Fatalf("decode profile %q: %v", event.Message, err)
		}
		profiles = append(profiles, profile)
	}
	return profiles
}

func readPhaseProfile(t *testing.T, store *db.Store, jobID string) phaseProfile {
	t.Helper()
	profiles := readPhaseProfiles(t, store, jobID)
	if len(profiles) != 1 {
		t.Fatalf("want exactly one %s event, got %d", phaseProfileEventKind, len(profiles))
	}
	return profiles[0]
}

func assertProfileIdentities(t *testing.T, profile phaseProfile) {
	t.Helper()
	if profile.CoveredMS+profile.ResidualMS != profile.WallMS {
		t.Fatalf("covered(%d) + residual(%d) = %d, want wall %d - this identity is exact",
			profile.CoveredMS, profile.ResidualMS, profile.CoveredMS+profile.ResidualMS, profile.WallMS)
	}
	if profile.ResidualMS < 0 || profile.CoveredMS < 0 || profile.OverlapMS < 0 {
		t.Fatalf("negative component in %+v", profile)
	}
	var summed int64
	for _, name := range sortedBuckets(profile.BucketMS) {
		summed += profile.BucketMS[name]
	}
	// Every field is rounded to whole milliseconds independently, so this
	// identity holds up to that rounding: at most half a millisecond per
	// bucket plus one for the union.
	tolerance := int64(len(profile.BucketMS) + 1)
	if delta := summed - (profile.CoveredMS + profile.OverlapMS); delta > tolerance || delta < -tolerance {
		t.Fatalf("sum(buckets)=%d, want covered(%d) + overlap(%d) = %d within %dms rounding",
			summed, profile.CoveredMS, profile.OverlapMS, profile.CoveredMS+profile.OverlapMS, tolerance)
	}
}

// TestPhaseInstrumentPairsOverlappingCommandsByToolID is #1824 review F1. Two
// commands run concurrently and the SECOND one finishes first. Arrival-order
// pairing gave each result the other's command, reporting the durations
// swapped, and the sum of durations exceeded the run's wall time - which the
// old code hid by clamping residual to zero.
func TestPhaseInstrumentPairsOverlappingCommandsByToolID(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	store := seedInstrumentJob(t, paths, "overlap", "codex-reviewer", "codex")
	handle := openInstrumentedTranscript(t, home, "overlap", "codex", store)

	longStart, longEnd := codexToolLines("long", "go test ./...")
	shortStart, shortEnd := codexToolLines("short", "git status --porcelain")

	// long opens first and closes LAST; short opens second and closes FIRST.
	if _, err := handle.Write([]byte(longStart)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	if _, err := handle.Write([]byte(shortStart)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if _, err := handle.Write([]byte(shortEnd)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	if _, err := handle.Write([]byte(longEnd)); err != nil {
		t.Fatal(err)
	}
	// An IDLE GAP is load-bearing here, not padding. With overlap but no idle
	// time, covered ~= wall and the broken formula residual = wall - SUM
	// clamps to zero while the identity still appears to hold. The gap forces
	// covered < wall, so a residual computed from the sum can no longer
	// coincide with one computed from the union - which is what makes this
	// test able to kill that mutant at all.
	time.Sleep(40 * time.Millisecond)
	if err := handle.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	profile := readPhaseProfile(t, store, "overlap")
	assertProfileIdentities(t, profile)
	if profile.ResidualMS <= 0 {
		t.Fatalf("residual_ms = %d, want positive - the idle gap is what distinguishes union-based residual from sum-based (%+v)", profile.ResidualMS, profile)
	}
	// The long-lived command is `go test`; the short one is `git status`. If
	// results are paired by arrival order these two are swapped.
	if profile.BucketMS[phaseBucketTest] <= profile.BucketMS[phaseBucketVCS] {
		t.Fatalf("test(%dms) must exceed vcs(%dms) - durations look swapped (profile %+v)",
			profile.BucketMS[phaseBucketTest], profile.BucketMS[phaseBucketVCS], profile)
	}
	if profile.OverlapMS <= 0 {
		t.Fatalf("overlap_ms = %d, want positive - these commands overlap by construction (profile %+v)", profile.OverlapMS, profile)
	}
	if profile.Unpaired != 0 {
		t.Fatalf("unpaired = %d, want 0 - both results carry a matching call id", profile.Unpaired)
	}
}

// TestPhaseInstrumentNeverAltersTheTranscriptBytes is the safety test.
func TestPhaseInstrumentNeverAltersTheTranscriptBytes(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	store := seedInstrumentJob(t, paths, "instrumented", "codex-reviewer", "codex")

	start, end := codexToolLines("c1", "go test ./...")
	stream := start + end + "a trailing line with no newline"

	handle := openInstrumentedTranscript(t, home, "instrumented", "codex", store)
	// Awkward chunk boundaries: a split inside a JSON line is the case a naive
	// per-Write parser corrupts.
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

// truncatingWriter persists a fixed prefix and then fails permanently. It is
// the sink #1824 review F4 needed: the instrument must observe EXACTLY what
// reached storage, and only a sink that refuses the REST can tell observing p
// apart from observing p[:n]. A sink that merely short-writes and is then
// retried cannot: the retry re-observes the remainder, which masks the defect.
type truncatingWriter struct {
	persisted []byte
	limit     int
}

func (w *truncatingWriter) Write(p []byte) (int, error) {
	remaining := w.limit - len(w.persisted)
	if remaining <= 0 {
		return 0, errors.New("sink closed")
	}
	n := len(p)
	if n > remaining {
		n = remaining
	}
	w.persisted = append(w.persisted, p[:n]...)
	if n < len(p) {
		return n, errors.New("short write")
	}
	return n, nil
}

// TestPhaseInstrumentObservesOnlyPersistedBytes is #1824 review F4. The sink
// accepts the command's START line and nothing more, so the completion never
// reaches storage. A profiler that observes p rather than p[:n] counts a
// command whose bytes are not in the transcript - describing a run that was
// never recorded.
func TestPhaseInstrumentObservesOnlyPersistedBytes(t *testing.T) {
	translator, err := transcript.NewTranslator("codex")
	if err != nil {
		t.Fatal(err)
	}
	start, end := codexToolLines("c1", "go test ./...")
	sink := &truncatingWriter{limit: len(start)}
	handle := &retainedTranscript{
		sink: sink, translator: translator, started: time.Now(),
		pending: map[string]pendingCommand{}, nonShellCalls: map[string]struct{}{}, bucketNS: map[string]int64{}, bucketCount: map[string]int{},
	}

	n, writeErr := handle.Write([]byte(start + end))
	if writeErr == nil {
		t.Fatal("want a short-write error from the truncating sink")
	}
	if n != len(start) {
		t.Fatalf("persisted %d bytes, want the %d-byte start line", n, len(start))
	}
	if string(sink.persisted) != start {
		t.Fatalf("sink holds %q, want only the start line", string(sink.persisted))
	}
	// The completion line never reached storage, so no command may be counted.
	if handle.commands != 0 {
		t.Fatalf("commands = %d, want 0 - the completion was never persisted, so counting it means observing unpersisted bytes", handle.commands)
	}
	if len(handle.pending) != 1 {
		t.Fatalf("pending calls = %d, want the one started command still open", len(handle.pending))
	}
}

// TestPhaseInstrumentCountsACommandWhoseStreamEndsWithoutANewline is the
// other #1824 review F4 mutant: dropping the final unterminated line silently
// short-measures a run whose last command is the expensive one.
func TestPhaseInstrumentCountsACommandWhoseStreamEndsWithoutANewline(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	store := seedInstrumentJob(t, paths, "unterminated", "codex-reviewer", "codex")
	handle := openInstrumentedTranscript(t, home, "unterminated", "codex", store)

	start, end := codexToolLines("c1", "go test ./...")
	if _, err := handle.Write([]byte(start)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	// The completion arrives with NO trailing newline.
	if _, err := handle.Write([]byte(strings.TrimSuffix(end, "\n"))); err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	profile := readPhaseProfile(t, store, "unterminated")
	if profile.Commands != 1 {
		t.Fatalf("commands = %d, want 1 - the final unterminated line is a real result (profile %+v)", profile.Commands, profile)
	}
	if profile.BucketMS[phaseBucketTest] <= 0 {
		t.Fatalf("test bucket = %dms, want the measured time (profile %+v)", profile.BucketMS[phaseBucketTest], profile)
	}
}

// TestPhaseInstrumentTagsAnOpaqueRuntimeRatherThanReportingZeroCommands is the
// test that matters most to the ANALYSIS.
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
	assertProfileIdentities(t, profile)
}

// TestPhaseInstrumentPartitionsWallTimeExactly drives the identities with real
// elapsed time on every term. An earlier version of this test was VACUOUS: it
// wrote both ends of each command in the same instant, so every term was 0ms
// and the identity held as 0 == 0 while a hard-coded-zero mutant survived.
func TestPhaseInstrumentPartitionsWallTimeExactly(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	store := seedInstrumentJob(t, paths, "decomposed", "codex-reviewer", "codex")

	handle := openInstrumentedTranscript(t, home, "decomposed", "codex", store)
	for i, command := range []string{"go test ./internal/cli/", `git commit -m "go test"`} {
		start, end := codexToolLines(fmt.Sprintf("c%d", i), command)
		if _, err := handle.Write([]byte(start)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(40 * time.Millisecond)
		if _, err := handle.Write([]byte(end)); err != nil {
			t.Fatal(err)
		}
		// Gap with no command in flight: this is what residual must capture.
		time.Sleep(30 * time.Millisecond)
	}
	if err := handle.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	profile := readPhaseProfile(t, store, "decomposed")
	assertProfileIdentities(t, profile)
	// Guard the guard: a zero term proves nothing.
	if profile.CoveredMS <= 0 || profile.ResidualMS <= 0 || profile.WallMS <= 0 {
		t.Fatalf("vacuous measurement - covered=%d residual=%d wall=%d must all be non-zero (%+v)",
			profile.CoveredMS, profile.ResidualMS, profile.WallMS, profile)
	}
	// Sequential commands cannot overlap.
	if profile.OverlapMS != 0 {
		t.Fatalf("overlap_ms = %d, want 0 for strictly sequential commands (%+v)", profile.OverlapMS, profile)
	}
	if profile.BucketCount[phaseBucketVCS] != 1 {
		t.Fatalf("vcs count = %d, want 1 (%+v)", profile.BucketCount[phaseBucketVCS], profile)
	}
	if profile.BucketMS[phaseBucketTest] <= 0 {
		t.Fatalf("test bucket = %dms, want the measured command time (%+v)", profile.BucketMS[phaseBucketTest], profile)
	}
}

// TestPhaseInstrumentSeparatesNonShellToolsFromCommands is #1824 review F2's
// second half: a file_change is a tool event, not a command, and counting it as
// one inflates the command count with rows the buckets cannot describe.
func TestPhaseInstrumentSeparatesNonShellToolsFromCommands(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	store := seedInstrumentJob(t, paths, "tools", "codex-reviewer", "codex")
	handle := openInstrumentedTranscript(t, home, "tools", "codex", store)

	fileChange := func(kind, status string) string {
		encoded, _ := json.Marshal(map[string]any{
			"type": kind,
			"item": map[string]any{
				"id": "f1", "type": "file_change", "status": status,
				"changes": []any{map[string]any{"path": "x.go", "kind": "modify"}},
			},
		})
		return string(encoded) + "\n"
	}
	if _, err := handle.Write([]byte(fileChange("item.started", "in_progress"))); err != nil {
		t.Fatal(err)
	}
	if _, err := handle.Write([]byte(fileChange("item.completed", "completed"))); err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	profile := readPhaseProfile(t, store, "tools")
	if profile.Commands != 0 {
		t.Fatalf("commands = %d, want 0 - a file_change is not a shell command (%+v)", profile.Commands, profile)
	}
	if profile.ToolEvents != 1 {
		t.Fatalf("tool_events = %d, want 1 (%+v)", profile.ToolEvents, profile)
	}
	assertProfileIdentities(t, profile)
}

// TestPhaseInstrumentTagsEachAttemptSeparately is #1824 review F5. RetryJob
// preserves prior job_events and re-delivers the same job id, so two profiles
// for one job id are expected - and must be distinguishable, or a retried
// review is double-counted with no attempt boundary.
func TestPhaseInstrumentTagsEachAttemptSeparately(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	store := seedInstrumentJob(t, paths, "retried", "codex-reviewer", "codex")

	for attempt := int64(1); attempt <= 2; attempt++ {
		handle, err := openRetainedTranscriptLog(home, "retried", "review", "codex", attempt, store)
		if err != nil {
			t.Fatalf("open attempt %d: %v", attempt, err)
		}
		start, end := codexToolLines(fmt.Sprintf("a%d", attempt), "go test ./...")
		if _, err := handle.Write([]byte(start + end)); err != nil {
			t.Fatal(err)
		}
		if err := handle.Close(); err != nil {
			t.Fatalf("close attempt %d: %v", attempt, err)
		}
	}

	profiles := readPhaseProfiles(t, store, "retried")
	if len(profiles) != 2 {
		t.Fatalf("profiles = %d, want one per attempt", len(profiles))
	}
	if profiles[0].Attempt == profiles[1].Attempt {
		t.Fatalf("both attempts recorded attempt=%d - a retry cannot be told from the first run", profiles[0].Attempt)
	}
}

// TestPhaseInstrumentCloseWithoutAStoreDoesNotPanic defends the emit guard
// itself, which the storeless path cannot reach.
func TestPhaseInstrumentCloseWithoutAStoreDoesNotPanic(t *testing.T) {
	handle := &retainedTranscript{
		jobID: "no-store", runtime: "codex", translator: opaqueTranslator{}, started: time.Now(),
		pending: map[string]pendingCommand{}, nonShellCalls: map[string]struct{}{}, bucketNS: map[string]int64{}, bucketCount: map[string]int{},
	}
	if err := handle.Close(); err != nil {
		t.Fatalf("close with no store: %v", err)
	}
}

// TestPhaseInstrumentWithoutAStoreWritesNoEventAndStillRetains proves the
// capture path is unchanged where there is no daemon store to write to.
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

func TestUnionNSMergesOverlappingIntervals(t *testing.T) {
	base := time.Now()
	at := func(ms int) time.Time { return base.Add(time.Duration(ms) * time.Millisecond) }
	for _, tc := range []struct {
		name      string
		intervals []commandInterval
		want      int64
	}{
		{"empty", nil, 0},
		{"single", []commandInterval{{at(0), at(50)}}, 50},
		{"disjoint", []commandInterval{{at(0), at(20)}, {at(40), at(60)}}, 40},
		{"overlapping", []commandInterval{{at(0), at(50)}, {at(25), at(75)}}, 75},
		{"nested", []commandInterval{{at(0), at(100)}, {at(25), at(50)}}, 100},
		{"out of order input", []commandInterval{{at(40), at(60)}, {at(0), at(20)}}, 40},
		{"touching", []commandInterval{{at(0), at(30)}, {at(30), at(60)}}, 60},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := tc.want * time.Millisecond.Nanoseconds()
			if got := unionNS(tc.intervals); got != want {
				t.Fatalf("unionNS = %d, want %d", got, want)
			}
		})
	}
}

// TestEffectiveTranscriptRuntimePrefersThePayloadOverride pins the precedence
// the store-backed resolver uses: a claude-registered agent dispatched with a
// codex override emits a codex tool stream, and reading the registered runtime
// would tag that run opaque and discard a real decomposition.
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

// BenchmarkRetainedTranscriptWrite measures the instrument's cost on the write
// path, which is the only hot path it touches.
func BenchmarkRetainedTranscriptWrite(b *testing.B) {
	start, end := codexToolLines("bench", "go test ./internal/cli/")
	payload := []byte(start + end)
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
				sink: file, translator: tc.translator, started: time.Now(),
				pending: map[string]pendingCommand{}, nonShellCalls: map[string]struct{}{}, bucketNS: map[string]int64{}, bucketCount: map[string]int{},
			}
			b.SetBytes(int64(len(payload)))
			b.ResetTimer()
			for range b.N {
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

// The three tests below came from CONSTRUCTED ADVERSARIAL INPUTS, not from
// mutating the implementation. That distinction is the point: 13 mutants all
// died against this file while every one of these defects was live, because a
// mutant asks "do the tests notice a change to this code" and only a
// counterexample asks "can the claimed invariant be violated".

// TestPhaseInstrumentKeepsTheFirstCallOnAToolIDCollision. Two concurrent
// commands arriving on the SAME id used to overwrite the pending entry, so the
// first result adopted the second command's text and billed a `go test` run
// wholly to `vcs` - the exact misattribution id pairing was introduced to fix.
func TestPhaseInstrumentKeepsTheFirstCallOnAToolIDCollision(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	store := seedInstrumentJob(t, paths, "collision", "codex-reviewer", "codex")
	handle := openInstrumentedTranscript(t, home, "collision", "codex", store)

	firstStart, firstEnd := codexToolLines("dup", "go test ./...")
	secondStart, secondEnd := codexToolLines("dup", "git status --porcelain")
	if _, err := handle.Write([]byte(firstStart)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	if _, err := handle.Write([]byte(secondStart)); err != nil {
		t.Fatal(err)
	}
	if _, err := handle.Write([]byte(firstEnd + secondEnd)); err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	profile := readPhaseProfile(t, store, "collision")
	assertProfileIdentities(t, profile)
	if profile.IDCollisions != 1 {
		t.Fatalf("id_collisions = %d, want 1 (%+v)", profile.IDCollisions, profile)
	}
	// The surviving call is the `go test` one, so its time must be in test and
	// NOT in vcs.
	if profile.BucketMS[phaseBucketTest] <= 0 {
		t.Fatalf("test bucket = %dms, want the first call's time (%+v)", profile.BucketMS[phaseBucketTest], profile)
	}
	if profile.BucketCount[phaseBucketVCS] != 0 {
		t.Fatalf("vcs count = %d, want 0 - the colliding call must not replace the first (%+v)", profile.BucketCount[phaseBucketVCS], profile)
	}
}

// TestPhaseInstrumentCountsCommandsStillRunningAtClose. A command in flight
// when the transcript closed used to contribute NO bucket time, so its
// duration fell into residual and a review killed mid-`go test` reported as
// almost entirely idle - the opposite of the truth.
func TestPhaseInstrumentCountsCommandsStillRunningAtClose(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	store := seedInstrumentJob(t, paths, "inflight", "codex-reviewer", "codex")
	handle := openInstrumentedTranscript(t, home, "inflight", "codex", store)

	start, _ := codexToolLines("never", "go test ./...")
	if _, err := handle.Write([]byte(start)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if err := handle.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	profile := readPhaseProfile(t, store, "inflight")
	assertProfileIdentities(t, profile)
	if profile.InFlight != 1 || profile.Commands != 1 {
		t.Fatalf("in_flight=%d commands=%d, want 1 and 1 (%+v)", profile.InFlight, profile.Commands, profile)
	}
	if profile.BucketMS[phaseBucketTest] <= 0 {
		t.Fatalf("test bucket = %dms, want the time the command actually ran (%+v)", profile.BucketMS[phaseBucketTest], profile)
	}
	// The whole run was inside one command, so residual must be small - the
	// defect reported it as the entire run.
	if profile.ResidualMS >= profile.BucketMS[phaseBucketTest] {
		t.Fatalf("residual %dms >= command time %dms - in-flight time leaked into idle (%+v)",
			profile.ResidualMS, profile.BucketMS[phaseBucketTest], profile)
	}
}

// TestPhaseInstrumentCapsAnUnterminatedLine. A stream with no newline grew the
// pending buffer without bound: 1 MiB measured, and nothing stops it at 1 GiB
// inside the daemon. The cap must also RECORD the loss, or the profile looks
// complete while silently missing commands.
func TestPhaseInstrumentCapsAnUnterminatedLine(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	store := seedInstrumentJob(t, paths, "flood", "codex-reviewer", "codex")
	handle := openInstrumentedTranscript(t, home, "flood", "codex", store)

	chunk := []byte(strings.Repeat("x", 64*1024))
	for range 24 {
		if _, err := handle.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if len(handle.partial) > maxPartialLineBytes {
		t.Fatalf("pending buffer = %d bytes, want at most %d", len(handle.partial), maxPartialLineBytes)
	}
	if err := handle.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	profile := readPhaseProfile(t, store, "flood")
	if profile.DroppedBytes <= 0 {
		t.Fatalf("dropped_bytes = %d, want the discarded fragment recorded (%+v)", profile.DroppedBytes, profile)
	}
	// The transcript itself must still hold every byte.
	onDisk, err := os.ReadFile(filepath.Join(paths.Logs, "jobs", "flood.log"))
	if err != nil {
		t.Fatal(err)
	}
	if len(onDisk) != 24*64*1024 {
		t.Fatalf("transcript holds %d bytes, want %d - capping the PARSER must never drop a retained byte", len(onDisk), 24*64*1024)
	}
}

// TestPhaseInstrumentReportsAResultWhoseCallWasNeverSeenAsUnpaired is #1824
// review F9. Kimi names a result whose call it never saw "tool", so a
// shell-ness test taken BEFORE the id lookup filed a genuinely lost pairing as
// ordinary tool activity and left unpaired at zero - contradicting what both
// the payload and the documentation say those fields mean.
func TestPhaseInstrumentReportsAResultWhoseCallWasNeverSeenAsUnpaired(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	store := seedInstrumentJob(t, paths, "orphan", "kimi-reviewer", "kimi")
	handle := openInstrumentedTranscript(t, home, "orphan", "kimi", store)

	// A kimi tool result with no preceding assistant tool_call.
	orphan, err := json.Marshal(map[string]any{
		"role": "tool", "tool_call_id": "call_never_opened", "content": "done",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handle.Write(append(orphan, '\n')); err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	profile := readPhaseProfile(t, store, "orphan")
	if profile.Unpaired != 1 {
		t.Fatalf("unpaired = %d, want 1 - the call id was never opened (%+v)", profile.Unpaired, profile)
	}
	if profile.ToolEvents != 0 {
		t.Fatalf("tool_events = %d, want 0 - a lost pairing is not ordinary tool activity (%+v)", profile.ToolEvents, profile)
	}
}

// TestPhaseProfileMillisecondsIsExactOnDeterministicInput is #1824 review F10.
// A +1ms per-bucket bias survived the end-to-end tests because real durations
// are not deterministic, so their tolerance had to be loose. The arithmetic is
// a pure function precisely so it can be pinned on exact nanoseconds.
func TestPhaseProfileMillisecondsIsExactOnDeterministicInput(t *testing.T) {
	ms := time.Millisecond.Nanoseconds()
	buckets := map[string]int64{phaseBucketTest: 40 * ms, phaseBucketVCS: 10 * ms}
	bucketMS, wall, covered, overlap := phaseProfileMilliseconds(buckets, 100*ms, 50*ms, 50*ms)
	if bucketMS[phaseBucketTest] != 40 || bucketMS[phaseBucketVCS] != 10 {
		t.Fatalf("buckets = %v, want exactly {test:40, vcs:10}", bucketMS)
	}
	if wall != 100 || covered != 50 || overlap != 0 {
		t.Fatalf("wall=%d covered=%d overlap=%d, want 100/50/0", wall, covered, overlap)
	}
	if wall-covered != 50 {
		t.Fatalf("residual = %d, want exactly 50", wall-covered)
	}
	// Overlap is the excess of summed command time over the union, exactly.
	if _, _, _, over := phaseProfileMilliseconds(buckets, 100*ms, 30*ms, 50*ms); over != 20 {
		t.Fatalf("overlap = %d, want exactly 20", over)
	}
	// Sub-millisecond remainders round half-up once, not per interval.
	if _, w, _, _ := phaseProfileMilliseconds(nil, 1_500_000, 0, 0); w != 2 {
		t.Fatalf("1.5ms rounded to %d, want 2", w)
	}
	if _, w, _, _ := phaseProfileMilliseconds(nil, 1_400_000, 0, 0); w != 1 {
		t.Fatalf("1.4ms rounded to %d, want 1", w)
	}
	// A negative excess (rounding artefact) must clamp to zero, never negative.
	if _, _, _, over := phaseProfileMilliseconds(nil, 10*ms, 5*ms, 4*ms); over != 0 {
		t.Fatalf("overlap = %d, want 0 rather than a negative", over)
	}
}

// TestPhaseInstrumentResyncsAfterDroppingAnOverLongLine is #1930 review F13.
// After the cap discards a run-on line, the REST of that line is not a line of
// its own. Resuming normal accumulation handed the translator a fragment
// starting mid-token; the following legitimate command must still be measured
// and no spurious command may be invented from the discarded remainder.
func TestPhaseInstrumentResyncsAfterDroppingAnOverLongLine(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	store := seedInstrumentJob(t, paths, "resync", "codex-reviewer", "codex")
	handle := openInstrumentedTranscript(t, home, "resync", "codex", store)

	// A run-on line that trips the cap, then its own terminator, then a real
	// command exchange.
	chunk := []byte(strings.Repeat("x", 64*1024))
	for range 20 {
		if _, err := handle.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if !handle.skipToNewline {
		t.Fatal("cap did not engage: skipToNewline is false")
	}
	if _, err := handle.Write([]byte("tail-of-the-dropped-line\n")); err != nil {
		t.Fatal(err)
	}
	if handle.skipToNewline {
		t.Fatal("still skipping after the dropped line ended")
	}
	start, end := codexToolLines("after", "go test ./...")
	if _, err := handle.Write([]byte(start)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(25 * time.Millisecond)
	if _, err := handle.Write([]byte(end)); err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	profile := readPhaseProfile(t, store, "resync")
	assertProfileIdentities(t, profile)
	if profile.Commands != 1 {
		t.Fatalf("commands = %d, want exactly the one real command after the drop (%+v)", profile.Commands, profile)
	}
	if profile.BucketMS[phaseBucketTest] <= 0 {
		t.Fatalf("test bucket = %dms, want the command that followed the drop (%+v)", profile.BucketMS[phaseBucketTest], profile)
	}
	if profile.DroppedBytes <= 0 {
		t.Fatalf("dropped_bytes = %d, want the discarded run-on line recorded (%+v)", profile.DroppedBytes, profile)
	}
}

// TestShellFieldsKeepsQuotedValuesWhole pins the splitter F12 required.
func TestShellFieldsKeepsQuotedValuesWhole(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		want  []string
	}{
		{"plain", "go test ./...", []string{"go", "test", "./..."}},
		{"double quoted value", `time -f "%e %M" go test`, []string{"time", "-f", "%e %M", "go", "test"}},
		{"single quoted value", `sudo -u 'display name' go test`, []string{"sudo", "-u", "display name", "go", "test"}},
		{"quoted command string", `bash -c "go test ./..."`, []string{"bash", "-c", "go test ./..."}},
		{"unterminated quote keeps the rest", `time -f "%e %M`, []string{"time", "-f", "%e %M"}},
		{"empty", "   ", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := shellFields(tc.input)
			if len(got) != len(tc.want) {
				t.Fatalf("shellFields(%q) = %q, want %q", tc.input, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("shellFields(%q)[%d] = %q, want %q", tc.input, i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestPhaseInstrumentClassifiesThroughTheProductionPath is #1930 review f20's
// real lesson, and it is the one this campaign kept relearning: a test that
// pins the classifier is NOT a test of the path that reaches it.
//
// TestClassifyPhaseCommandClassifiesEverySegment calls classifyPhaseCommand
// directly on raw text. Production never does. The real path is
// retainedTranscript.Write -> translator -> consume -> classifyPhaseCommand,
// and Codex wraps every command in an outer `/bin/bash -lc "..."` whose inner
// quotes arrive BACKSLASH-ESCAPED. Every case below passed the raw table while
// failing through this wrapper, so the table was green on input production
// does not produce.
func TestPhaseInstrumentClassifiesThroughTheProductionPath(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command string
		want    string
	}{
		// The reviewer's measured counterexamples, in its own words.
		{"nested quotes then redirects", `bash -c "go test ./..." >test.log 2>&1`, phaseBucketTest},
		{"redirect after quoted words", `go test ./... -run "TestA|TestB" >out.log 2>&1`, phaseBucketTest},
		{"quoted ampersand", `go test ./... -run "TestA&TestB"`, phaseBucketTest},
		{"escaped ampersand", `go test ./... -run TestA\&TestB`, phaseBucketTest},
		{"ampersand in a comment", "go test ./...  # A&B matters", phaseBucketTest},
		{"escaped ampersand in an env assignment", `PROBE=A\&B go test ./...`, phaseBucketTest},
		// Heredocs are OUTSIDE the declared grammar (ruling 123815): the body
		// and its delimiter are data this lexer does not track, so it refuses
		// rather than classifying the prefix confidently. Previously this
		// asserted `test`, which was a guess that happened to be right.
		{"heredoc is unsupported, not guessed", "go test ./... <<EOF\nA&B\nEOF", phaseBucketUnknown},
		// REPRESENTATIVE UNSUPPORTED CONTEXTS - each must refuse, and each is a
		// construct where the command word itself can differ from the text.
		{"command substitution", "go test $(go list ./...)", phaseBucketUnknown},
		{"parameter expansion", "${TOOL} test ./...", phaseBucketUnknown},
		{"bare parameter", "$TOOL test ./...", phaseBucketUnknown},
		{"arithmetic expansion", "go test -parallel $((N*2)) ./...", phaseBucketUnknown},
		{"backquote substitution", "go test `go list ./...`", phaseBucketUnknown},
		{"process substitution", "diff <(go list ./...) want.txt", phaseBucketUnknown},
		{"subshell", "(cd repo && go test ./...)", phaseBucketUnknown},
		{"arithmetic command", "((count++)) && go test ./...", phaseBucketUnknown},
		{"herestring", "go test ./... <<<data", phaseBucketUnknown},
		{"case terminator", "case x in a) go test ./... ;; esac", phaseBucketUnknown},
		{"unterminated quote", `go test -run "TestA`, phaseBucketUnknown},
		{"trailing line continuation", "go test ./... \\", phaseBucketUnknown},
		{"substitution inside double quotes", `go test -run "$NAME"`, phaseBucketUnknown},
		// SUPPORTED CASES MUST RETAIN THEIR BUCKETS - a boundary that swallowed
		// ordinary commands would be a demotion of the signal, not a refusal.
		// A CONSEQUENCE WORTH STATING RATHER THAN HIDING: Codex wraps every
		// command in DOUBLE quotes, where expansions are live, so a '$'
		// anywhere in a Codex command refuses - even one that a real shell
		// would treat as literal because no valid expansion follows it.
		// Deciding that would require implementing expansion grammar, which is
		// exactly what ruling 123815 forbids, so the lexer refuses instead.
		{"dollar refuses under the codex wrapper", `go test ./... -run 'TestA$'`, phaseBucketUnknown},
		{"parens inside single quotes stay classifiable", `git commit -m 'fix (again)'`, phaseBucketVCS},
		// And the operators must still work through the same wrapper.
		// A backslash inside SINGLE quotes is literal data in POSIX shells, not
		// an escape. Treating it as an escape swallows the closing quote, so
		// the `&&` that follows stays "inside" the quoted region and a genuine
		// two-phase chain reads as a single test run. This case is what makes
		// the single-quote arm of the escape rule observable at all.
		{"backslash is literal inside single quotes", `go test ./... -run 'A\' && git status`, phaseBucketMixed},
		// A '#' that is NOT preceded by whitespace is an ordinary character, not
		// a comment: truncating there would drop the rest of a real chain and
		// report one phase where two ran. This is what makes the
		// after-whitespace half of the comment rule observable.
		{"hash inside a word is not a comment", `git commit -m msg#1 && go test ./...`, phaseBucketMixed},
		{"real chain is still mixed", "go build ./... && go test ./...", phaseBucketMixed},
		{"redirect idiom still classifies", "go test ./... 2>&1", phaseBucketTest},
		{"vcs is still vcs", `git commit -m "go test is slow"`, phaseBucketVCS},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			paths := config.PathsForHome(home)
			store := seedInstrumentJob(t, paths, "prodpath", "codex-reviewer", "codex")
			handle := openInstrumentedTranscript(t, home, "prodpath", "codex", store)

			// codexToolLines wraps the command exactly as Codex does, with
			// %q's backslash-escaped inner quotes - the wrapper is the point.
			start, end := codexToolLines("p1", tc.command)
			if _, err := handle.Write([]byte(start)); err != nil {
				t.Fatal(err)
			}
			time.Sleep(20 * time.Millisecond)
			if _, err := handle.Write([]byte(end)); err != nil {
				t.Fatal(err)
			}
			if err := handle.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}

			profile := readPhaseProfile(t, store, "prodpath")
			assertProfileIdentities(t, profile)
			if profile.Commands != 1 {
				t.Fatalf("commands = %d, want 1 (%+v)", profile.Commands, profile)
			}
			if profile.BucketCount[tc.want] != 1 {
				t.Fatalf("through the production path %q landed in %v, want %s",
					tc.command, profile.BucketCount, tc.want)
			}
		})
	}
}

// kimiToolLines renders a kimi exchange, whose Bash arguments arrive as JSON
// with the command as a BARE string - no outer double-quote wrapper. That is
// the production path on which the UNQUOTED half of the grammar boundary is
// reachable at all: Codex double-quotes everything, so its fixture can only
// exercise the in-double-quotes arm.
func kimiToolLines(callID, command string) (string, string) {
	args, _ := json.Marshal(map[string]any{"command": command})
	call, _ := json.Marshal(map[string]any{
		"role": "assistant",
		"tool_calls": []any{map[string]any{
			"id": callID, "type": "function",
			"function": map[string]any{"name": "bash", "arguments": string(args)},
		}},
	})
	result, _ := json.Marshal(map[string]any{
		"role": "tool", "tool_call_id": callID, "content": "done",
	})
	return string(call) + "\n", string(result) + "\n"
}

// TestPhaseInstrumentRefusesUnsupportedSyntaxOnABareKimiPayload closes the one
// gap the Codex fixture cannot reach. Kimi sends the command unquoted inside
// JSON, so an unquoted substitution is seen by the boundary's unquoted arm -
// and a mutant that stops flagging it survives every Codex-wrapped case.
func TestPhaseInstrumentRefusesUnsupportedSyntaxOnABareKimiPayload(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command string
		want    string
	}{
		{"unquoted command substitution refuses", "go test $(go list ./...)", phaseBucketUnknown},
		{"unquoted parameter expansion refuses", "${TOOL} test ./...", phaseBucketUnknown},
		{"plain command still classifies", "go test ./internal/cli/", phaseBucketTest},
		{"redirect idiom still classifies", "go test ./... 2>&1", phaseBucketTest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			paths := config.PathsForHome(home)
			store := seedInstrumentJob(t, paths, "kimipath", "kimi-reviewer", "kimi")
			handle := openInstrumentedTranscript(t, home, "kimipath", "kimi", store)

			start, end := kimiToolLines("k1", tc.command)
			if _, err := handle.Write([]byte(start)); err != nil {
				t.Fatal(err)
			}
			time.Sleep(20 * time.Millisecond)
			if _, err := handle.Write([]byte(end)); err != nil {
				t.Fatal(err)
			}
			if err := handle.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}

			profile := readPhaseProfile(t, store, "kimipath")
			assertProfileIdentities(t, profile)
			if profile.BucketCount[tc.want] != 1 {
				t.Fatalf("bare kimi payload %q landed in %v, want %s", tc.command, profile.BucketCount, tc.want)
			}
		})
	}
}

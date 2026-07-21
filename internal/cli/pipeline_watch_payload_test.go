package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/pipeline"
)

func TestPipelineRunPayloadParsing(t *testing.T) {
	tests := []struct {
		name    string
		entries []string
		rawJSON string
		want    string
		wantErr string
	}{
		{name: "repeatable", entries: []string{"z=last", "a=first=rest"}, want: `{"a":"first=rest","z":"last"}`},
		{name: "json", rawJSON: `{"subject":"hello"}`, want: `{"subject":"hello"}`},
		{name: "missing equals", entries: []string{"broken"}, wantErr: "must be key=value"},
		{name: "empty key", entries: []string{"=value"}, wantErr: "key must not be empty"},
		{name: "mutually exclusive", entries: []string{"a=b"}, rawJSON: `{"c":"d"}`, wantErr: "mutually exclusive"},
		{name: "json array", rawJSON: `[]`, wantErr: "must be a JSON object"},
		{name: "json non-string", rawJSON: `{"a":1}`, wantErr: "string values"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pipelineRunPayload(tc.entries, tc.rawJSON)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %q, err=%v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestPipelineRunPayloadEmptyJSONFlagStillConflicts(t *testing.T) {
	if _, err := pipelineRunPayload([]string{"a=b"}, "", true); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("error=%v", err)
	}
}

func TestPipelineRunThreadsPayloadIntoRun(t *testing.T) {
	home := t.TempDir()
	runCLI := func(args ...string) (string, string, int) {
		var stdout, stderr bytes.Buffer
		code := Run(append(args, "--home", home), &stdout, &stderr)
		return stdout.String(), stderr.String(), code
	}
	spec := writeSpec(t, "name: payload-flow\nrepo: owner/repo\nstages:\n  - id: a\n    cmd: echo a\n")
	if _, stderr, code := runCLI("pipeline", "add", spec); code != 0 {
		t.Fatalf("add exit=%d stderr=%s", code, stderr)
	}
	stdout, stderr, code := runCLI("pipeline", "run", "payload-flow", "--payload", "subject=hello")
	if code != 0 {
		t.Fatalf("run exit=%d stderr=%s", code, stderr)
	}
	runID := strings.TrimSpace(stdout)
	if err := withStore(home, func(store *db.Store) error {
		run, ok, err := store.GetPipelineRun(context.Background(), runID)
		if err != nil {
			return err
		}
		if !ok || run.PayloadJSON != `{"subject":"hello"}` {
			t.Fatalf("run = %+v, found=%v", run, ok)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPipelineWatchTerminalRuns(t *testing.T) {
	tests := []struct {
		state string
		code  int
	}{
		{state: pipeline.RunSucceeded, code: 0},
		{state: pipeline.RunFailed, code: 1},
	}
	for _, tc := range tests {
		t.Run(tc.state, func(t *testing.T) {
			home, _, store := heartbeatLoopE2EHome(t)
			runID := "prun-watch-" + tc.state
			now := time.Now().UTC()
			if err := store.CreatePipelineRun(context.Background(), db.PipelineRun{ID: runID, Pipeline: "watch", Trigger: "manual", PayloadJSON: "{}", SpecHash: "hash", State: tc.state, StartedAt: now, FinishedAt: now}); err != nil {
				t.Fatal(err)
			}
			if err := store.CreatePipelineRunStage(context.Background(), db.PipelineRunStage{RunID: runID, StageID: "build", State: map[bool]string{true: pipeline.StageSucceeded, false: pipeline.StageFailed}[tc.state == pipeline.RunSucceeded], StartedAt: now, FinishedAt: now}); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			code := runPipelineWatchCmd([]string{runID, "--home", home, "--poll", "1ms"}, &stdout, &stderr)
			if code != tc.code || !strings.HasSuffix(stdout.String(), "state: "+tc.state+"\n") || stderr.Len() != 0 {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
		})
	}
}

func TestPipelineWatchTerminalJSON(t *testing.T) {
	home, _, store := heartbeatLoopE2EHome(t)
	now := time.Now().UTC()
	run := db.PipelineRun{ID: "prun-watch-json", Pipeline: "watch", Trigger: "manual", PayloadJSON: `{"subject":"hello"}`, SpecHash: "hash", State: pipeline.RunSucceeded, StartedAt: now, FinishedAt: now}
	if err := store.CreatePipelineRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := runPipelineWatchCmd([]string{run.ID, "--home", home, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	var got pipelineRunJSON
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil || got.ID != run.ID || got.State != pipeline.RunSucceeded {
		t.Fatalf("json=%s err=%v decoded=%+v", stdout.String(), err, got)
	}
}

func TestPipelineWatchTimeoutAndPollValidation(t *testing.T) {
	home, _, store := heartbeatLoopE2EHome(t)
	run := db.PipelineRun{ID: "prun-watch-running", Pipeline: "watch", Trigger: "manual", State: pipeline.RunRunning, StartedAt: time.Now().UTC()}
	if err := store.CreatePipelineRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := runPipelineWatchCmd([]string{run.ID, "--home", home, "--timeout", "1ms", "--poll", "1ms"}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "still running") {
		t.Fatalf("timeout code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := runPipelineWatchCmd([]string{run.ID, "--home", home, "--poll", "0s"}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "poll interval must be positive") {
		t.Fatalf("poll code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestPrintPipelineStageTransitionsDedupes(t *testing.T) {
	last := make(map[string]string)
	var out bytes.Buffer
	rows := []db.PipelineRunStage{{StageID: "a", State: pipeline.StageRunning}}
	printPipelineStageTransitions(&out, rows, last)
	printPipelineStageTransitions(&out, rows, last)
	rows[0].State = pipeline.StageSucceeded
	printPipelineStageTransitions(&out, rows, last)
	if got, want := out.String(), "a: RUNNING\na: SUCCEEDED\n"; got != want {
		t.Fatalf("transitions=%q want=%q", got, want)
	}
}

func TestPrintPipelinePayloadPreviewRedactsAndTruncates(t *testing.T) {
	var out bytes.Buffer
	printPipelinePayloadPreview(&out, `{"api_token":"do-not-print","subject":"`+strings.Repeat("x", 50)+`"}`)
	got := out.String()
	if strings.Contains(got, "do-not-print") || !strings.Contains(got, `api_token: "[redacted]"`) || !strings.Contains(got, strings.Repeat("x", 40)+"…") {
		t.Fatalf("payload preview=%q", got)
	}
}

// A SERVICE run persists a typed payload with integer/boolean values
// (canonicalPipelineServicePayload). The preview must render them, not silently
// drop the whole payload the way a map[string]string decode would.
func TestPrintPipelinePayloadPreviewRendersNonStringValues(t *testing.T) {
	var out bytes.Buffer
	printPipelinePayloadPreview(&out, `{"count":5,"enabled":true,"name":"foo"}`)
	got := out.String()
	for _, want := range []string{`count: "5"`, `enabled: "true"`, `name: "foo"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("preview %q missing %q", got, want)
		}
	}
}

// A non-object / undecodable payload falls back to the raw line so provenance is
// never silently hidden.
func TestPrintPipelinePayloadPreviewFallsBackToRaw(t *testing.T) {
	var out bytes.Buffer
	printPipelinePayloadPreview(&out, `[1,2,3]`)
	if got := out.String(); !strings.Contains(got, "payload_json: [1,2,3]") {
		t.Fatalf("expected raw fallback, got %q", got)
	}
}

// Long unambiguous markers redact; short benign keys that merely contain a
// fragment ("author" superset of "auth", "cookie_domain" superset of "cookie")
// must NOT be redacted.
func TestPipelinePayloadKeyRedactionBoundaries(t *testing.T) {
	for _, key := range []string{"secret", "api_key", "access_token", "db_password", "my_credential", "private_key"} {
		if !pipelinePayloadKeyLooksSecret(key) {
			t.Errorf("expected %q to be redacted", key)
		}
	}
	for _, key := range []string{"author", "oauth_provider", "cookie_domain", "person", "transcript_path", "count"} {
		if pipelinePayloadKeyLooksSecret(key) {
			t.Errorf("expected %q to NOT be redacted", key)
		}
	}
}

package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
)

// #1856 caller tests. Every fixture is a COPY under t.TempDir() pointed at
// through agent.RuntimeConfigDir: the live profile is never read and never
// written by these tests, and no kimi binary runs.
const (
	kimiDetectorLiveCredential    = `{"access_token":"tok","refresh_token":"ref","expires_at":1788000000,"scope":"coding","token_type":"Bearer"}`
	kimiDetectorBlankedCredential = `{"access_token":"","refresh_token":"","expires_at":0,"scope":"coding","token_type":"Bearer"}`
)

func writeKimiDetectorCredential(t *testing.T, profileDir, body string) string {
	t.Helper()
	path := filepath.Join(profileDir, runtime.KimiCredentialRelPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	return path
}

func kimiDetectorEvents(t *testing.T, store *db.Store, jobID string) []db.JobEvent {
	t.Helper()
	events, err := store.ListJobEvents(context.Background(), jobID)
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	var out []db.JobEvent
	for _, event := range events {
		if event.Kind == kimiCredentialDegradedEvent {
			out = append(out, event)
		}
	}
	return out
}

// TestNonSeatKimiDeliveryRecordsCredentialDegradation is the instrument's
// reason to exist: the credential carried a token before the run and none
// after, and the run SUCCEEDED. #1856's own measured window contains a kimi job
// that succeeded at 13:59:50Z shortly before the blanking, so an instrument
// that only reported on failures would miss the shape it was built for.
func TestNonSeatKimiDeliveryRecordsCredentialDegradation(t *testing.T) {
	store := daemonWorkerStore(t)
	worker := defaultJobWorker(store, io.Discard)
	profile := t.TempDir()
	path := writeKimiDetectorCredential(t, profile, kimiDetectorLiveCredential)
	agent := runtime.Agent{Name: "kimi-impl", Runtime: runtime.KimiRuntime, RuntimeConfigDir: profile}

	before, observed := worker.observeNonSeatKimiCredential(agent)
	if !observed || !before.HasToken() {
		t.Fatalf("pre-run observation = %+v observed = %v, want a token", before, observed)
	}
	// The vendor CLI blanking its own credential in place - the measured #1856
	// shape, reproduced without running kimi.
	writeKimiDetectorCredential(t, profile, kimiDetectorBlankedCredential)

	worker.recordKimiCredentialDegradation(context.Background(), "job-kimi-1", agent, before, observed)

	events := kimiDetectorEvents(t, store, "job-kimi-1")
	if len(events) != 1 {
		t.Fatalf("%s events = %d, want exactly 1", kimiCredentialDegradedEvent, len(events))
	}
	message := events[0].Message
	for _, want := range []string{path, "token/", "blank_token/", "INFERRED"} {
		if !strings.Contains(message, want) {
			t.Fatalf("event message %q is missing %q", message, want)
		}
	}
	// Report-only: the observation names the file, never its contents.
	if strings.Contains(message, "tok") && !strings.Contains(message, "kimi-code.json") {
		t.Fatalf("event message may not carry credential contents: %q", message)
	}
}

// TestReadOnlySeatKimiDeliveryIsNotObserved pins the POPULATION as a property
// of the code. A seat reads a staged clone inside its own writable cache root,
// so a change there says nothing about the operator's profile; observing it
// would emit events indistinguishable from real ones.
func TestReadOnlySeatKimiDeliveryIsNotObserved(t *testing.T) {
	store := daemonWorkerStore(t)
	worker := defaultJobWorker(store, io.Discard)
	profile := t.TempDir()
	writeKimiDetectorCredential(t, profile, kimiDetectorLiveCredential)

	for _, tc := range []struct {
		name  string
		agent runtime.Agent
	}{
		{"read-only seat", runtime.Agent{Name: "kimi-seat", Runtime: runtime.KimiRuntime, RuntimeConfigDir: profile, ReadOnlySeat: true}},
		{"another runtime", runtime.Agent{Name: "claude", Runtime: runtime.ClaudeRuntime, RuntimeConfigDir: profile}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, observed := worker.observeNonSeatKimiCredential(tc.agent)
			if observed {
				t.Fatalf("observation = %+v, want none for %s", before, tc.name)
			}
			writeKimiDetectorCredential(t, profile, kimiDetectorBlankedCredential)
			jobID := "job-" + tc.name
			worker.recordKimiCredentialDegradation(context.Background(), jobID, tc.agent, before, observed)
			if events := kimiDetectorEvents(t, store, jobID); len(events) != 0 {
				t.Fatalf("%s events = %d, want 0 for %s", kimiCredentialDegradedEvent, len(events), tc.name)
			}
			writeKimiDetectorCredential(t, profile, kimiDetectorLiveCredential)
		})
	}
}

// TestNonSeatKimiDeliveryStaysSilentWithoutADegradation keeps the instrument
// quiet on the cases that are not the defect: an unchanged credential, and a
// host that was already logged out before the run.
func TestNonSeatKimiDeliveryStaysSilentWithoutADegradation(t *testing.T) {
	store := daemonWorkerStore(t)
	worker := defaultJobWorker(store, io.Discard)

	for _, tc := range []struct {
		name   string
		before string
		after  string
	}{
		{"unchanged token", kimiDetectorLiveCredential, kimiDetectorLiveCredential},
		{"already blank", kimiDetectorBlankedCredential, kimiDetectorBlankedCredential},
		{"login during the run", kimiDetectorBlankedCredential, kimiDetectorLiveCredential},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile := t.TempDir()
			writeKimiDetectorCredential(t, profile, tc.before)
			agent := runtime.Agent{Name: "kimi-impl", Runtime: runtime.KimiRuntime, RuntimeConfigDir: profile}
			observation, observed := worker.observeNonSeatKimiCredential(agent)
			if !observed {
				t.Fatal("expected an observation for a non-seat kimi agent")
			}
			writeKimiDetectorCredential(t, profile, tc.after)
			jobID := "job-silent-" + tc.name
			worker.recordKimiCredentialDegradation(context.Background(), jobID, agent, observation, observed)
			if events := kimiDetectorEvents(t, store, jobID); len(events) != 0 {
				t.Fatalf("%s events = %d, want 0", kimiCredentialDegradedEvent, len(events))
			}
		})
	}
}

// TestAgentDoctorObservesTheLiveKimiCredential covers caller 1: `gitmoot agent
// doctor` runs the runtime CLI through the plain runner with no seat sandbox, so
// its child reads the LIVE profile. Doctor is a command rather than a job, so
// the observation is reported on stderr and there is no job row to attach.
func TestAgentDoctorObservesTheLiveKimiCredential(t *testing.T) {
	profile := t.TempDir()
	path := writeKimiDetectorCredential(t, profile, kimiDetectorLiveCredential)
	agent := runtime.Agent{Name: "gm-review-kimi", Runtime: runtime.KimiRuntime, RuntimeConfigDir: profile}

	before, observed := observeKimiDoctorCredential(agent)
	if !observed || !before.HasToken() {
		t.Fatalf("pre-check observation = %+v observed = %v, want a token", before, observed)
	}
	writeKimiDetectorCredential(t, profile, kimiDetectorBlankedCredential)
	degraded := kimiCredentialDegradationSinceDoctor(agent, before, observed)
	if degraded == "" {
		t.Fatal("a blanked credential across the doctor check must be reported")
	}
	for _, want := range []string{path, "blank_token/", "INFERRED"} {
		if !strings.Contains(degraded, want) {
			t.Fatalf("report %q is missing %q", degraded, want)
		}
	}
	// A non-kimi agent is not observed at all, so doctor's other runtimes keep
	// their byte-identical output.
	claude := runtime.Agent{Name: "claude", Runtime: runtime.ClaudeRuntime, RuntimeConfigDir: profile}
	if _, ok := observeKimiDoctorCredential(claude); ok {
		t.Fatal("observeKimiDoctorCredential must not observe a non-kimi runtime")
	}
	if report := kimiCredentialDegradationSinceDoctor(claude, before, false); report != "" {
		t.Fatalf("unobserved agent produced a report: %q", report)
	}
}

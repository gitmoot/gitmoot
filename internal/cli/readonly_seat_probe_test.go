package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/subprocess"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// `gitmoot auth probe claude` and `gitmoot doctor` were green for hours while
// every claude review job failed, because they probe the AMBIENT credential and
// a read-only seat authenticates with a staged snapshot of the configured
// config dir. A probe that cannot see the credential under test is a false
// green, so the probe now reports the staged file as well.
func TestSeatCredentialProbeReportsTheStagedCredential(t *testing.T) {
	for name, test := range map[string]struct {
		expiresAt    int64
		refreshToken string
		want         []string
	}{
		"unusable": {
			expiresAt: 0, refreshToken: "",
			want: []string{"UNUSABLE", "no refresh token", "re-logged in"},
		},
		"expired but refreshable": {
			expiresAt: time.Now().UTC().Add(-time.Hour).UnixMilli(), refreshToken: "r",
			want: []string{"EXPIRED", "refresh token present", "discarded with the job"},
		},
		"valid": {
			expiresAt: time.Now().UTC().Add(8 * time.Hour).UnixMilli(), refreshToken: "r",
			want: []string{"declares expiry", "does not prove"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeClaudeCredential(t, dir, test.expiresAt, test.refreshToken)
			t.Setenv("CLAUDE_CONFIG_DIR", dir)
			// Gateway mode and the runtime-auth overlay both change this verdict
			// now, so pin them instead of inheriting the host's (#1810 round 3).
			t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
			t.Setenv("ANTHROPIC_API_KEY", "")

			var stdout bytes.Buffer
			writeSeatCredentialProbe(&stdout, seatDoctorTestPaths(t, t.TempDir(), false))
			out := stdout.String()
			if !strings.Contains(out, dir) {
				t.Fatalf("probe output must name the staged file; got %q", out)
			}
			for _, want := range test.want {
				if !strings.Contains(out, want) {
					t.Fatalf("probe output %q must contain %q", out, want)
				}
			}
		})
	}
}

// A credential file with no readable expiry must not be reported as a verdict
// either way: the probe says what it can see and nothing more.
func TestSeatCredentialProbeStatesWhenItCannotAssert(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"),
		[]byte(`{"claudeAiOauth":{"accessToken":"host"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", dir)

	var stdout bytes.Buffer
	writeSeatCredentialProbe(&stdout, config.Paths{})
	out := stdout.String()
	if !strings.Contains(out, "no readable expiry") {
		t.Fatalf("probe output %q must say it cannot assert", out)
	}
	for _, forbidden := range []string{"UNUSABLE", "EXPIRED", "declares expiry"} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("probe output %q must not claim %q without an expiry", out, forbidden)
		}
	}
}

// The scheduler's probe is the only one that makes an automated DECISION, and it
// probed the AMBIENT credential: a seat authenticates with a staged snapshot of
// its config dir plus the resolved overlay, so an ambient "valid" released held
// seat jobs straight back into the dead snapshot they had just failed on (#1810
// review, round 2).
func TestSeatAuthProbeMeasuresTheStagedSeatCredential(t *testing.T) {
	store, home := blockerE2EHome(t)
	paths, err := pathsFromFlag(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runtimeAuthFilePath(paths.Home),
		[]byte("CLAUDE_CODE_OAUTH_TOKEN=seat-probe-token-long-enough\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sourceDir := t.TempDir()
	writeClaudeCredential(t, sourceDir, 0, "")
	seedDaemonWorkerAgentWithPolicy(t, store, "claude-reviewer", runtime.ClaudeRuntime,
		"550e8400-e29b-41d4-a716-446655440000", []string{"review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	worker := defaultJobWorker(store, io.Discard, home)
	job := db.Job{ID: "seat-auth-probe", Agent: "claude-reviewer"}

	var probedEnv []string
	var stagedCredential string
	original := claudeAuthLiveCheck
	claudeAuthLiveCheck = func(_ context.Context, runner subprocess.Runner, _ string, _ []string) error {
		curated, ok := runner.(subprocess.CuratedGroupRunner)
		if !ok {
			t.Fatalf("probe runner %T does not carry a curated seat environment", runner)
		}
		probedEnv = append([]string(nil), curated.BaseEnv...)
		for _, entry := range probedEnv {
			if dir, ok := strings.CutPrefix(entry, "CLAUDE_CONFIG_DIR="); ok {
				if data, readErr := os.ReadFile(filepath.Join(dir, ".credentials.json")); readErr == nil {
					stagedCredential = string(data)
				}
			}
		}
		return errors.Join(errors.New("rejected"), runtime.ErrClaudeAuthFailed)
	}
	t.Cleanup(func() { claudeAuthLiveCheck = original })

	t.Setenv("CLAUDE_CONFIG_DIR", sourceDir)
	payload := workflow.JobPayload{Repo: "owner/repo", ReadOnlySeat: true}
	if verdict := worker.defaultAuthProbe(context.Background(), job, payload); verdict != authProbeInvalid {
		t.Fatalf("verdict = %v, want invalid: the staged credential is dead", verdict)
	}
	if stagedCredential == "" || !strings.Contains(stagedCredential, "claudeAiOauth") {
		t.Fatalf("probe did not read a staged copy of the seat credential: %q", stagedCredential)
	}
	if !containsEnv(probedEnv, "CLAUDE_CODE_OAUTH_TOKEN=seat-probe-token-long-enough") {
		t.Fatalf("probe env lacks the resolved overlay: %v", redactEnvNames(probedEnv))
	}
	var configDir string
	for _, entry := range probedEnv {
		if dir, ok := strings.CutPrefix(entry, "CLAUDE_CONFIG_DIR="); ok {
			configDir = dir
		}
	}
	if configDir == "" || configDir == sourceDir {
		t.Fatalf("probe used config dir %q; it must stage a disposable copy, never the host profile", configDir)
	}
	if _, err := os.Stat(configDir); !os.IsNotExist(err) {
		t.Fatalf("probe left its staged credential behind at %q (err=%v)", configDir, err)
	}

	// A non-seat job keeps the ambient runner: nothing is staged for it.
	probedEnv = nil
	stagedCredential = ""
	claudeAuthLiveCheck = func(_ context.Context, runner subprocess.Runner, _ string, _ []string) error {
		if curated, ok := runner.(subprocess.CuratedGroupRunner); ok {
			for _, entry := range curated.BaseEnv {
				if strings.Contains(entry, "gitmoot-seat-auth-probe") {
					t.Fatalf("a non-seat job probed a staged seat credential: %q", entry)
				}
			}
		}
		return nil
	}
	if verdict := worker.defaultAuthProbe(context.Background(), job, workflow.JobPayload{Repo: "owner/repo"}); verdict != authProbeValid {
		t.Fatalf("non-seat verdict = %v, want valid", verdict)
	}
}

// Seat verdicts must not share a cache key with ambient jobs, or one ambient
// "valid" releases every held seat job.
func TestAuthProbeDedupKeySeparatesSeatsFromAmbient(t *testing.T) {
	store, home := blockerE2EHome(t)
	seedDaemonWorkerAgentWithPolicy(t, store, "claude-reviewer", runtime.ClaudeRuntime,
		"550e8400-e29b-41d4-a716-446655440000", []string{"review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	worker := defaultJobWorker(store, io.Discard, home)
	job := db.Job{ID: "k", Agent: "claude-reviewer"}

	ambient := worker.authProbeDedupKey(context.Background(), job, workflow.JobPayload{Repo: "owner/repo"})
	seatA := worker.authProbeDedupKey(context.Background(), job, workflow.JobPayload{Repo: "owner/repo", ReadOnlySeat: true, RuntimeConfigDir: "/profiles/a"})
	seatB := worker.authProbeDedupKey(context.Background(), job, workflow.JobPayload{Repo: "owner/repo", ReadOnlySeat: true, RuntimeConfigDir: "/profiles/b"})
	t.Setenv("CLAUDE_CONFIG_DIR", "/profiles/a")
	seatDefault := worker.authProbeDedupKey(context.Background(), job, workflow.JobPayload{Repo: "owner/repo", ReadOnlySeat: true})
	if ambient == seatA {
		t.Fatalf("seat and ambient share the key %q", ambient)
	}
	if seatA == seatB {
		t.Fatalf("two seats with different config dirs share the key %q", seatA)
	}
	if seatDefault != seatA {
		t.Fatalf("default seat key %q differs from explicit effective account key %q", seatDefault, seatA)
	}
}

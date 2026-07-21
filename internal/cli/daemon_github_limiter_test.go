package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/github"
)

func TestDaemonGitHubLimiterLineDefault(t *testing.T) {
	paths := config.PathsForHome(t.TempDir())
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	line := daemonGitHubLimiterLine(paths)
	if !strings.Contains(line, "max_concurrent=unlimited") {
		t.Fatalf("default line missing unlimited concurrency: %q", line)
	}
	if !strings.Contains(line, "min_interval=off") {
		t.Fatalf("default line missing min_interval=off: %q", line)
	}
	if !strings.Contains(line, "secondary_backoff=on") {
		t.Fatalf("default line should show secondary backoff on: %q", line)
	}
}

func TestDaemonGitHubLimiterLineConfigured(t *testing.T) {
	paths := config.PathsForHome(t.TempDir())
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	body := "\n[github]\nmax_concurrent = 6\nmin_interval = \"250ms\"\nsecondary_backoff = false\n"
	if err := os.WriteFile(paths.ConfigFile, []byte(config.DefaultConfig(paths)+body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	line := daemonGitHubLimiterLine(paths)
	if !strings.Contains(line, "max_concurrent=6") || !strings.Contains(line, "min_interval=250ms") {
		t.Fatalf("configured line = %q", line)
	}
	if !strings.Contains(line, "secondary_backoff=off") {
		t.Fatalf("configured line should show backoff off: %q", line)
	}
}

// configureGitHubLimiter must install the [github] policy onto the process-global
// limiter and log a summary; with the default config it enables secondary backoff.
func TestConfigureGitHubLimiterInstallsPolicy(t *testing.T) {
	t.Cleanup(func() { github.ConfigureDefault(github.RateLimiterConfig{}) })
	paths := config.PathsForHome(t.TempDir())
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	var out bytes.Buffer
	configureGitHubLimiter(paths, &out)
	if !strings.Contains(out.String(), "github limiter:") {
		t.Fatalf("expected summary log line, got %q", out.String())
	}
	state := github.DefaultLimiter().Snapshot()
	if !state.BackoffEnabled {
		t.Fatalf("default config should enable secondary backoff on the shared limiter")
	}
	if state.MaxConcurrent != 0 {
		t.Fatalf("default MaxConcurrent = %d, want 0 (unlimited)", state.MaxConcurrent)
	}
}

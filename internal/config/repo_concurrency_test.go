package config

import (
	"os"
	"strings"
	"testing"
)

func writeRepoConcurrencyConfig(t *testing.T, body string) Paths {
	t.Helper()
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return paths
}

func TestLoadRepoConcurrencyParsesValidSections(t *testing.T) {
	paths := writeRepoConcurrencyConfig(t, `
[repos."jerryfane/gitmoot"]
max_parallel = 1

[repos."owner/other"]
max_parallel = 4
scheduler = "pool"
`)
	repos, err := LoadRepoConcurrency(paths)
	if err != nil {
		t.Fatalf("LoadRepoConcurrency returned error: %v", err)
	}
	if len(repos) != 2 {
		t.Fatalf("expected 2 repo overrides, got %d: %+v", len(repos), repos)
	}
	// Config order is preserved.
	first := repos[0]
	if first.Repo != "jerryfane/gitmoot" || first.MaxParallel != 1 || first.Scheduler != "" {
		t.Fatalf("first override = %+v", first)
	}
	second := repos[1]
	if second.Repo != "owner/other" || second.MaxParallel != 4 || second.Scheduler != "pool" {
		t.Fatalf("second override = %+v", second)
	}

	got, ok := RepoConcurrencyFor(repos, "owner/other")
	if !ok || got.MaxParallel != 4 || got.Scheduler != "pool" {
		t.Fatalf("RepoConcurrencyFor(owner/other) = %+v ok=%v", got, ok)
	}
	if _, ok := RepoConcurrencyFor(repos, "owner/absent"); ok {
		t.Fatalf("RepoConcurrencyFor(owner/absent) should not match")
	}
}

func TestLoadRepoConcurrencyAbsentDefaultsToEmpty(t *testing.T) {
	// No [repos.*] section at all: off by default (empty slice, no error), so
	// every repo keeps the global scheduler behavior.
	paths := writeRepoConcurrencyConfig(t, "")
	repos, err := LoadRepoConcurrency(paths)
	if err != nil {
		t.Fatalf("LoadRepoConcurrency returned error: %v", err)
	}
	if len(repos) != 0 {
		t.Fatalf("expected no overrides for a config without [repos.*], got %+v", repos)
	}
	if _, ok := RepoConcurrencyFor(repos, "jerryfane/gitmoot"); ok {
		t.Fatalf("RepoConcurrencyFor on an empty list must not match")
	}
}

func TestLoadRepoConcurrencyZeroMaxParallelIsAllowed(t *testing.T) {
	// max_parallel = 0 explicitly means "use the global default" — it must NOT
	// error (never a zero-concurrency stalled repo), and it must not override.
	paths := writeRepoConcurrencyConfig(t, `
[repos."jerryfane/gitmoot"]
max_parallel = 0
`)
	repos, err := LoadRepoConcurrency(paths)
	if err != nil {
		t.Fatalf("LoadRepoConcurrency returned error: %v", err)
	}
	if len(repos) != 1 || repos[0].MaxParallel != 0 {
		t.Fatalf("expected one override with max_parallel 0, got %+v", repos)
	}
}

func TestLoadRepoConcurrencyRejectsNegativeMaxParallel(t *testing.T) {
	paths := writeRepoConcurrencyConfig(t, `
[repos."jerryfane/gitmoot"]
max_parallel = -1
`)
	_, err := LoadRepoConcurrency(paths)
	if err == nil || !strings.Contains(err.Error(), "max_parallel") {
		t.Fatalf("expected a max_parallel validation error, got %v", err)
	}
}

func TestLoadRepoConcurrencyRejectsUnknownScheduler(t *testing.T) {
	paths := writeRepoConcurrencyConfig(t, `
[repos."jerryfane/gitmoot"]
max_parallel = 2
scheduler = "turbo"
`)
	_, err := LoadRepoConcurrency(paths)
	if err == nil || !strings.Contains(err.Error(), "scheduler") {
		t.Fatalf("expected a scheduler validation error, got %v", err)
	}
}

func TestLoadRepoConcurrencyRejectsNonIntMaxParallel(t *testing.T) {
	paths := writeRepoConcurrencyConfig(t, `
[repos."jerryfane/gitmoot"]
max_parallel = "two"
`)
	_, err := LoadRepoConcurrency(paths)
	if err == nil {
		t.Fatalf("expected a parse error for a non-integer max_parallel")
	}
}

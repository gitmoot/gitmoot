package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, paths Paths, body string) {
	t.Helper()
	if err := os.WriteFile(paths.ConfigFile, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func TestLoadDaemonRuntimeConfigAbsentIsNoOp(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	cfg, err := LoadDaemonRuntimeConfig(paths)
	if err != nil {
		t.Fatalf("LoadDaemonRuntimeConfig: %v", err)
	}
	if cfg.PollSet || cfg.WorkersSet || cfg.SchedulerSet {
		t.Fatalf("absent [daemon] section must set nothing, got %+v", cfg)
	}
}

func TestLoadDaemonRuntimeConfigParsesFields(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	writeConfig(t, paths, "[daemon]\npoll = \"45s\"\nworkers = 4\nscheduler = \"pool\"\nidle_grace_ticks = 5\nidle_max_multiplier = 8\n")
	cfg, err := LoadDaemonRuntimeConfig(paths)
	if err != nil {
		t.Fatalf("LoadDaemonRuntimeConfig: %v", err)
	}
	if !cfg.PollSet || cfg.Poll != 45*time.Second {
		t.Fatalf("poll = %v (set=%v), want 45s", cfg.Poll, cfg.PollSet)
	}
	if !cfg.WorkersSet || cfg.Workers != 4 {
		t.Fatalf("workers = %d (set=%v), want 4", cfg.Workers, cfg.WorkersSet)
	}
	if !cfg.SchedulerSet || cfg.Scheduler != "pool" {
		t.Fatalf("scheduler = %q (set=%v), want pool", cfg.Scheduler, cfg.SchedulerSet)
	}
	if !cfg.IdleGraceTicksSet || cfg.IdleGraceTicks != 5 || !cfg.IdleMaxMultiplierSet || cfg.IdleMaxMultiplier != 8 {
		t.Fatalf("idle cadence = %+v, want grace=5 max=8", cfg)
	}
}

func TestLoadDaemonRuntimeConfigBareDuration(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	writeConfig(t, paths, "[daemon]\npoll = 1m\n")
	cfg, err := LoadDaemonRuntimeConfig(paths)
	if err != nil {
		t.Fatalf("LoadDaemonRuntimeConfig: %v", err)
	}
	if !cfg.PollSet || cfg.Poll != time.Minute {
		t.Fatalf("poll = %v, want 1m", cfg.Poll)
	}
}

func TestLoadDaemonRuntimeConfigParallelSugar(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	writeConfig(t, paths, "[daemon]\nparallel = 3\n")
	cfg, err := LoadDaemonRuntimeConfig(paths)
	if err != nil {
		t.Fatalf("LoadDaemonRuntimeConfig: %v", err)
	}
	if !cfg.WorkersSet || cfg.Workers != 3 || !cfg.SchedulerSet || cfg.Scheduler != "pool" {
		t.Fatalf("parallel=3 must set workers=3 + scheduler=pool, got %+v", cfg)
	}
}

func TestLoadDaemonRuntimeConfigRejectsBadValues(t *testing.T) {
	cases := map[string]string{
		"bad poll":               "[daemon]\npoll = \"nope\"\n",
		"nonpositive poll":       "[daemon]\npoll = \"0s\"\n",
		"bad workers":            "[daemon]\nworkers = 0\n",
		"bad scheduler":          "[daemon]\nscheduler = \"turbo\"\n",
		"parallel+workers":       "[daemon]\nparallel = 2\nworkers = 3\n",
		"nonpositive parallel":   "[daemon]\nparallel = 0\n",
		"nonpositive idle grace": "[daemon]\nidle_grace_ticks = 0\n",
		"nonpositive idle max":   "[daemon]\nidle_max_multiplier = 0\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			paths := PathsForHome(t.TempDir())
			if err := Initialize(paths); err != nil {
				t.Fatalf("Initialize: %v", err)
			}
			writeConfig(t, paths, body)
			if _, err := LoadDaemonRuntimeConfig(paths); err == nil {
				t.Fatalf("expected error for %q", strings.TrimSpace(body))
			}
		})
	}
}

func TestLoadDaemonRuntimeConfigIgnoresUnknownKeys(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	// An unknown key (e.g. a future #576 per-repo cap) must not break older binaries.
	writeConfig(t, paths, "[daemon]\nworkers = 2\nmax_per_repo = 1\n")
	cfg, err := LoadDaemonRuntimeConfig(paths)
	if err != nil {
		t.Fatalf("LoadDaemonRuntimeConfig: %v", err)
	}
	if !cfg.WorkersSet || cfg.Workers != 2 {
		t.Fatalf("workers = %d, want 2", cfg.Workers)
	}
}

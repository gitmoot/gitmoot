package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// DaemonRuntimeConfig is the daemon's WARM-reloadable runtime settings, read from
// the optional [daemon] section of the config file (issue #577). It is the concrete
// reload source that lets a running daemon pick up poll/worker/scheduler changes on
// SIGHUP WITHOUT a `daemon restart` — a restart tears down in-flight supervision and
// re-inherits the launching shell's environment, dropping runtime auth (the #559
// disaster).
//
// Each field carries a companion *Set bool reporting whether the key was PRESENT in
// the file. This is what makes the source COMPOSE cleanly with CLI flags and with a
// warm reload:
//   - at start, CLI flags remain the authoritative initial value; a [daemon] key is
//     applied only where the operator did not pass the matching flag (flag = override);
//   - on SIGHUP, only keys actually present in the file are applied to the live
//     supervisor, leaving every other live value (e.g. one set from a CLI flag)
//     untouched. An absent-or-empty [daemon] section therefore reloads to a NO-OP,
//     so behavior is byte-identical for daemons that never write the section.
//
// Per-repo concurrency caps (#576, built in parallel) plug into the SAME reload path
// once merged: they live in the config layer and are re-read each tick, so a warm
// reload composes with them without this type hard-coding global-only scope.
type DaemonRuntimeConfig struct {
	// Poll is the [daemon].poll interval. Applied live: the supervisor re-reads the
	// live poll each cycle, so a change takes effect on the next tick.
	Poll    time.Duration
	PollSet bool
	// Workers is the [daemon].workers count (worker-pool size). Applied live: the
	// worker limit is re-read per tick and the pool is re-dispatched each tick, so a
	// resize takes effect on the next tick without disturbing in-flight jobs.
	Workers    int
	WorkersSet bool
	// Scheduler is the [daemon].scheduler mode ("barrier" | "pool"). Applied live: the
	// per-tick worker dispatch reads the live mode each tick.
	Scheduler    string
	SchedulerSet bool
}

// LoadDaemonRuntimeConfig parses the [daemon] section. A file with no [daemon]
// section (or an empty one) returns a zero-value DaemonRuntimeConfig with every
// *Set false — i.e. "nothing to reload", which the caller treats as a no-op. The
// `parallel` key is intent-level sugar for `workers = N` + `scheduler = "pool"`
// together (mirroring the CLI --parallel flag); it conflicts with an explicit
// workers/scheduler in the same section so it can never silently win.
func LoadDaemonRuntimeConfig(paths Paths) (DaemonRuntimeConfig, error) {
	content, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		return DaemonRuntimeConfig{}, err
	}
	var cfg DaemonRuntimeConfig
	parallel := 0
	parallelSet := false
	current := false
	for _, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(stripConfigComment(raw))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section := strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")
			current = strings.TrimSpace(section) == "daemon"
			continue
		}
		if !current {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		switch key {
		case "poll":
			parsed, err := parseConfigDuration(value)
			if err != nil {
				return DaemonRuntimeConfig{}, fmt.Errorf("parse [daemon].poll: %w", err)
			}
			cfg.Poll = parsed
			cfg.PollSet = true
		case "workers":
			parsed, err := strconv.Atoi(value)
			if err != nil {
				return DaemonRuntimeConfig{}, fmt.Errorf("parse [daemon].workers: %w", err)
			}
			cfg.Workers = parsed
			cfg.WorkersSet = true
		case "scheduler":
			parsed, err := parseConfigString(value)
			if err != nil {
				return DaemonRuntimeConfig{}, fmt.Errorf("parse [daemon].scheduler: %w", err)
			}
			cfg.Scheduler = strings.TrimSpace(parsed)
			cfg.SchedulerSet = true
		case "parallel":
			parsed, err := strconv.Atoi(value)
			if err != nil {
				return DaemonRuntimeConfig{}, fmt.Errorf("parse [daemon].parallel: %w", err)
			}
			parallel = parsed
			parallelSet = true
		default:
			// Unknown keys are ignored so the section can grow (e.g. #576 per-repo
			// caps) without breaking older binaries.
		}
	}
	if parallelSet {
		if cfg.WorkersSet || cfg.SchedulerSet {
			return DaemonRuntimeConfig{}, fmt.Errorf("[daemon].parallel cannot be combined with workers or scheduler")
		}
		if parallel <= 0 {
			return DaemonRuntimeConfig{}, fmt.Errorf("[daemon].parallel must be positive")
		}
		cfg.Workers = parallel
		cfg.WorkersSet = true
		cfg.Scheduler = "pool"
		cfg.SchedulerSet = true
	}
	if err := validateDaemonRuntimeConfig(cfg); err != nil {
		return DaemonRuntimeConfig{}, err
	}
	return cfg, nil
}

// parseConfigDuration accepts either a bare Go duration token (30s) or a quoted
// one ("30s"), so [daemon].poll can be written in the natural TOML style either way.
func parseConfigDuration(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "\"") {
		unquoted, err := parseConfigString(value)
		if err != nil {
			return 0, err
		}
		value = strings.TrimSpace(unquoted)
	}
	return time.ParseDuration(value)
}

func validateDaemonRuntimeConfig(cfg DaemonRuntimeConfig) error {
	if cfg.PollSet && cfg.Poll <= 0 {
		return fmt.Errorf("[daemon].poll must be positive")
	}
	if cfg.WorkersSet && cfg.Workers <= 0 {
		return fmt.Errorf("[daemon].workers must be positive")
	}
	if cfg.SchedulerSet {
		switch cfg.Scheduler {
		case "barrier", "pool":
		default:
			return fmt.Errorf("unsupported [daemon].scheduler %q; use barrier or pool", cfg.Scheduler)
		}
	}
	return nil
}

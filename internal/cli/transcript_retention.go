package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/credgw"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/subprocess"
	"github.com/gitmoot/gitmoot/internal/transcript"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

const (
	transcriptRetentionInterval = 15 * time.Minute
	transcriptFinalizerGrace    = 10 * time.Minute
	transcriptSweepDeleteLimit  = 256
)

// openRetainedTranscriptLog is side-effect-free while capture is disabled or
// invalid. Enabled logs are canonical, private, and append-only across retries.
func openRetainedTranscriptLog(home, jobID string) (string, *os.File, error) {
	paths, err := pathsFromFlag(home)
	if err != nil {
		return "", nil, err
	}
	if !config.LoadTranscriptsConfig(paths).Enabled {
		return "", nil, nil
	}
	dir := filepath.Join(paths.Logs, "jobs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", nil, err
	}
	path := transcript.JobLogPath(paths.Logs, jobID)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return "", nil, err
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return "", nil, err
	}
	return path, file, nil
}

// appendDeliveryAdapterOutput adds a writer at the existing runner base instead
// of rebuilding an adapter. This preserves relay env, credential curation,
// model-gateway leases, Landlock wrappers, and any pipeline-progress tee.
func appendDeliveryAdapterOutput(adapter workflow.DeliveryAdapter, out io.Writer) (workflow.DeliveryAdapter, error) {
	if adapter == nil || out == nil {
		return adapter, nil
	}
	switch a := adapter.(type) {
	case modelGatewayRuntimeAdapter:
		inner, err := appendDeliveryAdapterOutput(a.Adapter, out)
		if err != nil {
			return nil, err
		}
		runtimeAdapter, ok := inner.(runtime.Adapter)
		if !ok {
			return nil, fmt.Errorf("transcript tee returned incompatible %T model-gateway adapter", inner)
		}
		a.Adapter = runtimeAdapter
		return a, nil
	case runtime.CodexAdapter:
		a.Runner = appendRuntimeOutputRunner(a.Runner, out)
		return a, nil
	case *runtime.CodexAdapter:
		a.Runner = appendRuntimeOutputRunner(a.Runner, out)
		return a, nil
	case runtime.ClaudeAdapter:
		a.Runner = appendRuntimeOutputRunner(a.Runner, out)
		return a, nil
	case *runtime.ClaudeAdapter:
		a.Runner = appendRuntimeOutputRunner(a.Runner, out)
		return a, nil
	case runtime.KimiAdapter:
		a.Runner = appendRuntimeOutputRunner(a.Runner, out)
		return a, nil
	case *runtime.KimiAdapter:
		a.Runner = appendRuntimeOutputRunner(a.Runner, out)
		return a, nil
	case runtime.KimiCLIAdapter:
		a.Runner = appendRuntimeOutputRunner(a.Runner, out)
		return a, nil
	case *runtime.KimiCLIAdapter:
		a.Runner = appendRuntimeOutputRunner(a.Runner, out)
		return a, nil
	case runtime.ShellAdapter:
		a.Runner = appendRuntimeOutputRunner(a.Runner, out)
		return a, nil
	case *runtime.ShellAdapter:
		a.Runner = appendRuntimeOutputRunner(a.Runner, out)
		return a, nil
	default:
		return nil, fmt.Errorf("transcript tee cannot wrap adapter %T", adapter)
	}
}

func appendRuntimeOutputRunner(runner subprocess.Runner, out io.Writer) subprocess.Runner {
	if runner == nil {
		return subprocess.TeeRunner{Inner: subprocess.GroupRunner{}, Out: runtimeOutputWriter(out)}
	}
	switch r := runner.(type) {
	case subprocess.TeeRunner:
		r.Out = runtimeOutputWriter(r.Out, out)
		return r
	case *subprocess.TeeRunner:
		copy := *r
		copy.Out = runtimeOutputWriter(copy.Out, out)
		return &copy
	case subprocess.EnvInjectingRunner:
		r.Inner = appendRuntimeOutputRunner(r.Inner, out)
		return r
	case *subprocess.EnvInjectingRunner:
		copy := *r
		copy.Inner = appendRuntimeOutputRunner(copy.Inner, out)
		return &copy
	case subprocess.WrappingRunner:
		r.Inner = appendRuntimeOutputRunner(r.Inner, out)
		return r
	case *subprocess.WrappingRunner:
		copy := *r
		copy.Inner = appendRuntimeOutputRunner(copy.Inner, out)
		return &copy
	case *credgw.Runner:
		copy := *r
		copy.Inner = appendRuntimeOutputRunner(copy.Inner, out)
		return &copy
	default:
		if stream, ok := runner.(subprocess.StreamRunner); ok {
			return subprocess.TeeRunner{Inner: stream, Out: runtimeOutputWriter(out)}
		}
		return runner
	}
}

type transcriptRetentionStats struct {
	Scanned int
	Removed int
	Errors  int
}

type transcriptRetentionFile struct {
	path     string
	size     int64
	oldest   time.Time
	expired  bool
	eligible bool
}

func startTranscriptRetentionLoop(ctx context.Context, paths config.Paths, store *db.Store, stdout io.Writer) {
	go func() {
		ticker := time.NewTicker(transcriptRetentionInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				if _, err := sweepTranscriptRetention(ctx, paths, store, now.UTC(), os.Remove); err != nil {
					writeLine(stdout, "transcript retention sweep failed: %v", err)
				}
			}
		}
	}()
}

func sweepTranscriptRetention(ctx context.Context, paths config.Paths, store *db.Store, now time.Time, remove func(string) error) (transcriptRetentionStats, error) {
	var stats transcriptRetentionStats
	cfg := config.LoadTranscriptsConfig(paths)
	if !cfg.Enabled {
		return stats, nil
	}
	jobs, err := store.ListTranscriptJobs(ctx)
	if err != nil {
		return stats, err
	}
	type jobRef struct {
		state   string
		updated time.Time
	}
	byName := make(map[string][]jobRef, len(jobs)*2)
	for _, job := range jobs {
		updated := parseTranscriptStoreTime(job.UpdatedAt)
		if updated.IsZero() {
			updated = parseTranscriptStoreTime(job.CreatedAt)
		}
		ref := jobRef{state: job.State, updated: updated}
		canonical := transcript.JobLogName(job.ID) + ".log"
		legacy := transcript.LegacyLogName(job.ID) + ".log"
		byName[canonical] = append(byName[canonical], ref)
		if legacy != canonical {
			byName[legacy] = append(byName[legacy], ref)
		}
	}
	dir := filepath.Join(paths.Logs, "jobs")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return stats, nil
		}
		return stats, err
	}
	files := make([]transcriptRetentionFile, 0, len(entries))
	var total int64
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".log") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			stats.Errors++
			continue
		}
		stats.Scanned++
		total += info.Size()
		file := transcriptRetentionFile{path: filepath.Join(dir, entry.Name()), size: info.Size(), oldest: info.ModTime()}
		refs := byName[entry.Name()]
		if len(refs) == 0 {
			file.expired = !info.ModTime().After(now.Add(-cfg.Retain))
			file.eligible = file.expired
			files = append(files, file)
			continue
		}
		settled := true
		latest := time.Time{}
		for _, ref := range refs {
			if !workflow.IsSettledJobState(ref.state) {
				settled = false
			}
			if ref.updated.After(latest) {
				latest = ref.updated
			}
		}
		file.oldest = latest
		graceProtected := latest.IsZero() || latest.After(now.Add(-transcriptFinalizerGrace))
		file.eligible = settled && !graceProtected
		file.expired = file.eligible && !latest.After(now.Add(-cfg.Retain))
		files = append(files, file)
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].oldest.Equal(files[j].oldest) {
			return files[i].path < files[j].path
		}
		return files[i].oldest.Before(files[j].oldest)
	})
	selected := make(map[string]bool)
	remaining := total
	selectFile := func(file transcriptRetentionFile) {
		if len(selected) >= transcriptSweepDeleteLimit || selected[file.path] {
			return
		}
		selected[file.path] = true
		remaining -= file.size
	}
	for _, file := range files {
		if file.expired {
			selectFile(file)
		}
	}
	if remaining > cfg.MaxTotalBytes {
		for _, file := range files {
			if remaining <= cfg.MaxTotalBytes || len(selected) >= transcriptSweepDeleteLimit {
				break
			}
			if file.eligible {
				selectFile(file)
			}
		}
	}
	for _, file := range files {
		if !selected[file.path] {
			continue
		}
		if err := remove(file.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			stats.Errors++
			continue
		}
		stats.Removed++
	}
	return stats, nil
}

func parseTranscriptStoreTime(value string) time.Time {
	value = strings.TrimSpace(value)
	for _, layout := range []string{"2006-01-02 15:04:05", time.RFC3339Nano, time.RFC3339} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC()
		}
	}
	return time.Time{}
}

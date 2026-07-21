package pipeline

import (
	"context"
	"encoding/json"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/subprocess"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

const (
	PipelineProgressThreshold = 60 * time.Second
	PipelineProgressInterval  = 30 * time.Second
	pipelineProgressLineBytes = 400
	PipelineProgressLineBytes = pipelineProgressLineBytes
)

type PipelineProgressEventPayload struct {
	Elapsed  string `json:"elapsed"`
	Activity string `json:"activity,omitempty"`
}

// PipelineProgressLineTracker is the live-output writer shared with TeeRunner.
// It retains only the most recent non-empty logical line. Sanitization happens
// before the line can reach storage or an operator surface.
type PipelineProgressLineTracker struct {
	mu      sync.Mutex
	pending string
	last    string
}

func (t *PipelineProgressLineTracker) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pending += string(p)
	parts := strings.FieldsFunc(t.pending, func(r rune) bool { return r == '\n' || r == '\r' })
	ended := len(t.pending) > 0 && (t.pending[len(t.pending)-1] == '\n' || t.pending[len(t.pending)-1] == '\r')
	complete := len(parts)
	if !ended && complete > 0 {
		complete--
	}
	for _, line := range parts[:complete] {
		if clean := SanitizePipelineProgressLine(line); clean != "" {
			t.last = clean
		}
	}
	if ended {
		t.pending = ""
	} else if len(parts) > 0 {
		t.pending = parts[len(parts)-1]
	} else if len(t.pending) > 8192 {
		t.pending = t.pending[len(t.pending)-8192:]
	}
	if len(t.pending) > 8192 {
		t.pending = t.pending[len(t.pending)-8192:]
	}
	return len(p), nil
}

func (t *PipelineProgressLineTracker) LastLine() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if clean := SanitizePipelineProgressLine(t.pending); clean != "" {
		return clean
	}
	return t.last
}

var pipelineProgressEscapeRE = regexp.MustCompile("\\x1b(?:\\[[0-?]*[ -/]*[@-~]|\\][^\\x07]*(?:\\x07|\\x1b\\\\|$))")

func SanitizePipelineProgressLine(value string) string {
	value = pipelineProgressEscapeRE.ReplaceAllString(value, "")
	var b strings.Builder
	for _, r := range value {
		switch {
		case r == '\t':
			b.WriteByte(' ')
		case unicode.IsControl(r):
			continue
		default:
			b.WriteRune(r)
		}
	}
	value = strings.TrimSpace(workflow.RedactCommentText(b.String()))
	return truncatePipelineProgressLine(value, pipelineProgressLineBytes)
}

func truncatePipelineProgressLine(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

// RuntimeOutputWriter is the one runtime-output composition point. Cockpit and
// progress share one MultiWriter beneath one SyncWriter, so concurrent stdout /
// stderr copies cannot race either destination.
func RuntimeOutputWriter(writers ...io.Writer) io.Writer {
	nonNil := make([]io.Writer, 0, len(writers))
	for _, writer := range writers {
		if writer != nil {
			nonNil = append(nonNil, writer)
		}
	}
	if len(nonNil) == 0 {
		return nil
	}
	if len(nonNil) == 1 {
		return subprocess.SyncWriter(nonNil[0])
	}
	return subprocess.SyncWriter(io.MultiWriter(nonNil...))
}

func PipelineProgressTicks(ctx context.Context, threshold, interval time.Duration) <-chan time.Time {
	out := make(chan time.Time, 1)
	go func() {
		defer close(out)
		timer := time.NewTimer(threshold)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case tick := <-timer.C:
			select {
			case out <- tick:
			case <-ctx.Done():
				return
			}
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case tick := <-ticker.C:
				select {
				case out <- tick:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out
}

func EmitPipelineProgress(ctx context.Context, store *db.Store, stdout io.Writer, jobID string, startedAt time.Time, tracker *PipelineProgressLineTracker, ticks <-chan time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case tick, ok := <-ticks:
			if !ok {
				return
			}
			elapsed := tick.Sub(startedAt)
			if elapsed < 0 {
				elapsed = 0
			}
			message, err := json.Marshal(PipelineProgressEventPayload{
				Elapsed:  elapsed.Round(time.Second).String(),
				Activity: tracker.LastLine(),
			})
			if err != nil {
				continue
			}
			if err := store.UpsertLatestJobEvent(ctx, db.JobEvent{JobID: jobID, Kind: "progress", Message: string(message)}); err != nil && ctx.Err() == nil {
				writeLine(stdout, "job %s progress event failed: %v", jobID, err)
			}
		}
	}
}

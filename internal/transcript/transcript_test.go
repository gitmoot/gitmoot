package transcript

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gitmootruntime "github.com/gitmoot/gitmoot/internal/runtime"
)

func TestRuntimeRendererGoldens(t *testing.T) {
	for _, tc := range []struct {
		runtime string
		input   string
		golden  string
	}{
		{runtime: "codex", input: "codex.jsonl", golden: "codex.golden"},
		{runtime: "claude", input: "claude.json", golden: "claude.golden"},
		{runtime: "kimi", input: "kimi.jsonl", golden: "kimi.golden"},
		{runtime: "shell", input: "shell.txt", golden: "shell.golden"},
		{runtime: "codex", input: "codex_tool_run.jsonl", golden: "codex_tool_run.golden"},
		{runtime: "kimi", input: "kimi_tool_run.jsonl", golden: "kimi_tool_run.golden"},
		{runtime: "claude", input: "claude_envelope_real.json", golden: "claude_envelope_real.golden"},
	} {
		t.Run(tc.runtime, func(t *testing.T) {
			translator, err := NewTranslator(tc.runtime)
			if err != nil {
				t.Fatal(err)
			}
			input := readTestdata(t, tc.input)
			var got bytes.Buffer
			renderer := NewRenderer(&got)
			scanner := bufio.NewScanner(strings.NewReader(input))
			scanner.Buffer(make([]byte, 0, 64*1024), MaxLogicalLineBytes)
			for scanner.Scan() {
				if err := renderer.Render(translator.Translate(scanner.Text())...); err != nil {
					t.Fatal(err)
				}
			}
			if err := scanner.Err(); err != nil {
				t.Fatal(err)
			}
			if err := renderer.Render(translator.Flush()...); err != nil {
				t.Fatal(err)
			}
			if want := readTestdata(t, tc.golden); got.String() != want {
				t.Fatalf("rendered transcript:\n%s\nwant:\n%s", got.String(), want)
			}
		})
	}
}

func TestUnknownAndMalformedLinesFailOpenWithRedaction(t *testing.T) {
	translator, err := NewTranslator("codex")
	if err != nil {
		t.Fatal(err)
	}
	lines := []string{
		`{"type":"future.event","token":"ghp_abcdefghijklmnopqrstuvwxyz"}`,
		`not json password=supersecretvalue123456789`,
		`{"type":"turn.started"}`,
	}
	var got bytes.Buffer
	renderer := NewRenderer(&got)
	for _, line := range lines {
		if err := renderer.Render(translator.Translate(line)...); err != nil {
			t.Fatal(err)
		}
	}
	text := got.String()
	if strings.Contains(text, "ghp_abcdefghijklmnopqrstuvwxyz") || strings.Contains(text, "supersecretvalue123456789") {
		t.Fatalf("unredacted transcript: %s", text)
	}
	if strings.Count(text, "[REDACTED]") != 2 || !strings.Contains(text, "turn: started") {
		t.Fatalf("renderer did not continue per line: %s", text)
	}
}

func TestRendererRedactsBeforeTruncationAndLabelsUsage(t *testing.T) {
	prefix := strings.Repeat("x", renderRawLimit-10)
	secret := "ghp_abcdefghijklmnopqrstuvwxyz"
	var got bytes.Buffer
	renderer := NewRenderer(&got)
	if err := renderer.Render(
		Event{Kind: KindRaw, RawLine: prefix + secret},
		Event{Kind: KindUsage, InputTokens: 7, OutputTokens: 3},
	); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got.String(), secret) || !strings.Contains(got.String(), "[REDACTED]") {
		t.Fatalf("redaction did not precede truncation: %q", got.String())
	}
	if !strings.Contains(got.String(), "latest reported usage") {
		t.Fatalf("usage label missing: %q", got.String())
	}
}

func TestPlainRendererByteStableWithRoundTwoMetadata(t *testing.T) {
	var got bytes.Buffer
	events := []Event{
		{Kind: KindToolCall, Name: "bash", ToolIcon: "$", Preview: PreviewTail, PreviewLines: 5, InputDigest: "echo ok"},
		{Kind: KindToolResult, Name: "bash", ToolIcon: "$", Preview: PreviewTail, PreviewLines: 5, Status: "completed", OutputDigest: "one\ntwo", Duration: 1200 * time.Millisecond},
		{Kind: KindUsage, InputTokens: 7, OutputTokens: 3, Duration: 2 * time.Second},
	}
	if err := NewRenderer(&got).Render(events...); err != nil {
		t.Fatal(err)
	}
	want := "▸ bash echo ok\n◂ completed one two\nusage (latest reported usage): in=7 out=3\n"
	if got.String() != want {
		t.Fatalf("plain round-two output = %q, want %q", got.String(), want)
	}
}

func TestClaudeMalformedLineDoesNotPoisonFinalEnvelope(t *testing.T) {
	translator, err := NewTranslator("claude")
	if err != nil {
		t.Fatal(err)
	}
	if events := translator.Translate("diagnostic before envelope"); len(events) != 0 {
		t.Fatalf("Claude rendered before final flush: %+v", events)
	}
	translator.Translate(`{"result":"final text","usage":{"input_tokens":9,"output_tokens":4}}`)
	var got bytes.Buffer
	if err := NewRenderer(&got).Render(translator.Flush()...); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"diagnostic before envelope", "final text", "in=9 out=4"} {
		if !strings.Contains(got.String(), want) {
			t.Fatalf("Claude transcript missing %q: %q", want, got.String())
		}
	}
}

func TestClaudeNonEnvelopeJSONRendersRaw(t *testing.T) {
	translator, err := NewTranslator("claude")
	if err != nil {
		t.Fatal(err)
	}
	line := `{"progress":"still thinking"}`
	translator.Translate(line)
	var got bytes.Buffer
	if err := NewRenderer(&got).Render(translator.Flush()...); err != nil {
		t.Fatal(err)
	}
	if got.String() != line+"\n" || strings.Contains(got.String(), "latest reported usage") || strings.Contains(got.String(), "\u25cf") {
		t.Fatalf("non-envelope Claude JSON rendered as a result: %q", got.String())
	}
}

func TestFileChangeDigestCapsCount(t *testing.T) {
	changes := make([]gitmootruntime.CodexFileChangeEntry, 10)
	for i := range changes {
		changes[i] = gitmootruntime.CodexFileChangeEntry{Kind: "modify", Path: fmt.Sprintf("file-%d", i)}
	}
	digest := fileChangeDigest(changes)
	if strings.Contains(digest, "file-8") || strings.Contains(digest, "file-9") || !strings.Contains(digest, "(+2 more)") {
		t.Fatalf("capped file-change digest = %q", digest)
	}
}

func TestLineBufferEverySplitAndByteAtATime(t *testing.T) {
	input := []byte("alpha\nbeta\nfinal")
	want := []string{"alpha", "beta", "final"}
	for split := 0; split <= len(input); split++ {
		var got []string
		buffer := newLineBuffer(MaxLogicalLineBytes, func(line string) error {
			got = append(got, line)
			return nil
		})
		if err := buffer.Write(input[:split]); err != nil {
			t.Fatal(err)
		}
		if err := buffer.Write(input[split:]); err != nil {
			t.Fatal(err)
		}
		if err := buffer.Flush(); err != nil {
			t.Fatal(err)
		}
		assertLines(t, got, want)
	}

	var got []string
	buffer := newLineBuffer(MaxLogicalLineBytes, func(line string) error {
		got = append(got, line)
		return nil
	})
	for _, ch := range input {
		if err := buffer.Write([]byte{ch}); err != nil {
			t.Fatal(err)
		}
	}
	if err := buffer.Flush(); err != nil {
		t.Fatal(err)
	}
	assertLines(t, got, want)
}

func TestLineBufferCapsOversizedLogicalLine(t *testing.T) {
	var got []string
	buffer := newLineBuffer(8, func(line string) error {
		got = append(got, line)
		return nil
	})
	if err := buffer.Write([]byte("123456789012\nnext\n")); err != nil {
		t.Fatal(err)
	}
	assertLines(t, got, []string{"12345678", "next"})
}

func TestFollowRetriesCreationAndFlushesFinalLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "later.log")
	var settled atomic.Bool
	var got []string
	done := make(chan error, 1)
	go func() {
		done <- Follow(context.Background(), path, FollowOptions{
			PollInterval: time.Millisecond,
			Settled:      func(context.Context) (bool, error) { return settled.Load(), nil },
		}, func(line string) error {
			got = append(got, line)
			return nil
		})
	}()
	time.Sleep(5 * time.Millisecond)
	if err := os.WriteFile(path, []byte("created\nfinal"), 0o600); err != nil {
		t.Fatal(err)
	}
	settled.Store(true)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Follow did not terminate")
	}
	assertLines(t, got, []string{"created", "final"})
}

func TestFollowDeletionMidFollowDrainsOpenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "delete.log")
	if err := os.WriteFile(path, []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var settled atomic.Bool
	var got []string
	firstLine := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		done <- Follow(context.Background(), path, FollowOptions{
			PollInterval: time.Millisecond,
			Settled:      func(context.Context) (bool, error) { return settled.Load(), nil },
		}, func(line string) error {
			got = append(got, line)
			select {
			case firstLine <- struct{}{}:
			default:
			}
			return nil
		})
	}()
	select {
	case <-firstLine:
	case <-time.After(time.Second):
		t.Fatal("Follow did not emit the first line")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	settled.Store(true)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	assertLines(t, got, []string{"before"})
}

func TestFollowSettlesThenDrainsToEOF(t *testing.T) {
	path := filepath.Join(t.TempDir(), "drain.log")
	if err := os.WriteFile(path, []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var polls atomic.Int32
	var got []string
	err := Follow(context.Background(), path, FollowOptions{
		PollInterval: time.Millisecond,
		Settled: func(context.Context) (bool, error) {
			if polls.Add(1) == 1 {
				file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					return false, err
				}
				_, writeErr := file.WriteString("after settle\nfinal")
				closeErr := file.Close()
				if writeErr != nil {
					return false, writeErr
				}
				if closeErr != nil {
					return false, closeErr
				}
				return true, nil
			}
			return true, nil
		},
	}, func(line string) error {
		got = append(got, line)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	assertLines(t, got, []string{"before", "after settle", "final"})
}

func TestFollowReopensReplacedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "replace.log")
	if err := os.WriteFile(path, []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var settled atomic.Bool
	var got []string
	lines := make(chan string, 4)
	done := make(chan error, 1)
	go func() {
		done <- Follow(context.Background(), path, FollowOptions{
			PollInterval: time.Millisecond,
			Settled:      func(context.Context) (bool, error) { return settled.Load(), nil },
		}, func(line string) error {
			got = append(got, line)
			lines <- line
			return nil
		})
	}()
	waitForLine(t, lines, "before")
	if err := os.Rename(path, filepath.Join(dir, "replace.old")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("after replacement\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitForLine(t, lines, "after replacement")
	settled.Store(true)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	assertLines(t, got, []string{"before", "after replacement"})
}

func TestFollowReopensInPlaceTruncate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "truncate.log")
	if err := os.WriteFile(path, []byte("before-long-line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var settled atomic.Bool
	var got []string
	lines := make(chan string, 4)
	done := make(chan error, 1)
	go func() {
		done <- Follow(context.Background(), path, FollowOptions{
			PollInterval: time.Millisecond,
			Settled:      func(context.Context) (bool, error) { return settled.Load(), nil },
		}, func(line string) error {
			got = append(got, line)
			lines <- line
			return nil
		})
	}()
	waitForLine(t, lines, "before-long-line")
	if err := os.WriteFile(path, []byte("after\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitForLine(t, lines, "after")
	settled.Store(true)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	assertLines(t, got, []string{"before-long-line", "after"})
}

func TestFollowFatalOpenAndReadCallbacks(t *testing.T) {
	err := Follow(context.Background(), filepath.Join(t.TempDir(), "missing"), FollowOptions{
		PollInterval: time.Millisecond,
		Settled:      func(context.Context) (bool, error) { return true, nil },
	}, func(string) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "open log") {
		t.Fatalf("missing log error = %v", err)
	}

	want := errors.New("render failed")
	path := filepath.Join(t.TempDir(), "render.log")
	if err := os.WriteFile(path, []byte("line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = Follow(context.Background(), path, FollowOptions{
		PollInterval: time.Millisecond,
		Settled:      func(context.Context) (bool, error) { return true, nil },
	}, func(string) error { return want })
	if !errors.Is(err, want) {
		t.Fatalf("callback error = %v, want %v", err, want)
	}
}

func readTestdata(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func assertLines(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("lines = %#v, want %#v", got, want)
	}
}

func waitForLine(t *testing.T, lines <-chan string, want string) {
	t.Helper()
	select {
	case got := <-lines:
		if got != want {
			t.Fatalf("line = %q, want %q", got, want)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for line %q", want)
	}
}

func TestStyledRendererGolden(t *testing.T) {
	translator, err := newTranslator("codex", func() time.Time { return time.Unix(100, 0) })
	if err != nil {
		t.Fatal(err)
	}
	input := readTestdata(t, "codex_tool_run.jsonl")
	var got bytes.Buffer
	renderer := NewStyledRenderer(&got)
	scanner := bufio.NewScanner(strings.NewReader(input))
	scanner.Buffer(make([]byte, 0, 64*1024), MaxLogicalLineBytes)
	for scanner.Scan() {
		if err := renderer.Render(translator.Translate(scanner.Text())...); err != nil {
			t.Fatal(err)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if err := renderer.Render(translator.Flush()...); err != nil {
		t.Fatal(err)
	}
	if want := readTestdata(t, "codex_tool_run_styled.golden"); got.String() != want {
		t.Fatalf("styled transcript:\n%q\nwant:\n%q", got.String(), want)
	}
}

func TestStyledOutputPreviewHeadTailAndShort(t *testing.T) {
	lines := func(prefix string, count int) string {
		parts := make([]string, count)
		for i := range parts {
			parts[i] = fmt.Sprintf("%s-%02d", prefix, i+1)
		}
		return strings.Join(parts, "\n")
	}
	for _, tc := range []struct {
		name    string
		event   Event
		want    []string
		notWant []string
	}{
		{
			name:    "shell tail",
			event:   Event{Kind: KindToolResult, Status: "completed", OutputDigest: lines("tail", 8), Preview: PreviewTail, PreviewLines: 5},
			want:    []string{"... (3 earlier lines)", "tail-04", "tail-08"},
			notWant: []string{"tail-01", "tail-03"},
		},
		{
			name:    "search head",
			event:   Event{Kind: KindToolResult, Status: "completed", OutputDigest: lines("head", 17), Preview: PreviewHead, PreviewLines: 15},
			want:    []string{"head-01", "head-15", "... (2 more lines)"},
			notWant: []string{"head-16", "head-17"},
		},
		{
			name:    "short unchanged",
			event:   Event{Kind: KindToolResult, Status: "completed", OutputDigest: "alpha\nbeta", Preview: PreviewHead, PreviewLines: 10},
			want:    []string{"alpha", "beta"},
			notWant: []string{"more lines", "earlier lines"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got bytes.Buffer
			if err := NewStyledRenderer(&got).Render(tc.event); err != nil {
				t.Fatal(err)
			}
			for _, want := range tc.want {
				if !strings.Contains(got.String(), want) {
					t.Fatalf("preview missing %q: %q", want, got.String())
				}
			}
			for _, notWant := range tc.notWant {
				if strings.Contains(got.String(), notWant) {
					t.Fatalf("preview unexpectedly contains %q: %q", notWant, got.String())
				}
			}
		})
	}
}

func TestToolPresentationTable(t *testing.T) {
	for _, tc := range []struct {
		name  string
		icon  string
		mode  PreviewMode
		lines int
	}{
		{"Bash", "$", PreviewTail, 5},
		{"read_file", "→", PreviewHead, 10},
		{"apply_patch", "←", PreviewHead, 10},
		{"grep", "✱", PreviewHead, 15},
		{"web_search", "◈", PreviewHead, 10},
		{"web_fetch", "%", PreviewHead, 10},
		{"future_tool", "⚙", PreviewHead, 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyTool(tc.name)
			if got.icon != tc.icon || got.preview != tc.mode || got.previewLines != tc.lines {
				t.Fatalf("classifyTool(%q) = %+v", tc.name, got)
			}
		})
	}
}

func TestCodexTranslatorThreadsToolDurationAndPresentation(t *testing.T) {
	times := []time.Time{time.Unix(100, 0), time.Unix(101, 200_000_000)}
	translator, err := newTranslator("codex", func() time.Time {
		got := times[0]
		times = times[1:]
		return got
	})
	if err != nil {
		t.Fatal(err)
	}
	started := translator.Translate(`{"type":"item.started","item":{"id":"tool-1","type":"command_execution","command":"bash -lc 'echo ok'","status":"in_progress"}}`)
	completed := translator.Translate(`{"type":"item.completed","item":{"id":"tool-1","type":"command_execution","command":"bash -lc 'echo ok'","aggregated_output":"ok\n","exit_code":0,"status":"completed"}}`)
	if len(started) != 1 || started[0].ToolIcon != "$" || started[0].Preview != PreviewTail || started[0].PreviewLines != 5 {
		t.Fatalf("started event = %+v", started)
	}
	if len(completed) != 1 || completed[0].Name != "bash" || completed[0].Duration != 1200*time.Millisecond {
		t.Fatalf("completed event = %+v", completed)
	}
}

func TestStyledOutputPreviewRedactsBeforeSelection(t *testing.T) {
	secret := "ghp_abcdefghijklmnopqrstuvwxyz"
	preview := styledOutputPreview("first\n"+secret, PreviewHead, 10)
	joined := strings.Join(preview, "\n")
	if strings.Contains(joined, secret) || !strings.Contains(joined, "[REDACTED]") {
		t.Fatalf("preview was not redacted: %q", joined)
	}
}

func TestStyledRendererDurationCancellationAndNarration(t *testing.T) {
	var got bytes.Buffer
	err := NewStyledRenderer(&got).Render(
		Event{Kind: KindAgentText, Text: "# Result\n- use `go test`\n1. verify"},
		Event{Kind: KindToolResult, Status: "cancelled", Duration: 1200 * time.Millisecond},
		Event{Kind: KindUsage, InputTokens: 1, OutputTokens: 2, Duration: 62 * time.Second},
	)
	if err != nil {
		t.Fatal(err)
	}
	out := got.String()
	for _, want := range []string{
		"\x1b[1mResult\x1b[0m",
		"\x1b[36m- \x1b[0muse `\x1b[36mgo test\x1b[0m`",
		"\x1b[36m1. \x1b[0mverify",
		"\x1b[33m◂ cancelled\x1b[0m",
		"Took 1.2s (cancelled)",
		"· Took 1m02s",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("styled output missing %q: %q", want, out)
		}
	}
}

func TestMarkdownRendererGolden(t *testing.T) {
	header := Header{Action: "ask", Agent: "audit", Runtime: "codex", Model: "gpt-test", Workflow: "demo", Prompt: "Check `status`."}
	events := []Event{
		{Kind: KindLifecycle, Phase: "thread", Detail: "started"},
		{Kind: KindLifecycle, Phase: "turn", Detail: "started"},
		{Kind: KindAgentText, Text: "I will \x1b[31minspect\x1b[0m it."},
		{Kind: KindToolCall, Name: "bash", InputDigest: "echo status"},
		{Kind: KindToolResult, Name: "bash", Status: "completed (exit 0)", OutputDigest: "status\n```embedded", Duration: 1200 * time.Millisecond},
		{Kind: KindAgentText, Text: "Done."},
		{Kind: KindUsage, InputTokens: 1234, OutputTokens: 42, Duration: 2500 * time.Millisecond},
	}
	var got bytes.Buffer
	if err := RenderMarkdown(&got, header, events); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got.String(), "\x1b[") {
		t.Fatalf("markdown contains ANSI: %q", got.String())
	}
	if want := readTestdata(t, "codex_tool_run.md.golden"); got.String() != want {
		t.Fatalf("markdown transcript:\n%s\nwant:\n%s", got.String(), want)
	}
}

func TestStyledRendererFailureIsRedAndTextKeepsLines(t *testing.T) {
	var got bytes.Buffer
	renderer := NewStyledRenderer(&got)
	if err := renderer.Render(
		Event{Kind: KindAgentText, Text: "first line\nsecond line"},
		Event{Kind: KindToolResult, Status: "failed (exit 1)", OutputDigest: "boom"},
	); err != nil {
		t.Fatal(err)
	}
	out := got.String()
	if !strings.Contains(out, "● first line\n  second line\n") {
		t.Fatalf("agent text lost its line break: %q", out)
	}
	if !strings.Contains(out, "\x1b[31m◂ failed (exit 1)") {
		t.Fatalf("failed result not red: %q", out)
	}
	if !strings.Contains(out, "\x1b[90m  ↳ boom") {
		t.Fatalf("output digest not dim/indented: %q", out)
	}
}

func TestFormatTokens(t *testing.T) {
	for _, tc := range []struct {
		in   int
		want string
	}{
		{812, "812"}, {1234, "1.2k"}, {42_500, "42k"}, {1_740_206, "1.7M"}, {12_345_678, "12M"},
	} {
		if got := formatTokens(tc.in); got != tc.want {
			t.Fatalf("formatTokens(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRenderHeaderPlainAndStyled(t *testing.T) {
	full := Header{Action: "ask", Agent: "planner", Runtime: "codex", Model: "gpt-5.5", Workflow: "demo", Prompt: "do the thing"}
	var plain bytes.Buffer
	if err := NewRenderer(&plain).RenderHeader(full); err != nil {
		t.Fatal(err)
	}
	want := "▶ ask · planner · codex/gpt-5.5 · workflow demo\n  do the thing\n\n"
	if plain.String() != want {
		t.Fatalf("plain header = %q, want %q", plain.String(), want)
	}

	var bare bytes.Buffer
	if err := NewRenderer(&bare).RenderHeader(Header{Action: "review", Agent: "a", Runtime: "shell"}); err != nil {
		t.Fatal(err)
	}
	if bare.String() != "▶ review · a · shell\n\n" {
		t.Fatalf("bare header = %q", bare.String())
	}

	var styled bytes.Buffer
	if err := NewStyledRenderer(&styled).RenderHeader(full); err != nil {
		t.Fatal(err)
	}
	out := styled.String()
	if !strings.Contains(out, "\x1b[90m▶ ask · planner · codex/gpt-5.5 · workflow demo\x1b[0m") {
		t.Fatalf("styled meta not dim: %q", out)
	}
	if !strings.Contains(out, "\n  do the thing\n") {
		t.Fatalf("styled prompt missing: %q", out)
	}
}

func TestRenderHeaderTruncatesAndRedactsPrompt(t *testing.T) {
	long := "token=ghp_1234567890abcdefghijklmnopqrstuvwxyz " + strings.Repeat("x", 600)
	var got bytes.Buffer
	if err := NewRenderer(&got).RenderHeader(Header{Action: "ask", Agent: "a", Runtime: "codex", Prompt: long}); err != nil {
		t.Fatal(err)
	}
	out := got.String()
	if strings.Contains(out, "ghp_") {
		t.Fatalf("secret leaked into header: %q", out)
	}
	if !strings.Contains(out, "more chars)") {
		t.Fatalf("missing truncation marker: %q", out)
	}
	if len(out) > headerPromptLimit+200 {
		t.Fatalf("header too long: %d bytes", len(out))
	}
}

package transcript

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestJobLogPathCollisionAndLegacyLookup(t *testing.T) {
	logs := t.TempDir()
	left := JobLogPath(logs, "a/b")
	right := JobLogPath(logs, "a_b")
	if left == right {
		t.Fatalf("collision-proof paths collided: %s", left)
	}
	legacy := LegacyJobLogPath(logs, "a/b")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte("legacy"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ResolveJobLogPath(logs, "a/b"); got != legacy {
		t.Fatalf("ResolveJobLogPath = %s, want legacy %s", got, legacy)
	}
}

func TestExportJSONLExactSchemaAndRedaction(t *testing.T) {
	secret := "ghp_123456789012345678901234567890"
	events := []Event{
		{Kind: KindToolCall, Name: "exec " + secret, InputDigest: "token=" + secret},
		{Kind: KindToolResult, Name: "exec", Status: "failed " + secret, OutputDigest: secret},
		{Kind: KindLifecycle, Phase: "turn", Detail: secret},
		{Kind: KindRaw, RawLine: secret},
	}
	var got bytes.Buffer
	metadata := ExportMetadata{JobID: "j", RootJobID: "j", Runtime: "codex", Agent: "a", Action: "ask", Repo: "o/r", Outcome: "failed", AttemptCount: 2, CreatedAt: "created", EndedAt: "ended"}
	if err := ExportJSONL(&got, metadata, events); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got.String(), secret) || !strings.Contains(got.String(), "[REDACTED]") {
		t.Fatalf("export redaction failed: %s", got.String())
	}
	for i, line := range strings.Split(strings.TrimSpace(got.String()), "\n") {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("row %d: %v", i, err)
		}
		if len(row) != 22 || row["schema_version"] != float64(1) || row["step_index"] != float64(i) {
			t.Fatalf("row %d schema = %#v", i, row)
		}
		if !strings.HasPrefix(line, `{"schema_version":1,`) {
			t.Fatalf("schema_version not first: %s", line)
		}
	}
}

func TestReadSnapshotEventsOversizedLineContinues(t *testing.T) {
	input := strings.Repeat("x", MaxLogicalLineBytes+100) + "\nsecond\n"
	translator, err := NewSnapshotTranslator("shell")
	if err != nil {
		t.Fatal(err)
	}
	events, err := ReadSnapshotEvents(strings.NewReader(input), translator)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Kind != KindRaw || !strings.Contains(events[0].RawLine, "[truncated:") || events[1].RawLine != "second" {
		t.Fatalf("events = %#v", events)
	}
}

func TestSnapshotTranslatorRealFixturesExportJSONL(t *testing.T) {
	for _, tc := range []struct{ runtime, file string }{
		{"codex", "codex_tool_run.jsonl"},
		{"kimi", "kimi_tool_run.jsonl"},
		{"claude", "claude_envelope_real.json"},
	} {
		t.Run(tc.runtime, func(t *testing.T) {
			input, err := os.ReadFile(filepath.Join("testdata", tc.file))
			if err != nil {
				t.Fatal(err)
			}
			translator, err := NewSnapshotTranslator(tc.runtime)
			if err != nil {
				t.Fatal(err)
			}
			events, err := ReadSnapshotEvents(bytes.NewReader(input), translator)
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			if err := ExportJSONL(&output, ExportMetadata{JobID: "fixture", RootJobID: "fixture", Runtime: tc.runtime, AttemptCount: 1}, events); err != nil {
				t.Fatal(err)
			}
			for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
				if !json.Valid([]byte(line)) {
					t.Fatalf("invalid JSONL row: %s", line)
				}
			}
		})
	}
}

func TestExportPreservesIdentifiersWhileMaskingText(t *testing.T) {
	metadata := ExportMetadata{JobID: "local-ask-okbot-18c2d1ad11b07a07", RootJobID: "local-ask-okbot-18c2d1ad11b07a07", Runtime: "shell", Agent: "okbot", Action: "ask", Repo: "e2e/toy", Outcome: "succeeded"}
	row := exportRow(metadata, 0, SanitizeEvent(Event{Kind: KindRaw, RawLine: "summary with token ghp_abcdefghijklmnopqrstuvwxyz123456789012"}))
	if row.JobID != "local-ask-okbot-18c2d1ad11b07a07" {
		t.Fatalf("job id mangled: %q", row.JobID)
	}
	if !strings.Contains(row.Text, "[REDACTED]") || strings.Contains(row.Text, "ghp_abcdefghijk") {
		t.Fatalf("text not masked: %q", row.Text)
	}
}

package pipeline

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/gitmoot/gitmoot/internal/db"
)

func TestBuildPipelineShellStageUpstreamContextDeterministicJSON(t *testing.T) {
	stage := Stage{ID: "consume", Cmd: "consume", Needs: []string{" z ", "a", "z"}}
	byID := map[string]db.PipelineRunStage{
		"z": {StageID: "z", State: StageSucceeded, Summary: "line one\nline two with `ticks` and \"quotes\"\x00"},
		"a": {StageID: "a", State: StageSucceeded, Summary: "snowman ☃"},
	}

	got := buildPipelineShellStageUpstreamContext(stage, byID)
	want := `{"schema_version":1,"complete":true,"stages":{"a":{"id":"a","state":"succeeded","summary":"snowman ☃","summary_truncated":false},"z":{"id":"z","state":"succeeded","summary":"line one\nline two with ` + "`ticks`" + ` and \"quotes\"\u0000","summary_truncated":false}}}`
	if got != want {
		t.Fatalf("context bytes differ:\n got: %s\nwant: %s", got, want)
	}
	if again := buildPipelineShellStageUpstreamContext(stage, byID); again != got {
		t.Fatalf("context is not deterministic:\nfirst:  %s\nsecond: %s", got, again)
	}
}

func TestBuildPipelineShellStageUpstreamContextCapsAndFlags(t *testing.T) {
	t.Run("rune-safe per-summary truncation", func(t *testing.T) {
		stage := Stage{ID: "consume", Cmd: "consume", Needs: []string{"source"}}
		got := buildPipelineShellStageUpstreamContext(stage, map[string]db.PipelineRunStage{
			"source": {StageID: "source", State: StageSucceeded, Summary: strings.Repeat("界", maxPipelineShellUpstreamSummaryBytes)},
		})
		var decoded pipelineShellUpstreamContext
		if err := json.Unmarshal([]byte(got), &decoded); err != nil {
			t.Fatalf("Unmarshal: %v\n%s", err, got)
		}
		source := decoded.Stages["source"]
		marshaledSummary, err := json.Marshal(source.Summary)
		if err != nil {
			t.Fatal(err)
		}
		if len(marshaledSummary) > maxPipelineShellUpstreamSummaryBytes || !utf8.ValidString(source.Summary) {
			t.Fatalf("marshaled summary len=%d validUTF8=%v", len(marshaledSummary), utf8.ValidString(source.Summary))
		}
		if !source.SummaryTruncated || decoded.Complete {
			t.Fatalf("truncation flags = stage:%v complete:%v", source.SummaryTruncated, decoded.Complete)
		}
	})

	t.Run("escaping expansion truncates instead of omitting", func(t *testing.T) {
		stage := Stage{ID: "consume", Cmd: "consume", Needs: []string{"source"}}
		got := buildPipelineShellStageUpstreamContext(stage, map[string]db.PipelineRunStage{
			"source": {StageID: "source", State: StageSucceeded, Summary: strings.Repeat("\x00", 12_000)},
		})
		var decoded pipelineShellUpstreamContext
		if err := json.Unmarshal([]byte(got), &decoded); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		source, ok := decoded.Stages["source"]
		if !ok {
			t.Fatalf("escape-heavy source was omitted: %s", got)
		}
		marshaledSummary, err := json.Marshal(source.Summary)
		if err != nil {
			t.Fatal(err)
		}
		if len(marshaledSummary) > maxPipelineShellUpstreamSummaryBytes {
			t.Fatalf("marshaled summary len=%d exceeds cap=%d", len(marshaledSummary), maxPipelineShellUpstreamSummaryBytes)
		}
		if !source.SummaryTruncated || decoded.Complete {
			t.Fatalf("escape truncation flags = stage:%v complete:%v", source.SummaryTruncated, decoded.Complete)
		}
	})

	t.Run("marshaled exact boundary", func(t *testing.T) {
		stage := Stage{ID: "consume", Cmd: "consume", Needs: []string{"source"}}
		for _, tc := range []struct {
			name          string
			summaryBytes  int
			wantTruncated bool
		}{
			{name: "exact", summaryBytes: maxPipelineShellUpstreamSummaryBytes - 2},
			{name: "one byte over", summaryBytes: maxPipelineShellUpstreamSummaryBytes - 1, wantTruncated: true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				got := buildPipelineShellStageUpstreamContext(stage, map[string]db.PipelineRunStage{
					"source": {StageID: "source", State: StageSucceeded, Summary: strings.Repeat("a", tc.summaryBytes)},
				})
				var decoded pipelineShellUpstreamContext
				if err := json.Unmarshal([]byte(got), &decoded); err != nil {
					t.Fatal(err)
				}
				source := decoded.Stages["source"]
				marshaledSummary, err := json.Marshal(source.Summary)
				if err != nil {
					t.Fatal(err)
				}
				if len(marshaledSummary) != maxPipelineShellUpstreamSummaryBytes {
					t.Fatalf("marshaled summary len=%d, want exact boundary %d", len(marshaledSummary), maxPipelineShellUpstreamSummaryBytes)
				}
				if source.SummaryTruncated != tc.wantTruncated || decoded.Complete == tc.wantTruncated {
					t.Fatalf("boundary flags = stage:%v complete:%v", source.SummaryTruncated, decoded.Complete)
				}
			})
		}
	})

	t.Run("final marshaled byte cap omits expected stages", func(t *testing.T) {
		stage := Stage{ID: "consume", Cmd: "consume"}
		byID := make(map[string]db.PipelineRunStage)
		for i := 0; i < 5; i++ {
			id := fmt.Sprintf("source-%d", i)
			stage.Needs = append(stage.Needs, id)
			byID[id] = db.PipelineRunStage{StageID: id, State: StageSucceeded, Summary: strings.Repeat(string(rune('a'+i)), maxPipelineShellUpstreamSummaryBytes-2)}
		}
		got := buildPipelineShellStageUpstreamContext(stage, byID)
		if len(got) > maxPipelineShellUpstreamContextBytes {
			t.Fatalf("marshaled context len=%d exceeds cap=%d", len(got), maxPipelineShellUpstreamContextBytes)
		}
		var decoded pipelineShellUpstreamContext
		if err := json.Unmarshal([]byte(got), &decoded); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if decoded.Complete || len(decoded.Stages) >= len(stage.Needs) {
			t.Fatalf("complete=%v stages=%d, want incomplete with omissions", decoded.Complete, len(decoded.Stages))
		}
		for id, source := range decoded.Stages {
			if source.SummaryTruncated {
				t.Fatalf("stage %s was per-summary truncated; total-cap case must test omission", id)
			}
		}
	})
}

func TestBuildPipelineShellStageUpstreamContextEmptyAndMissingNeeds(t *testing.T) {
	if got := buildPipelineShellStageUpstreamContext(Stage{ID: "root", Cmd: "true"}, nil); got != "" {
		t.Fatalf("root context = %q, want empty", got)
	}
	if got := buildPipelineShellStageUpstreamContext(Stage{ID: "agent", Agent: "a", Needs: []string{"source"}}, nil); got != "" {
		t.Fatalf("agent context = %q, want empty", got)
	}
	got := buildPipelineShellStageUpstreamContext(Stage{ID: "consume", Cmd: "true", Needs: []string{"source"}}, nil)
	if got != `{"schema_version":1,"complete":false,"stages":{}}` {
		t.Fatalf("missing-upstream context = %s", got)
	}
}

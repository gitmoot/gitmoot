package transcript

import (
	"strings"
	"testing"

	gitmootruntime "github.com/gitmoot/gitmoot/internal/runtime"
)

// TestEveryRegisteredRuntimeHasATranslator is the tripwire for the gap omp (#1428)
// was the first runtime to open: newTranslator's default branch is an ERROR, and
// exportAllJobTranscripts builds a snapshot translator INSIDE its per-job loop and
// returns that error — so a single job of a translator-less runtime aborts
// `gitmoot job transcript --all` for every OTHER job in the store, mid-stream.
// Registration is therefore not complete until the runtime has a case here, even a
// raw passthrough one.
func TestEveryRegisteredRuntimeHasATranslator(t *testing.T) {
	names := gitmootruntime.SupportedRuntimes()
	if len(names) == 0 {
		t.Fatal("SupportedRuntimes is empty; this test would pass vacuously")
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			if _, err := NewTranslator(name); err != nil {
				t.Fatalf("NewTranslator(%q) = %v; `job watch --transcript` would error for this runtime", name, err)
			}
			if _, err := NewSnapshotTranslator(name); err != nil {
				t.Fatalf("NewSnapshotTranslator(%q) = %v; one such job aborts `job transcript --all` for every other job", name, err)
			}
		})
	}
}

// TestOmpTranslatorPassesRawNDJSONThrough pins what omp's interim translator is: a
// passthrough that emits the retained bytes as raw events, one per line, and
// invents no tool/usage decoration it cannot honestly derive yet. The real decoding
// translator is a named follow-up; this asserts the interim neither errors nor
// silently drops lines.
func TestOmpTranslatorPassesRawNDJSONThrough(t *testing.T) {
	lines := []string{
		`{"type":"session","id":"5b0f3f8a-1f4d-4a9e-9a34-3f0f2f7b1c2d"}`,
		`{"type":"message_end","message":{"role":"assistant","content":"done","usage":{"input":10,"output":5}}}`,
		`{"type":"agent_end"}`,
	}
	translator, err := NewSnapshotTranslator(gitmootruntime.OmpRuntime)
	if err != nil {
		t.Fatal(err)
	}
	events, err := ReadSnapshotEvents(strings.NewReader(strings.Join(lines, "\n")+"\n"), translator)
	if err != nil {
		t.Fatal(err)
	}
	events = append(events, translator.Flush()...)
	if len(events) != len(lines) {
		t.Fatalf("events = %#v, want one per NDJSON line (%d)", events, len(lines))
	}
	for i, event := range events {
		if event.Kind != KindRaw {
			t.Fatalf("event %d kind = %v, want KindRaw (omp decoding is a follow-up, not a silent guess)", i, event.Kind)
		}
		if event.RawLine != lines[i] {
			t.Fatalf("event %d raw = %q, want %q", i, event.RawLine, lines[i])
		}
	}
}

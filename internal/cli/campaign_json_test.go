package cli

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// The message --json outputs rename the campaign and source-run keys in the
// CLI (#2602) while db.Message keeps its stored tags; a regression back to
// serializing the raw db struct would bring the old keys back.
func TestMessageJSONUsesCampaignAndRunKeys(t *testing.T) {
	message := db.Message{ID: 7, Sender: "a", Recipient: "b", Body: "hi", WorkflowID: "ops/rename", SourceJobID: "local-review-x-1"}
	for name, value := range map[string]any{
		"one":  newMessageJSON(message),
		"list": newMessagesJSON([]db.Message{message}),
	} {
		var buf bytes.Buffer
		if err := writeJSON(&buf, value); err != nil {
			t.Fatal(err)
		}
		var decoded any
		if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
			t.Fatal(err)
		}
		if list, ok := decoded.([]any); ok {
			decoded = list[0]
		}
		got := decoded.(map[string]any)
		if got["campaign"] != "ops/rename" || got["source_run_id"] != "local-review-x-1" {
			t.Fatalf("%s: new keys missing or wrong: %s", name, buf.String())
		}
		for _, old := range []string{"workflow", "source_job_id"} {
			if _, present := got[old]; present {
				t.Fatalf("%s: old key %q still emitted: %s", name, old, buf.String())
			}
		}
		if got["id"] != float64(7) || got["message"] != "hi" {
			t.Fatalf("%s: unrenamed keys changed: %s", name, buf.String())
		}
	}
	raw, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	var stored map[string]any
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	if stored["workflow"] != "ops/rename" || stored["source_job_id"] != "local-review-x-1" {
		t.Fatalf("db.Message tags changed: %s", raw)
	}
}

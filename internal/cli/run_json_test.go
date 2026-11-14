package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// Cleanup --json renames owner_job_id at every depth (#2601): the inspection,
// its nested obligation, receipt and consumers, and apply's decision and
// receipts. The stored structs keep their tags.
func TestCleanupJSONHasNoOwnerJobIDAtAnyDepth(t *testing.T) {
	obligation := db.CleanupObligation{ResourceID: "res-1", OwnerJobID: "local-review-a-1"}
	inspection := workflow.CleanupInspection{
		ResourceID: "res-1",
		OwnerJobID: "local-review-a-1",
		Obligation: &obligation,
		Receipt:    &db.CleanupReceipt{OwnerJobID: "local-review-a-1"},
		Consumers:  []workflow.CleanupConsumer{{JobID: "local-review-b-1"}},
	}
	apply := workflow.CleanupApplyResult{
		Decision:       inspection,
		Receipt:        &db.CleanupReceipt{OwnerJobID: "local-review-a-1"},
		RefusalReceipt: &db.CleanupRefusal{OwnerJobID: "local-review-a-1"},
	}
	for name, value := range map[string]any{
		"inspect": newCleanupInspectionJSON(inspection),
		"apply":   newCleanupApplyResultJSON(apply),
		"list":    newCleanupObligationsJSON([]db.CleanupObligation{obligation}),
	} {
		var buf bytes.Buffer
		if err := writeJSON(&buf, value); err != nil {
			t.Fatal(err)
		}
		out := buf.String()
		if strings.Contains(out, `"owner_job_id"`) || strings.Contains(out, `"job_id"`) {
			t.Fatalf("%s: old key still emitted:\n%s", name, out)
		}
		if !strings.Contains(out, `"owner_run_id": "local-review-a-1"`) {
			t.Fatalf("%s: owner_run_id missing:\n%s", name, out)
		}
	}
	var inspected map[string]any
	var buf bytes.Buffer
	if err := writeJSON(&buf, newCleanupInspectionJSON(inspection)); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(buf.Bytes(), &inspected); err != nil {
		t.Fatal(err)
	}
	nested, ok := inspected["obligation"].(map[string]any)
	if !ok || nested["owner_run_id"] != "local-review-a-1" || nested["resource_id"] != "res-1" {
		t.Fatalf("nested obligation not projected: %s", buf.String())
	}
	buf.Reset()
	if err := writeJSON(&buf, newCleanupInspectionJSON(workflow.CleanupInspection{ResourceID: "res-2"})); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), `"obligation"`) {
		t.Fatalf("nil obligation must stay omitted: %s", buf.String())
	}
	raw, err := json.Marshal(obligation)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"owner_job_id"`) {
		t.Fatalf("db.CleanupObligation tag changed: %s", raw)
	}
}

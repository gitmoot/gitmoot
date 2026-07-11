package activepieces

import (
	"encoding/json"
	"testing"
)

func TestEmbeddedTemplatesAreValidFlowObjects(t *testing.T) {
	templates, err := Templates()
	if err != nil {
		t.Fatalf("Templates: %v", err)
	}
	if len(templates) != 2 {
		t.Fatalf("template count = %d, want 2", len(templates))
	}
	for _, template := range templates {
		t.Run(template.ID, func(t *testing.T) {
			var flow struct {
				DisplayName   string          `json:"displayName"`
				SchemaVersion string          `json:"schemaVersion"`
				Trigger       json.RawMessage `json:"trigger"`
			}
			if err := json.Unmarshal(template.Flow, &flow); err != nil {
				t.Fatalf("unmarshal flow: %v", err)
			}
			if flow.DisplayName == "" || flow.SchemaVersion != "20" || len(flow.Trigger) == 0 {
				t.Fatalf("invalid flow envelope: %+v", flow)
			}
			var trigger map[string]any
			if err := json.Unmarshal(flow.Trigger, &trigger); err != nil {
				t.Fatalf("unmarshal trigger: %v", err)
			}
			if trigger["type"] == "" {
				t.Fatal("trigger type is empty")
			}
			assertGitmootActions(t, trigger)
		})
	}
}

func assertGitmootActions(t *testing.T, trigger map[string]any) {
	t.Helper()
	found := 0
	for step, ok := trigger["nextAction"].(map[string]any); ok; step, ok = step["nextAction"].(map[string]any) {
		settings, _ := step["settings"].(map[string]any)
		if settings["pieceName"] != "@gitmoot/piece-gitmoot" {
			continue
		}
		found++
		input, _ := settings["input"].(map[string]any)
		if input["auth"] == "" {
			t.Fatal("Gitmoot action input has no auth connection")
		}
		if settings["actionName"] == "ask_agent" && input["repo"] == "" {
			t.Fatal("ask_agent action input has no repo")
		}
	}
	if found == 0 {
		t.Fatal("template contains no Gitmoot action")
	}
}

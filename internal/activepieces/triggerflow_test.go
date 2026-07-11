package activepieces

import (
	"encoding/json"
	"testing"

	"github.com/jerryfane/gitmoot/internal/pipeline"
)

func TestBuildTriggerFlow(t *testing.T) {
	displayName, raw, err := BuildTriggerFlow("mail-triage", pipeline.Trigger{Kind: "email", Connection: "gmail-imap", Mailbox: "Alerts"}, "gitmoot-bridge", "0.4.3", "0.1.3")
	if err != nil {
		t.Fatal(err)
	}
	if displayName != "gitmoot: mail-triage" {
		t.Fatalf("displayName = %q", displayName)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	trigger := got["trigger"].(map[string]any)
	settings := trigger["settings"].(map[string]any)
	if got["schemaVersion"] != "20" || trigger["type"] != "PIECE_TRIGGER" || settings["pieceName"] != "@activepieces/piece-imap" || settings["pieceVersion"] != "0.4.3" || settings["triggerName"] != "new_email" {
		t.Fatalf("trigger shape = %+v", trigger)
	}
	input := settings["input"].(map[string]any)
	if input["auth"] != "{{connections['gmail-imap']}}" || input["mailbox"] != "Alerts" {
		t.Fatalf("trigger input = %+v", input)
	}
	action := trigger["nextAction"].(map[string]any)
	actionSettings := action["settings"].(map[string]any)
	actionInput := actionSettings["input"].(map[string]any)
	if action["type"] != "PIECE" || actionSettings["pieceName"] != "@gitmoot/piece-gitmoot" || actionSettings["pieceVersion"] != "0.1.3" || actionSettings["actionName"] != "run_pipeline" {
		t.Fatalf("action shape = %+v", action)
	}
	if len(actionInput) != 2 || actionInput["pipeline_name"] != "mail-triage" || actionInput["auth"] != "{{connections['gitmoot-bridge']}}" {
		t.Fatalf("action input = %+v", actionInput)
	}
	if options, ok := actionSettings["errorHandlingOptions"].(map[string]any); !ok || len(options) != 0 {
		t.Fatalf("errorHandlingOptions = %#v, want empty object", actionSettings["errorHandlingOptions"])
	}

	// Cross-check the generated trigger settings against the embedded AP 0.82
	// starter template rather than duplicating the external shape in this test.
	templates, err := Templates()
	if err != nil {
		t.Fatal(err)
	}
	var template map[string]any
	for _, candidate := range templates {
		if candidate.ID == "gmail-imap-ask-agent" {
			if err := json.Unmarshal(candidate.Flow, &template); err != nil {
				t.Fatal(err)
			}
		}
	}
	templateTrigger := template["trigger"].(map[string]any)
	templateSettings := templateTrigger["settings"].(map[string]any)
	for _, key := range []string{"type"} {
		if trigger[key] != templateTrigger[key] {
			t.Fatalf("trigger %s = %v, template = %v", key, trigger[key], templateTrigger[key])
		}
	}
	for _, key := range []string{"pieceName", "triggerName"} {
		if settings[key] != templateSettings[key] {
			t.Fatalf("settings %s = %v, template = %v", key, settings[key], templateSettings[key])
		}
	}
}

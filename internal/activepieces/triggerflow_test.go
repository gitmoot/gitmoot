package activepieces

import (
	"bytes"
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

func TestBuildTriggerFlowGoldenWithAndWithoutMap(t *testing.T) {
	cases := []struct {
		name    string
		trigger pipeline.Trigger
		version string
		want    string
	}{
		{
			name:    "without map",
			trigger: pipeline.Trigger{Kind: "email", Connection: "gmail-imap", Mailbox: "Alerts"},
			version: "0.1.3",
			want: `{
  "displayName":"gitmoot: mail-triage","schemaVersion":"20",
  "trigger":{"name":"trigger","valid":true,"displayName":"Receive email over IMAP","type":"PIECE_TRIGGER","lastUpdatedDate":"2026-01-01T00:00:00.000Z",
    "settings":{"pieceName":"@activepieces/piece-imap","pieceVersion":"0.4.3","triggerName":"new_email","input":{"auth":"{{connections['gmail-imap']}}","mailbox":"Alerts"},"propertySettings":{}},
    "nextAction":{"name":"step_1","valid":true,"displayName":"Run Gitmoot pipeline","type":"PIECE","settings":{"pieceName":"@gitmoot/piece-gitmoot","pieceVersion":"0.1.3","actionName":"run_pipeline","input":{"auth":"{{connections['gitmoot-bridge']}}","pipeline_name":"mail-triage"},"propertySettings":{},"errorHandlingOptions":{}}}
  }
}`,
		},
		{
			name: "with map",
			trigger: pipeline.Trigger{Kind: "email", Connection: "gmail-imap", Mailbox: "Alerts", Map: map[string]string{
				"subject": "subject", "sender": "from_address", "body": "text", "message_id": "message_id", "received_at": "date",
			}},
			version: "0.1.4",
			want: `{
  "displayName":"gitmoot: mail-triage","schemaVersion":"20",
  "trigger":{"name":"trigger","valid":true,"displayName":"Receive email over IMAP","type":"PIECE_TRIGGER","lastUpdatedDate":"2026-01-01T00:00:00.000Z",
    "settings":{"pieceName":"@activepieces/piece-imap","pieceVersion":"0.4.3","triggerName":"new_email","input":{"auth":"{{connections['gmail-imap']}}","mailbox":"Alerts"},"propertySettings":{}},
    "nextAction":{"name":"step_1","valid":true,"displayName":"Run Gitmoot pipeline","type":"PIECE","settings":{"pieceName":"@gitmoot/piece-gitmoot","pieceVersion":"0.1.4","actionName":"run_pipeline","input":{"auth":"{{connections['gitmoot-bridge']}}","payload":{"body":"{{trigger['text']}}","message_id":"{{trigger['messageId']}}","received_at":"{{trigger['date']}}","sender":"{{trigger['from']['value'][0]['address']}}","subject":"{{trigger['subject']}}"},"pipeline_name":"mail-triage"},"propertySettings":{},"errorHandlingOptions":{}}}
  }
}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, got, err := BuildTriggerFlow("mail-triage", tc.trigger, "gitmoot-bridge", "0.4.3", tc.version)
			if err != nil {
				t.Fatal(err)
			}
			var want bytes.Buffer
			if err := json.Compact(&want, []byte(tc.want)); err != nil {
				t.Fatalf("compact golden: %v", err)
			}
			if string(got) != want.String() {
				t.Fatalf("generated flow mismatch\n got: %s\nwant: %s", got, want.String())
			}
		})
	}
}

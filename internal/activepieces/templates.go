package activepieces

import (
	"embed"
	"encoding/json"
	"fmt"
)

//go:embed templates/*.json
var templateFS embed.FS

type Template struct {
	ID          string
	DisplayName string
	Description string
	Filename    string
	Flow        json.RawMessage
}

var templateRegistry = []Template{
	{
		ID:          "webhook-run-pipeline",
		DisplayName: "Gitmoot: Webhook to Pipeline",
		Description: "Receive a webhook and enqueue a named Gitmoot pipeline.",
		Filename:    "webhook-run-pipeline.json",
	},
	{
		ID:          "gmail-imap-ask-agent",
		DisplayName: "Gitmoot: Email to Agent Acknowledgement",
		Description: "Receive mail over IMAP, enqueue an agent job, and send an SMTP acknowledgement.",
		Filename:    "gmail-imap-ask-agent.json",
	},
}

// The email flow deliberately acknowledges the queued job_id. The bridge's
// ask_agent action is asynchronous, so a full agent-answer reply needs polling
// or a callback in a future version.

func Templates() ([]Template, error) {
	templates := make([]Template, 0, len(templateRegistry))
	for _, entry := range templateRegistry {
		flow, err := templateFS.ReadFile("templates/" + entry.Filename)
		if err != nil {
			return nil, fmt.Errorf("read embedded template %s: %w", entry.ID, err)
		}
		entry.Flow = append(json.RawMessage(nil), flow...)
		templates = append(templates, entry)
	}
	return templates, nil
}

func SelectTemplates(ids []string) ([]Template, error) {
	all, err := Templates()
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return all, nil
	}
	byID := make(map[string]Template, len(all))
	for _, entry := range all {
		byID[entry.ID] = entry
	}
	selected := make([]Template, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		entry, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("unknown Activepieces template %q", id)
		}
		if !seen[id] {
			selected = append(selected, entry)
			seen[id] = true
		}
	}
	return selected, nil
}

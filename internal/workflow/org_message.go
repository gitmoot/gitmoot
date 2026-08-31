package workflow

import "strings"

// FormatOrgMessageNote records a non-obligatory message between organization
// roles. The marker keeps the sender and recipient citable with the note body.
func FormatOrgMessageNote(from, to, workflowID, message string) string {
	if strings.TrimSpace(message) == "" {
		return ""
	}
	return formatAddressedOrgNote("message", []addressedOrgNoteField{
		{key: "to", value: to}, {key: "from", value: from}, {key: "wf", value: workflowID},
	}, message)
}

// ParseOrgMessageNote decodes a durable non-obligatory organization message.
func ParseOrgMessageNote(body string) (from, to, workflowID, message string, ok bool) {
	values, message, ok := parseAddressedOrgNote("message", body)
	if !ok || message == "" || len(values) != 3 || values["from"] == "" || values["to"] == "" || values["wf"] == "" {
		return "", "", "", "", false
	}
	return values["from"], values["to"], values["wf"], message, true
}

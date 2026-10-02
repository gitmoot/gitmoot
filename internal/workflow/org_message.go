package workflow

// ParseOrgMessageNote decodes a durable non-obligatory organization message.
func ParseOrgMessageNote(body string) (from, to, workflowID, message string, ok bool) {
	values, message, ok := parseAddressedOrgNote("message", body)
	if !ok || message == "" || len(values) != 3 || values["from"] == "" || values["to"] == "" || values["wf"] == "" {
		return "", "", "", "", false
	}
	return values["from"], values["to"], values["wf"], message, true
}

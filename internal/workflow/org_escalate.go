package workflow

import "strings"

const OrgEscalatePrefix = "[org:escalate "

// FormatOrgEscalateNote encodes an org escalation in its durable workflow-note
// schema. Invalid delimiter-bearing fields return an empty string; normal CLI
// callers validate role and workflow values before reaching this helper.
func FormatOrgEscalateNote(from, to, wf, question string) string {
	if !validOrgEscalateField(from) || !validOrgEscalateField(to) || !validOrgEscalateField(wf) || strings.TrimSpace(question) == "" {
		return ""
	}
	return OrgEscalatePrefix + "to=" + to + " from=" + from + " wf=" + wf + "] " + question
}

// ParseOrgEscalateNote decodes the typed escalation prefix. The first closing
// bracket ends the key block, so brackets in the question are preserved.
func ParseOrgEscalateNote(body string) (from, to, wf, question string, ok bool) {
	if !strings.HasPrefix(body, OrgEscalatePrefix) {
		return "", "", "", "", false
	}
	end := strings.IndexByte(body, ']')
	if end < 0 || end == len(OrgEscalatePrefix)-1 || end+1 >= len(body) || body[end+1] != ' ' {
		return "", "", "", "", false
	}
	fields := strings.Fields(body[len(OrgEscalatePrefix):end])
	if len(fields) != 3 {
		return "", "", "", "", false
	}
	values := make(map[string]string, 3)
	for _, field := range fields {
		key, value, hasValue := strings.Cut(field, "=")
		if !hasValue || (key != "to" && key != "from" && key != "wf") || !validOrgEscalateField(value) {
			return "", "", "", "", false
		}
		if _, duplicate := values[key]; duplicate {
			return "", "", "", "", false
		}
		values[key] = value
	}
	from, to, wf = values["from"], values["to"], values["wf"]
	question = body[end+2:]
	if from == "" || to == "" || wf == "" || strings.TrimSpace(question) == "" {
		return "", "", "", "", false
	}
	return from, to, wf, question, true
}

func validOrgEscalateField(value string) bool {
	return value != "" && !strings.ContainsAny(value, "[]= \t\r\n")
}

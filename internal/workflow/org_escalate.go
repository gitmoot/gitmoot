package workflow

import (
	"strconv"
	"strings"
)

const OrgEscalatePrefix = "[org:escalate "
const OrgEscalateResolvedPrefix = "[org:escalate-resolved "

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

// FormatOrgEscalateResolvedNote encodes the append-only marker that closes an
// escalation note. answerNoteID optionally links the note containing an answer.
func FormatOrgEscalateResolvedNote(escalationNoteID int64, resolvedBy string, answerNoteID int64) string {
	if escalationNoteID <= 0 || !validOrgEscalateField(resolvedBy) {
		return ""
	}
	body := OrgEscalateResolvedPrefix + "id=" + strconv.FormatInt(escalationNoteID, 10) + " by=" + resolvedBy
	if answerNoteID > 0 {
		body += " note=" + strconv.FormatInt(answerNoteID, 10)
	}
	return body + "] resolved"
}

// ParseOrgEscalateResolvedNote decodes an escalation resolution marker.
func ParseOrgEscalateResolvedNote(body string) (escalationNoteID int64, resolvedBy string, answerNoteID int64, ok bool) {
	if !strings.HasPrefix(body, OrgEscalateResolvedPrefix) {
		return 0, "", 0, false
	}
	end := strings.IndexByte(body, ']')
	if end < 0 || body[end:] != "] resolved" {
		return 0, "", 0, false
	}
	fields := strings.Fields(body[len(OrgEscalateResolvedPrefix):end])
	if len(fields) < 2 || len(fields) > 3 {
		return 0, "", 0, false
	}
	values := make(map[string]string, 3)
	for _, field := range fields {
		key, value, hasValue := strings.Cut(field, "=")
		if !hasValue || (key != "id" && key != "by" && key != "note") || value == "" {
			return 0, "", 0, false
		}
		if _, duplicate := values[key]; duplicate {
			return 0, "", 0, false
		}
		values[key] = value
	}
	if !validOrgEscalateField(values["by"]) || values["id"] == "" {
		return 0, "", 0, false
	}
	escalationNoteID, err := strconv.ParseInt(values["id"], 10, 64)
	if err != nil || escalationNoteID <= 0 {
		return 0, "", 0, false
	}
	if noteValue, present := values["note"]; present {
		answerNoteID, err = strconv.ParseInt(noteValue, 10, 64)
		if err != nil || answerNoteID <= 0 {
			return 0, "", 0, false
		}
	}
	return escalationNoteID, values["by"], answerNoteID, true
}

func validOrgEscalateField(value string) bool {
	return value != "" && !strings.ContainsAny(value, "[]= \t\r\n")
}

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
	return formatAddressedOrgNote("escalate", []addressedOrgNoteField{
		{key: "to", value: to}, {key: "from", value: from}, {key: "wf", value: wf},
	}, question)
}

// ParseOrgEscalateNote decodes the typed escalation prefix. The first closing
// bracket ends the key block, so brackets in the question are preserved.
func ParseOrgEscalateNote(body string) (from, to, wf, question string, ok bool) {
	values, question, ok := parseAddressedOrgNote("escalate", body)
	if !ok || len(values) != 3 {
		return "", "", "", "", false
	}
	from, to, wf = values["from"], values["to"], values["wf"]
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
	fields := []addressedOrgNoteField{{key: "id", value: strconv.FormatInt(escalationNoteID, 10)}, {key: "by", value: resolvedBy}}
	if answerNoteID > 0 {
		fields = append(fields, addressedOrgNoteField{key: "note", value: strconv.FormatInt(answerNoteID, 10)})
	}
	return formatAddressedOrgNote("escalate-resolved", fields, "resolved")
}

// ParseOrgEscalateResolvedNote decodes an escalation resolution marker.
func ParseOrgEscalateResolvedNote(body string) (escalationNoteID int64, resolvedBy string, answerNoteID int64, ok bool) {
	values, content, ok := parseAddressedOrgNote("escalate-resolved", body)
	if !ok || content != "resolved" || len(values) < 2 || len(values) > 3 ||
		(len(values) == 3 && values["note"] == "") {
		return 0, "", 0, false
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

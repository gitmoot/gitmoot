package workflow

import "strconv"

const (
	OrgDirectivePrefix          = "[org:directive "
	OrgDirectiveAckPrefix       = "[org:directive-ack "
	OrgDirectiveCancelPrefix    = "[org:directive-cancel "
	OrgDirectiveDonePrefix      = "[org:directive-done "
	OrgDirectiveExhaustedPrefix = "[org:directive-exhausted "
)

func FormatOrgDirectiveNote(from, to, wf, directive string) string {
	return formatAddressedOrgNote("directive", []addressedOrgNoteField{
		{key: "to", value: to}, {key: "from", value: from}, {key: "wf", value: wf},
	}, directive)
}

func ParseOrgDirectiveNote(body string) (from, to, wf, directive string, ok bool) {
	values, directive, ok := parseAddressedOrgNote("directive", body)
	if !ok || directive == "" || len(values) != 3 || values["from"] == "" || values["to"] == "" || values["wf"] == "" {
		return "", "", "", "", false
	}
	return values["from"], values["to"], values["wf"], directive, true
}

func FormatOrgDirectiveAckNote(directiveID int64, by string) string {
	return formatOrgDirectiveReceipt("directive-ack", directiveID, by)
}

func ParseOrgDirectiveAckNote(body string) (directiveID int64, by string, ok bool) {
	return parseOrgDirectiveReceipt("directive-ack", body)
}

func FormatOrgDirectiveCancelNote(directiveID int64, by string) string {
	return formatOrgDirectiveReceipt("directive-cancel", directiveID, by)
}

func ParseOrgDirectiveCancelNote(body string) (directiveID int64, by string, ok bool) {
	return parseOrgDirectiveReceipt("directive-cancel", body)
}

// FormatOrgDirectiveExhaustedNote records that a nudge ladder ran to its cap
// without the obligation being met (#1352). It is a MARKER NOTE rather than only
// a column because ack, cancel, done and escalate are all notes and Comms builds
// its threads from notes — a column-only terminal state was the anomaly, and
// invisible to the operator who needs it most.
func FormatOrgDirectiveExhaustedNote(directiveID int64, by string) string {
	return formatOrgDirectiveReceipt("directive-exhausted", directiveID, by)
}

func ParseOrgDirectiveExhaustedNote(body string) (directiveID int64, by string, ok bool) {
	return parseOrgDirectiveReceipt("directive-exhausted", body)
}

func FormatOrgDirectiveDoneNote(directiveID int64, by string) string {
	return formatOrgDirectiveReceipt("directive-done", directiveID, by)
}

func ParseOrgDirectiveDoneNote(body string) (directiveID int64, by string, ok bool) {
	return parseOrgDirectiveReceipt("directive-done", body)
}

func formatOrgDirectiveReceipt(kind string, directiveID int64, by string) string {
	if directiveID <= 0 {
		return ""
	}
	return formatAddressedOrgNote(kind, []addressedOrgNoteField{
		{key: "id", value: strconv.FormatInt(directiveID, 10)}, {key: "by", value: by},
	}, "")
}

func parseOrgDirectiveReceipt(kind, body string) (directiveID int64, by string, ok bool) {
	values, content, ok := parseAddressedOrgNote(kind, body)
	if !ok || content != "" || len(values) != 2 || values["by"] == "" {
		return 0, "", false
	}
	directiveID, err := strconv.ParseInt(values["id"], 10, 64)
	if err != nil || directiveID <= 0 {
		return 0, "", false
	}
	return directiveID, values["by"], true
}

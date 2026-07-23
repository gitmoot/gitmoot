package workflow

import "strings"

const OrgHandoffPrefix = "[org:handoff "

// FormatOrgHandoffNote encodes a role-session handoff in the durable workflow
// journal. Invalid delimiter-bearing roles or empty notes return an empty body.
func FormatOrgHandoffNote(role, note string) string {
	if !validOrgEscalateField(role) || strings.TrimSpace(note) == "" {
		return ""
	}
	return OrgHandoffPrefix + "role=" + role + "] " + note
}

// ParseOrgHandoffNote decodes the typed handoff prefix. The first closing
// bracket ends the header, so brackets in the handoff text are preserved.
func ParseOrgHandoffNote(body string) (role, handoff string, ok bool) {
	if !strings.HasPrefix(body, OrgHandoffPrefix) {
		return "", "", false
	}
	end := strings.IndexByte(body, ']')
	if end < 0 || end == len(OrgHandoffPrefix)-1 || end+1 >= len(body) || body[end+1] != ' ' {
		return "", "", false
	}
	header := body[len(OrgHandoffPrefix):end]
	key, value, hasValue := strings.Cut(header, "=")
	handoff = body[end+2:]
	if !hasValue || key != "role" || !validOrgEscalateField(value) || strings.TrimSpace(handoff) == "" {
		return "", "", false
	}
	return value, handoff, true
}

package workflow

import "strings"

const orgHandoffPrefix = "[org:handoff "

// FormatOrgHandoffNote encodes a role-session handoff in the durable workflow
// journal. Invalid delimiter-bearing roles or empty notes return an empty body.
func FormatOrgHandoffNote(role, note string) string {
	if !validOrgEscalateField(role) || strings.TrimSpace(note) == "" {
		return ""
	}
	return orgHandoffPrefix + "role=" + role + "] " + note
}

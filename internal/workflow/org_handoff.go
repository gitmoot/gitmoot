package workflow

const OrgHandoffPrefix = "[org:handoff "

// FormatOrgHandoffNote encodes a role-session handoff in the durable workflow
// journal. Invalid delimiter-bearing roles or empty notes return an empty body.
func FormatOrgHandoffNote(role, note string) string {
	return formatAddressedOrgNote("handoff", []addressedOrgNoteField{{key: "role", value: role}}, note)
}

// ParseOrgHandoffNote decodes the typed handoff prefix. The first closing
// bracket ends the header, so brackets in the handoff text are preserved.
func ParseOrgHandoffNote(body string) (role, handoff string, ok bool) {
	values, handoff, ok := parseAddressedOrgNote("handoff", body)
	if !ok || handoff == "" || len(values) != 1 || values["role"] == "" {
		return "", "", false
	}
	return values["role"], handoff, true
}

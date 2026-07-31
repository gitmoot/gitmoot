package workflow

import "strings"

type addressedOrgNoteField struct {
	key   string
	value string
}

func formatAddressedOrgNote(kind string, fields []addressedOrgNoteField, body string) string {
	if !validOrgEscalateField(kind) || len(fields) == 0 {
		return ""
	}
	parts := make([]string, 0, len(fields))
	seen := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		if !validOrgEscalateField(field.key) || !validOrgEscalateField(field.value) {
			return ""
		}
		if _, duplicate := seen[field.key]; duplicate {
			return ""
		}
		seen[field.key] = struct{}{}
		parts = append(parts, field.key+"="+field.value)
	}
	note := "[org:" + kind + " " + strings.Join(parts, " ") + "]"
	if body == "" {
		return note
	}
	if strings.TrimSpace(body) == "" {
		return ""
	}
	return note + " " + body
}

func parseAddressedOrgNote(kind, body string) (map[string]string, string, bool) {
	if !validOrgEscalateField(kind) {
		return nil, "", false
	}
	prefix := "[org:" + kind + " "
	if !strings.HasPrefix(body, prefix) {
		return nil, "", false
	}
	end := strings.IndexByte(body, ']')
	if end < 0 || end == len(prefix)-1 {
		return nil, "", false
	}
	content := ""
	if end+1 < len(body) {
		if body[end+1] != ' ' {
			return nil, "", false
		}
		content = body[end+2:]
		if strings.TrimSpace(content) == "" {
			return nil, "", false
		}
	}
	fields := strings.Fields(body[len(prefix):end])
	values := make(map[string]string, len(fields))
	for _, field := range fields {
		key, value, ok := strings.Cut(field, "=")
		if !ok || !validOrgEscalateField(key) || !validOrgEscalateField(value) {
			return nil, "", false
		}
		if _, duplicate := values[key]; duplicate {
			return nil, "", false
		}
		values[key] = value
	}
	return values, content, true
}

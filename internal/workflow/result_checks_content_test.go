package workflow

import "testing"

// TestEntryCarriesContentRejectsContainersWithOnlyEmptyValues is the round-N F3:
// the false-accept was only HALF closed. Counting keys made
// {"name":"","command":""} read as populated evidence, which is worse than a
// hard failure because a coordinator sees a populated tests_run and believes
// the job looked.
func TestEntryCarriesContentRejectsContainersWithOnlyEmptyValues(t *testing.T) {
	empty := map[string]string{
		"empty object":              `{}`,
		"object of empty strings":   `{"name":"","command":""}`,
		"nested empty object":       `{"a":{"b":""}}`,
		"empty array":               `[]`,
		"array of empty strings":    `["",""]`,
		"array of empty containers": `[{},[]]`,
		"whitespace":                "   ",
		"empty string":              "",
	}
	for name, value := range empty {
		t.Run("no content/"+name, func(t *testing.T) {
			if entryCarriesContent(value) {
				t.Fatalf("%s reads as evidence but carries no information", name)
			}
		})
	}

	populated := map[string]string{
		"object with a value":       `{"name":"diff scope"}`,
		"object with one populated": `{"name":"","command":"go test ./..."}`,
		"nested populated":          `{"a":{"b":"real"}}`,
		"array with a value":        `["go test ./..."]`,
		"plain text":                "go test ./...",
		"unparseable container":     `{not json`,
	}
	for name, value := range populated {
		t.Run("content/"+name, func(t *testing.T) {
			if !entryCarriesContent(value) {
				t.Fatalf("%s carries information but was rejected", name)
			}
		})
	}
}

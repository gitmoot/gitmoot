package cli

import (
	"context"
	"encoding/json"
	"testing"
)

// #2589 dropped the agent template tables the Learning page's Skills read, so
// the DataSource method reports no templates as a non-nil slice (marshals to
// [] not null) without touching the database.
func TestWebDataSourceSkills(t *testing.T) {
	ds := &webDataSource{home: t.TempDir()}
	skills, err := ds.Skills(context.Background())
	if err != nil {
		t.Fatalf("Skills: %v", err)
	}
	encoded, err := json.Marshal(skills.Templates)
	if err != nil {
		t.Fatalf("marshal templates: %v", err)
	}
	if string(encoded) != "[]" {
		t.Fatalf("Skills().Templates = %s, want []", encoded)
	}
}

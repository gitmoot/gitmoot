package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
)

// TestDoctorJSONOutput pins the fix for "gitmoot doctor --json advertised but
// rejected": doctor now accepts --json and emits the checks as a JSON array (each
// with name/status/ok/required/detail) instead of erroring with
// "flag provided but not defined: -json".
func TestDoctorJSONOutput(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	// --repo at a temp dir keeps the run local; individual checks may warn/fail (no
	// git remote, etc.) but the JSON shape is what this test asserts.
	Run([]string{"doctor", "--json", "--repo", t.TempDir()}, &stdout, &stderr)

	if bytes.Contains(stderr.Bytes(), []byte("flag provided but not defined")) {
		t.Fatalf("doctor still rejects --json: %s", stderr.String())
	}
	var checks []map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &checks); err != nil {
		t.Fatalf("doctor --json did not produce a JSON array: %v\nstdout=%q", err, stdout.String())
	}
	if len(checks) == 0 {
		t.Fatalf("doctor --json produced an empty array")
	}
	for _, c := range checks {
		for _, k := range []string{"name", "status", "ok", "required", "detail"} {
			if _, ok := c[k]; !ok {
				t.Errorf("doctor --json check missing key %q: %v", k, c)
			}
		}
	}
}

func TestDoctorWarnsWhenDirectedEventKindHasNoObserverRule(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AddEventRule(context.Background(), db.EventRule{
		ID: "addressed-reply", OnKind: "reply", WakeRole: "owner",
		Scope: db.EventRuleScopeAddressed, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.AddEventRule(context.Background(), db.EventRule{
		ID: "disabled-observer-reply", OnKind: "reply", WakeRole: "auditor",
		Scope: db.EventRuleScopeObserver, Enabled: false,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	checks := runDoctorJSONChecks(t)
	check, ok := doctorJSONCheckByName(checks, "event observers")
	if !ok {
		t.Fatalf("doctor checks = %#v, want event observers warning for empty observer set", checks)
	}
	if check["status"] != "warn" || !bytes.Contains([]byte(check["detail"].(string)), []byte("reply")) {
		t.Fatalf("event observers check = %#v, want reply warning", check)
	}

	store, err = db.Open(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AddEventRule(context.Background(), db.EventRule{
		ID: "observer-reply", OnKind: "reply", WakeRole: "owner",
		Scope: db.EventRuleScopeObserver, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	checks = runDoctorJSONChecks(t)
	if check, ok := doctorJSONCheckByName(checks, "event observers"); ok {
		t.Fatalf("event observers check = %#v, want silence when every directed kind has an enabled observer", check)
	}
}

func runDoctorJSONChecks(t *testing.T) []map[string]any {
	t.Helper()
	var stdout, stderr bytes.Buffer
	Run([]string{"doctor", "--json", "--repo", t.TempDir()}, &stdout, &stderr)
	var checks []map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &checks); err != nil {
		t.Fatalf("doctor --json: %v\nstdout=%q\nstderr=%q", err, stdout.String(), stderr.String())
	}
	return checks
}

func doctorJSONCheckByName(checks []map[string]any, name string) (map[string]any, bool) {
	for _, check := range checks {
		if check["name"] == name {
			return check, true
		}
	}
	return nil, false
}

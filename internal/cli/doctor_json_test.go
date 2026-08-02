package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
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
		ID: "observer-reply", OnKind: "RePlY", WakeRole: "owner",
		Scope: db.EventRuleScopeObserver, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.AddEventRule(context.Background(), db.EventRule{
		ID: "addressed-blocked", OnKind: "blocked", WakeRole: "owner",
		Scope: db.EventRuleScopeAddressed, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.AddEventRule(context.Background(), db.EventRule{
		ID: "addressed-escalation", OnKind: "escalation", WakeRole: "owner",
		Scope: db.EventRuleScopeAddressed, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	checks := runDoctorJSONChecks(t)
	check, ok := doctorJSONCheckByName(checks, "event observers")
	if !ok {
		t.Fatalf("doctor checks = %#v, want event observers warning", checks)
	}
	detail := check["detail"].(string)
	missingKinds := make([]string, 0)
	for _, kind := range wakeTargetRoleKinds() {
		if kind != db.WakeOutboxKindReply {
			missingKinds = append(missingKinds, kind)
		}
	}
	if check["status"] != "warn" ||
		!strings.Contains(detail, strings.Join(missingKinds, ", ")) ||
		strings.Contains(detail, "reply") {
		t.Fatalf("event observers check = %#v, want derived missing kinds %v with reply covered", check, missingKinds)
	}
	t.Logf("reviewer probe: warned=true detail=%q", detail)

	store, err = db.Open(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range wakeTargetRoleKinds() {
		if kind == db.WakeOutboxKindReply {
			continue
		}
		if err := store.AddEventRule(context.Background(), db.EventRule{
			ID: "observer-" + kind, OnKind: kind, WakeRole: "owner",
			Scope: db.EventRuleScopeObserver, Enabled: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	checks = runDoctorJSONChecks(t)
	if check, ok := doctorJSONCheckByName(checks, "event observers"); ok {
		t.Fatalf("event observers check = %#v, want silence when every directed kind has an enabled observer", check)
	}
}

func TestBuildEventObserverDoctorCheckWarnsForEachMissingDirectedKind(t *testing.T) {
	kinds := wakeTargetRoleKinds()
	for _, missingKind := range kinds {
		t.Run(missingKind, func(t *testing.T) {
			rules := make([]db.EventRule, 0, len(kinds)-1)
			for _, kind := range kinds {
				if kind == missingKind {
					continue
				}
				rules = append(rules, db.EventRule{
					ID:      "observer-" + kind,
					OnKind:  kind,
					Scope:   db.EventRuleScopeObserver,
					Enabled: true,
				})
			}
			check, warned := buildEventObserverDoctorCheck(rules)
			if !warned || check.OK || !strings.Contains(check.Detail, ": "+missingKind+" —") {
				t.Fatalf("drop %s: warned=%v check=%#v, want warning naming only %s", missingKind, warned, check, missingKind)
			}
			t.Logf("drop %s: warned=%v detail=%q", missingKind, warned, check.Detail)
		})
	}
}

func TestEventObserverDoctorCheckWarnsWhenCoverageIsUnreadable(t *testing.T) {
	t.Run("empty database path", func(t *testing.T) {
		check, ok := eventObserverDoctorCheck(config.Paths{})
		if !ok || check.OK || !strings.Contains(check.Detail, "database path is empty") {
			t.Fatalf("check = %#v, ok = %v, want unverified warning", check, ok)
		}
	})

	t.Run("open failure", func(t *testing.T) {
		check, ok := eventObserverDoctorCheck(config.Paths{
			Database: filepath.Join(t.TempDir(), "missing", "gitmoot.db"),
		})
		if !ok || check.OK || !strings.Contains(check.Detail, "open event rule store") {
			t.Fatalf("check = %#v, ok = %v, want unverified warning", check, ok)
		}
	})

	t.Run("list failure", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "gitmoot.db")
		raw, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := raw.Exec(`CREATE TABLE unrelated (id TEXT PRIMARY KEY)`); err != nil {
			t.Fatal(err)
		}
		if err := raw.Close(); err != nil {
			t.Fatal(err)
		}
		check, ok := eventObserverDoctorCheck(config.Paths{Database: path})
		if !ok || check.OK || !strings.Contains(check.Detail, "list event rules") {
			t.Fatalf("check = %#v, ok = %v, want unverified warning", check, ok)
		}
	})
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

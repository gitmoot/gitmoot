package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/org"
)

// TestResolveDispatchOrgRoleUsesCallerPane pins #2363: a seat that dispatches
// from its own registered pane is attributed without --org-role, so its
// verdicts and blocked notices return to it. An unregistered pane resolves to
// no role (and the org gate then refuses the dispatch); a pane bound to two
// roles is an error, never a choice.
func TestResolveDispatchOrgRoleUsesCallerPane(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatal(err)
	}
	const orgConfig = `
[org.roles."owner"]
scope=["*"]
pane="w1:p0"
[org.roles."app"]
parent="owner"
scope=["acme/docus"]
pane="w1:p3"
[org.roles."twin"]
parent="owner"
scope=["acme/twin"]
pane="w1:p9"
`
	if err := os.WriteFile(paths.ConfigFile, []byte(orgConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	withOrgProvider(t, orgFixtureProvider{snapshot: org.Snapshot{
		ObservedAt: time.Now(),
		PaneBindings: map[string]org.PaneBinding{
			"owner": {PaneID: "w1:p0"},
			"app":   {PaneID: "w1:p3"},
			"twin":  {PaneID: "w1:p9"},
			"twin2": {PaneID: "w1:p9"},
		},
	}})
	t.Setenv("GITMOOT_ORG_ROLE", "")
	ctx := context.Background()

	t.Setenv("HERDR_PANE_ID", "w1:p3")
	if got, err := resolveDispatchOrgRole(ctx, home, ""); err != nil || got != "app" {
		t.Fatalf("registered pane resolved %q, %v; want app", got, err)
	}
	if got, err := resolveDispatchOrgRole(ctx, home, "Owner"); err != nil || got != "owner" {
		t.Fatalf("explicit role resolved %q, %v; want owner over the pane", got, err)
	}

	t.Setenv("HERDR_PANE_ID", "w1:p7")
	if got, err := resolveDispatchOrgRole(ctx, home, ""); err != nil || got != "" {
		t.Fatalf("unregistered pane resolved %q, %v; want empty so the gate refuses", got, err)
	}

	t.Setenv("HERDR_PANE_ID", "w1:p9")
	if _, err := resolveDispatchOrgRole(ctx, home, ""); !errors.Is(err, errPaneMapsToMultipleRoles) {
		t.Fatalf("ambiguous pane err = %v, want errPaneMapsToMultipleRoles", err)
	}
}

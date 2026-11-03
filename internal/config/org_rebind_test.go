package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #2503 review: the daemon's automatic rename-follow must never undo an
// operator's concurrent unbind, rebind or removal of the seat. The rebind only
// replaces the exact expected binding of an existing role; anything else
// writes nothing.
func TestRebindOrgSeatPaneOnlyReplacesTheExpectedBinding(t *testing.T) {
	const base = "[org.roles.\"owner\"]\nscope = [\"*\"]\n\n[org.roles.\"worker\"]\nparent = \"owner\"\nscope = [\"*\"]\n"
	cases := []struct {
		name    string
		worker  string
		wantErr bool
		want    string
	}{
		{"expected binding is replaced", "pane = \"agent:worker\"\n", false, `pane = "agent:tino"`},
		{"operator unbound the seat", "", true, ""},
		{"operator rebound the seat", "pane = \"agent:other\"\n", true, `pane = "agent:other"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(base+tc.worker), 0o600); err != nil {
				t.Fatal(err)
			}
			err := RebindOrgSeatPane(Paths{ConfigFile: path}, "worker", "agent:worker", "agent:tino")
			if tc.wantErr != (err != nil) || (err != nil && !errors.Is(err, ErrOrgSeatBindingChanged)) {
				t.Fatalf("rebind err = %v, want binding-changed refusal %t", err, tc.wantErr)
			}
			after, _ := os.ReadFile(path)
			if tc.wantErr && string(after) != base+tc.worker {
				t.Fatalf("refused rebind still wrote the config:\n%s", after)
			}
			if tc.want != "" && !strings.Contains(string(after), tc.want) {
				t.Fatalf("config = %s, want %s", after, tc.want)
			}
		})
	}
	t.Run("operator removed the seat", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.toml")
		removed := "[org.roles.\"owner\"]\nscope = [\"*\"]\n"
		if err := os.WriteFile(path, []byte(removed), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := RebindOrgSeatPane(Paths{ConfigFile: path}, "worker", "agent:worker", "agent:tino"); !errors.Is(err, ErrOrgSeatBindingChanged) {
			t.Fatalf("rebind of a removed role err = %v, want refusal", err)
		}
		if after, _ := os.ReadFile(path); string(after) != removed {
			t.Fatalf("rebind recreated a removed role:\n%s", after)
		}
	})
	// A hand edit (no lock) landing after the guard read and before the file
	// is installed: the operator unbinds the seat. The rebind must leave the
	// operator's file in place.
	t.Run("hand edit during the rebind wins", func(t *testing.T) {
		if !canExchangeFiles {
			t.Skip("atomic exchange is Linux-only")
		}
		path := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(path, []byte(base+"pane = \"agent:worker\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		previous := beforeConfigExchange
		t.Cleanup(func() { beforeConfigExchange = previous })
		beforeConfigExchange = func() {
			if err := os.WriteFile(path, []byte(base), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if err := RebindOrgSeatPane(Paths{ConfigFile: path}, "worker", "agent:worker", "agent:tino"); !errors.Is(err, ErrOrgSeatBindingChanged) {
			t.Fatalf("rebind racing a hand edit err = %v, want refusal", err)
		}
		if after, _ := os.ReadFile(path); string(after) != base {
			t.Fatalf("rebind overwrote the operator's concurrent unbind:\n%s", after)
		}
		if leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".config-*.toml")); len(leftovers) != 0 {
			t.Fatalf("staged files left behind: %v", leftovers)
		}
	})
}

package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// #2510 (deimos-forge post-merge review of #2503): `agent type set` rewrote
// the whole config from a copy read before the daemon's automatic seat rebind,
// so the save put the old binding back. Every gitmoot config writer must hold
// the config edit lock: here an agent-type save is held between its read and
// its write while the rebind runs, and the result must keep both edits.
func TestAgentTypeSaveCannotRevertAConcurrentSeatRebind(t *testing.T) {
	home := t.TempDir()
	paths := PathsForHome(home)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte("[org.roles.\"owner\"]\nscope = [\"*\"]\n\n[org.roles.\"deimos\"]\nparent = \"owner\"\nscope = [\"*\"]\npane = \"agent:deimos\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	read, release := make(chan struct{}), make(chan struct{})
	saved := make(chan error, 1)
	go func() {
		saved <- saveAgentType(paths, AgentType{Name: "reviewer", Runtime: "omp"}, func(path string, contents []byte) error {
			close(read) // the save has read the config and is about to write
			<-release
			return writeConfigAtomic(path, contents)
		})
	}()
	<-read
	rebound := make(chan error, 1)
	go func() { rebound <- RebindOrgSeatPane(paths, "deimos", "agent:deimos", "agent:renamed") }()
	select {
	case err := <-rebound:
		close(release)
		<-saved
		t.Fatalf("rebind ran while an agent-type save held a stale copy (err=%v); the save then overwrites it", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	if err := <-saved; err != nil {
		t.Fatal(err)
	}
	if err := <-rebound; err != nil {
		t.Fatalf("rebind after the save: %v", err)
	}
	after, _ := os.ReadFile(paths.ConfigFile)
	if !strings.Contains(string(after), `pane = "agent:renamed"`) || !strings.Contains(string(after), "reviewer") {
		t.Fatalf("config lost an edit:\n%s", after)
	}
}

// #2511 review (P1): `org seat add` keeps the config edit and calls Restore if
// its later route write fails. If the daemon rebinds the seat in between,
// Restore must not write the stale pre-edit bytes over that rebind; with no
// edit in between it still rolls back.
func TestOrgSeatRestoreNeverRevertsALaterEdit(t *testing.T) {
	base := "[org.roles.\"owner\"]\nscope = [\"*\"]\n"
	for _, tc := range []struct {
		name       string
		between    func(t *testing.T, paths Paths)
		wantErr    error
		wantSubstr string
	}{
		{"no later edit: rolled back", func(*testing.T, Paths) {}, nil, ""},
		{"later rebind: kept", func(t *testing.T, paths Paths) {
			if err := RebindOrgSeatPane(paths, "deimos", "agent:deimos", "agent:renamed"); err != nil {
				t.Fatal(err)
			}
		}, ErrOrgSeatConfigChangedSinceEdit, `pane = "agent:renamed"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			paths := PathsForHome(t.TempDir())
			if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(paths.ConfigFile, []byte(base), 0o600); err != nil {
				t.Fatal(err)
			}
			edit, created, err := UpsertOrgSeatRole(paths, OrgRole{Name: "deimos", Parent: "owner", Scope: []string{"*"}, MergeRule: "self", Pane: "agent:deimos"}, "")
			if err != nil || !created {
				t.Fatalf("upsert created=%t err=%v", created, err)
			}
			tc.between(t, paths)
			if err := edit.Restore(); !errors.Is(err, tc.wantErr) && !(tc.wantErr == nil && err == nil) {
				t.Fatalf("restore err = %v, want %v", err, tc.wantErr)
			}
			after, _ := os.ReadFile(paths.ConfigFile)
			if tc.wantSubstr == "" && string(after) != base {
				t.Fatalf("restore with no later edit did not roll back:\n%s", after)
			}
			if tc.wantSubstr != "" && !strings.Contains(string(after), tc.wantSubstr) {
				t.Fatalf("restore reverted the later edit:\n%s", after)
			}
		})
	}
}

// #2511 review (P2): Initialize on a home whose config another command already
// created (and edited) must not truncate it back to the default.
func TestInitializeNeverTruncatesAnExistingConfig(t *testing.T) {
	home := t.TempDir()
	paths := PathsForHome(home)
	if err := Initialize(paths); err != nil {
		t.Fatal(err)
	}
	edited := append(mustRead(t, paths.ConfigFile), []byte("\n[org.roles.\"owner\"]\nscope = [\"*\"]\n")...)
	if err := os.WriteFile(paths.ConfigFile, edited, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Initialize(paths); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, paths.ConfigFile); string(got) != string(edited) {
		t.Fatalf("Initialize rewrote an existing config")
	}
	// The racing case: this Initialize has seen no file, and another command
	// creates and edits it before this one writes. Before #2510 the late
	// write truncated it back to the default.
	if err := os.Remove(paths.ConfigFile); err != nil {
		t.Fatal(err)
	}
	previous := afterConfigAbsent
	t.Cleanup(func() { afterConfigAbsent = previous })
	afterConfigAbsent = func() {
		if err := os.WriteFile(paths.ConfigFile, edited, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := Initialize(paths); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, paths.ConfigFile); string(got) != string(edited) {
		t.Fatalf("Initialize truncated a config created after its absence check:\n%s", got)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

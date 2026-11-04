package config

import (
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

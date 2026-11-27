package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const fixtureRetiredWorktreeMount = `[[worktrees.retired]]
root = "/mnt/box/worktrees"
mount_point = "/mnt/box"
mount_source = "user@host:share"
mount_fstype = "fuse.sshfs"
`

// loadWorktreeRootsWithHome writes the config built from the home it will live in and loads it.
func loadWorktreeRootsWithHome(t *testing.T, body func(home string) string) (WorktreeRoots, error) {
	t.Helper()
	paths := writeGateConfig(t, "")
	if err := os.WriteFile(paths.ConfigFile, []byte(body(paths.Home)), 0o600); err != nil {
		t.Fatal(err)
	}
	return LoadWorktreeRoots(paths)
}

// #2763: every [[worktrees.retired]] entry is parsed, in file order, next to the current root.
func TestLoadWorktreeRootRetiredRoots(t *testing.T) {
	got, err := LoadWorktreeRoots(writeGateConfig(t, "[worktrees]\nroot = \"/srv/worktrees\"\n\n"+
		fixtureRetiredWorktreeMount+"\n[[worktrees.retired]]\nroot = \"/srv/older\" # root only\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	want := WorktreeRoots{Active: WorktreeRoot{Root: "/srv/worktrees"}, Retired: []WorktreeRoot{
		{Root: "/mnt/box/worktrees", MountPoint: "/mnt/box", MountSource: "user@host:share", MountFSType: "fuse.sshfs"},
		{Root: "/srv/older"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}

	// Moving back to <home>/worktrees: retired roots without a [worktrees] section.
	back, err := LoadWorktreeRoots(writeGateConfig(t, fixtureRetiredWorktreeMount))
	if err != nil || back.Active.Configured() || len(back.Retired) != 1 || back.Retired[0].Root != "/mnt/box/worktrees" {
		t.Fatalf("retired-only config = %+v %v", back, err)
	}

	// A malformed header of another section ends the entry instead of being read as a key
	// (pins worktrees.go::arrayTableValues in section_coverage_test.go).
	ended, err := LoadWorktreeRoots(writeGateConfig(t, fixtureRetiredWorktreeMount+"[disk_guard\nmin_free_bytes = 1\n"))
	if err != nil || len(ended.Retired) != 1 || ended.Retired[0].MountFSType != "fuse.sshfs" {
		t.Fatalf("an unrelated malformed header broke [[worktrees.retired]]: %+v %v", ended, err)
	}
}

// #2763: a retired entry is validated exactly like root.
func TestLoadWorktreeRootRetiredRefusesInvalidEntries(t *testing.T) {
	current := "[worktrees]\nroot = \"/srv/worktrees\"\n"
	for name, body := range map[string]string{
		"missing root":         "[[worktrees.retired]]\nmount_point = \"/mnt/box\"\nmount_source = \"s\"\nmount_fstype = \"fuse.sshfs\"\n",
		"empty entry":          "[[worktrees.retired]]\n",
		"relative root":        "[[worktrees.retired]]\nroot = \"worktrees\"\n",
		"unclean root":         "[[worktrees.retired]]\nroot = \"/mnt/box/../worktrees\"\n",
		"filesystem root":      "[[worktrees.retired]]\nroot = \"/\"\n",
		"partial mount":        "[[worktrees.retired]]\nroot = \"/mnt/box/worktrees\"\nmount_point = \"/mnt/box\"\n",
		"root outside mount":   strings.Replace(fixtureRetiredWorktreeMount, "/mnt/box/worktrees", "/srv/old", 1),
		"root is mount point":  strings.Replace(fixtureRetiredWorktreeMount, "/mnt/box/worktrees", "/mnt/box", 1),
		"unknown key":          fixtureRetiredWorktreeMount + "fallback = \"/x\"\n",
		"duplicate key":        fixtureRetiredWorktreeMount + "root = \"/mnt/box/other\"\n",
		"unquoted value":       "[[worktrees.retired]]\nroot = /mnt/box/worktrees\n",
		"not key value":        fixtureRetiredWorktreeMount + "root\n",
		"invalid current root": "[worktrees]\nroot = \"relative\"\n" + fixtureRetiredWorktreeMount,
		"retired twice":        current + fixtureRetiredWorktreeMount + fixtureRetiredWorktreeMount,
	} {
		t.Run(name, func(t *testing.T) {
			if got, err := LoadWorktreeRoots(writeGateConfig(t, body)); err == nil {
				t.Fatalf("accepted %q as %+v", body, got)
			}
		})
	}
}

// #2763 review P2-2: no two recognised roots (the active root, each retired root, and the
// always-recognised <home>/worktrees) may be equal or nested in either direction, and the error
// names both roots.
func TestLoadWorktreeRootsRefusesOverlappingRoots(t *testing.T) {
	retired := func(root string) string { return fmt.Sprintf("[[worktrees.retired]]\nroot = %q\n", root) }
	active := func(root string) string { return fmt.Sprintf("[worktrees]\nroot = %q\n", root) }
	for _, tc := range []struct {
		name  string
		body  func(home string) string
		roots func(home string) [2]string
	}{
		{"retired equals active",
			func(string) string { return active("/srv/worktrees") + retired("/srv/worktrees") },
			func(string) [2]string { return [2]string{"/srv/worktrees", "/srv/worktrees"} }},
		{"retired inside active",
			func(string) string { return active("/srv/worktrees") + retired("/srv/worktrees/old") },
			func(string) [2]string { return [2]string{"/srv/worktrees/old", "/srv/worktrees"} }},
		{"active inside retired",
			func(string) string { return active("/srv/worktrees/new") + retired("/srv/worktrees") },
			func(string) [2]string { return [2]string{"/srv/worktrees", "/srv/worktrees/new"} }},
		{"retired equals home worktrees",
			func(home string) string { return retired(filepath.Join(home, "worktrees")) },
			func(home string) [2]string {
				return [2]string{filepath.Join(home, "worktrees"), filepath.Join(home, "worktrees")}
			}},
		{"retired inside home worktrees",
			func(home string) string { return retired(filepath.Join(home, "worktrees", "old")) },
			func(home string) [2]string {
				return [2]string{filepath.Join(home, "worktrees", "old"), filepath.Join(home, "worktrees")}
			}},
		{"retired contains home worktrees",
			func(home string) string { return retired(home) },
			func(home string) [2]string { return [2]string{home, filepath.Join(home, "worktrees")} }},
		{"active inside home worktrees",
			func(home string) string { return active(filepath.Join(home, "worktrees", "new")) },
			func(home string) [2]string {
				return [2]string{filepath.Join(home, "worktrees", "new"), filepath.Join(home, "worktrees")}
			}},
		{"active contains home worktrees",
			func(home string) string { return active(home) },
			func(home string) [2]string { return [2]string{home, filepath.Join(home, "worktrees")} }},
		{"retired inside retired",
			func(string) string { return retired("/mnt/a") + retired("/mnt/a/b") },
			func(string) [2]string { return [2]string{"/mnt/a/b", "/mnt/a"} }},
		{"retired contains retired",
			func(string) string { return retired("/mnt/a/b") + retired("/mnt/a") },
			func(string) [2]string { return [2]string{"/mnt/a", "/mnt/a/b"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var home string
			got, err := loadWorktreeRootsWithHome(t, func(h string) string { home = h; return tc.body(h) })
			if err == nil {
				t.Fatalf("accepted overlapping roots as %+v", got)
			}
			want := tc.roots(home)
			for _, root := range want {
				if !strings.Contains(err.Error(), fmt.Sprintf("%q", root)) {
					t.Fatalf("error %q does not name root %q", err, root)
				}
			}
			if want[0] != want[1] && strings.Count(err.Error(), "root \"") < 2 {
				t.Fatalf("error %q does not name both roots", err)
			}
		})
	}
	// Prefixes that are not path ancestors do not overlap.
	if got, err := loadWorktreeRootsWithHome(t, func(home string) string {
		return active("/srv/worktrees") + retired("/srv/worktrees-old") + retired(filepath.Join(home, "worktrees-old"))
	}); err != nil || len(got.Retired) != 2 {
		t.Fatalf("sibling prefixes refused: %+v %v", got, err)
	}
}

// #2763 review P2-3: headers are TOML key paths. Every spelling of [[worktrees.retired]] is an
// entry and every spelling of [worktrees] is the active root; the table form of the retired array,
// the array form of [worktrees], and malformed headers that target either are refused, while a
// malformed header of an unrelated section stays tolerated.
// (pins worktrees.go::singleSectionValues and worktrees.go::arrayTableValues).
func TestWorktreeRootHeaderSpellings(t *testing.T) {
	entry := "root = \"/srv/old\"\n"
	for _, header := range []string{
		"[[worktrees.retired]]",
		"[[ worktrees . retired ]]",
		"[[worktrees.\"retired\"]]",
		"[[worktrees.'retired']]",
		"[[\"worktrees\".retired]]",
		"[[worktrees.retired]] # comment",
	} {
		t.Run("retired "+header, func(t *testing.T) {
			got, err := LoadWorktreeRoots(writeGateConfig(t, header+"\n"+entry))
			if err != nil || len(got.Retired) != 1 || got.Retired[0].Root != "/srv/old" {
				t.Fatalf("%s = %+v %v, want one retired root", header, got, err)
			}
		})
	}
	for _, header := range []string{"[worktrees]", "[ worktrees ]", "[\"worktrees\"]", "['worktrees']"} {
		t.Run("active "+header, func(t *testing.T) {
			got, err := LoadWorktreeRoots(writeGateConfig(t, header+"\n"+entry))
			if err != nil || got.Active.Root != "/srv/old" {
				t.Fatalf("%s = %+v %v, want the active root", header, got, err)
			}
		})
	}
	for _, header := range []string{
		"[worktrees.retired]",
		"[ worktrees . retired ]",
		"[worktrees.\"retired\"]",
		"[worktrees.'retired']",
		"[[worktrees.retired]",
		"[[worktrees.retired]]]",
		"[[worktrees.retired]] x",
		"[[worktrees.retired]]x",
		"[[worktrees . \"retired\"",
		"[worktrees",
		"[worktrees]]",
		"[worktrees] x",
		"[[worktrees]]",
		"[[\"worktrees\"]]",
	} {
		t.Run("refused "+header, func(t *testing.T) {
			if got, err := LoadWorktreeRoots(writeGateConfig(t, header+"\n"+entry)); err == nil {
				t.Fatalf("accepted %q as %+v", header, got)
			}
		})
	}
	for _, header := range []string{"[disk_guard", "[disk_guard]] x", "[worktrees_other", "[retired"} {
		t.Run("tolerated "+header, func(t *testing.T) {
			got, err := LoadWorktreeRoots(writeGateConfig(t, "[worktrees]\n"+entry+fixtureRetiredWorktreeMount+header+"\nmin_free_bytes = 1\n"))
			if err != nil || got.Active.Root != "/srv/old" || len(got.Retired) != 1 || got.Retired[0].MountFSType != "fuse.sshfs" {
				t.Fatalf("unrelated malformed header %q broke [worktrees]: %+v %v", header, got, err)
			}
		})
	}
}

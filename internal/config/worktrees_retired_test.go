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

// #2763: every [[worktrees.retired]] entry is parsed, in file order, next to the current root.
func TestLoadWorktreeRootRetiredRoots(t *testing.T) {
	got, err := LoadWorktreeRoot(writeGateConfig(t, "[worktrees]\nroot = \"/srv/worktrees\"\n\n"+
		fixtureRetiredWorktreeMount+"\n[[worktrees.retired]]\nroot = \"/srv/older\" # root only\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	want := WorktreeRoot{Root: "/srv/worktrees", Retired: []WorktreeRoot{
		{Root: "/mnt/box/worktrees", MountPoint: "/mnt/box", MountSource: "user@host:share", MountFSType: "fuse.sshfs"},
		{Root: "/srv/older"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}

	// Moving back to <home>/worktrees: retired roots without a [worktrees] section.
	back, err := LoadWorktreeRoot(writeGateConfig(t, fixtureRetiredWorktreeMount))
	if err != nil || back.Configured() || len(back.Retired) != 1 || back.Retired[0].Root != "/mnt/box/worktrees" {
		t.Fatalf("retired-only config = %+v %v", back, err)
	}

	// A malformed header of another section ends the entry instead of being read as a key
	// (pins worktrees.go::loadArrayTableValues in section_coverage_test.go).
	ended, err := LoadWorktreeRoot(writeGateConfig(t, fixtureRetiredWorktreeMount+"[disk_guard\nmin_free_bytes = 1\n"))
	if err != nil || len(ended.Retired) != 1 || ended.Retired[0].MountFSType != "fuse.sshfs" {
		t.Fatalf("an unrelated malformed header broke [[worktrees.retired]]: %+v %v", ended, err)
	}
}

// #2763: a retired entry is validated exactly like root, and may not alias a recognised root.
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
		"equals current root":  current + "[[worktrees.retired]]\nroot = \"/srv/worktrees\"\n",
		"retired twice":        current + fixtureRetiredWorktreeMount + fixtureRetiredWorktreeMount,
		"single-bracket table": "[worktrees.retired]\nroot = \"/srv/old\"\n",
		"unclosed array table": "[[worktrees.retired]\nroot = \"/srv/old\"\n",
		"invalid current root": "[worktrees]\nroot = \"relative\"\n" + fixtureRetiredWorktreeMount,
	} {
		t.Run(name, func(t *testing.T) {
			if got, err := LoadWorktreeRoot(writeGateConfig(t, body)); err == nil {
				t.Fatalf("accepted %q as %+v", body, got)
			}
		})
	}
	t.Run("equals home worktrees", func(t *testing.T) {
		paths := writeGateConfig(t, "")
		body := fmt.Sprintf("[[worktrees.retired]]\nroot = %q\n", filepath.Join(paths.Home, "worktrees"))
		if err := os.WriteFile(paths.ConfigFile, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := LoadWorktreeRoot(paths); err == nil {
			t.Fatalf("accepted <home>/worktrees as a retired root: %+v", got)
		}
	})
}

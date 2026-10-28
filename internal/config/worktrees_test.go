package config

import (
	"strings"
	"testing"
)

const fixtureWorktreeMount = `root = "/mnt/box/worktrees"
mount_point = "/mnt/box"
mount_source = "user@host:share"
mount_fstype = "fuse.sshfs"
`

// #2375: no section keeps the default layout; a full section is parsed exactly.
func TestLoadWorktreeRootDefaultAndConfigured(t *testing.T) {
	none, err := LoadWorktreeRoot(writeGateConfig(t, "[workflow]\nrequire_workflow = true\n"))
	if err != nil || none.Configured() || none.HasMount() {
		t.Fatalf("no [worktrees] = %+v %v, want the default layout", none, err)
	}
	got, err := LoadWorktreeRoot(writeGateConfig(t, "[worktrees]\n"+fixtureWorktreeMount))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	want := WorktreeRoot{Root: "/mnt/box/worktrees", MountPoint: "/mnt/box", MountSource: "user@host:share", MountFSType: "fuse.sshfs"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	rootOnly, err := LoadWorktreeRoot(writeGateConfig(t, "[worktrees]\nroot = \"/srv/worktrees\"\n"))
	if err != nil || rootOnly.Root != "/srv/worktrees" || rootOnly.HasMount() {
		t.Fatalf("root-only section = %+v %v", rootOnly, err)
	}
}

// #2375: anything ambiguous refuses the file instead of falling back to <home>/worktrees.
func TestLoadWorktreeRootRefusesInvalidSections(t *testing.T) {
	for name, body := range map[string]string{
		"missing root":          "mount_point = \"/mnt/box\"\nmount_source = \"s\"\nmount_fstype = \"fuse.sshfs\"\n",
		"relative root":         "root = \"worktrees\"\n",
		"unclean root":          "root = \"/mnt/box/../worktrees\"\n",
		"filesystem root":       "root = \"/\"\n",
		"partial mount":         "root = \"/mnt/box/worktrees\"\nmount_point = \"/mnt/box\"\n",
		"root outside mount":    strings.Replace(fixtureWorktreeMount, "/mnt/box/worktrees", "/srv/worktrees", 1),
		"root is mount point":   strings.Replace(fixtureWorktreeMount, "/mnt/box/worktrees", "/mnt/box", 1),
		"sibling prefix":        strings.Replace(fixtureWorktreeMount, "/mnt/box/worktrees", "/mnt/boxes/worktrees", 1),
		"unknown key":           fixtureWorktreeMount + "fallback = \"/root/.gitmoot/worktrees\"\n",
		"duplicate key":         fixtureWorktreeMount + "root = \"/mnt/box/other\"\n",
		"unquoted value":        "root = /mnt/box/worktrees\n",
		"section twice":         fixtureWorktreeMount + "[worktrees]\n" + fixtureWorktreeMount,
		"not key value":         fixtureWorktreeMount + "root\n",
		"empty mount source":    strings.Replace(fixtureWorktreeMount, "\"user@host:share\"", "\"\"", 1),
		"relative mount point":  strings.Replace(fixtureWorktreeMount, "mount_point = \"/mnt/box\"", "mount_point = \"mnt/box\"", 1),
		"empty section is root": "",
	} {
		t.Run(name, func(t *testing.T) {
			if got, err := LoadWorktreeRoot(writeGateConfig(t, "[worktrees]\n"+body)); err == nil {
				t.Fatalf("accepted %q as %+v", body, got)
			}
		})
	}
}

func TestWorktreeRootMalformedHeaderRefuses(t *testing.T) {
	if _, err := LoadWorktreeRoot(writeGateConfig(t, "[worktrees\n"+fixtureWorktreeMount)); err == nil {
		t.Fatal("a malformed [worktrees header was accepted")
	}
	got, err := LoadWorktreeRoot(writeGateConfig(t, "[worktrees]\n"+fixtureWorktreeMount+"[disk_guard\nmin_free_bytes = 1\n"))
	if err != nil || got.Root != "/mnt/box/worktrees" {
		t.Fatalf("an unrelated malformed header broke [worktrees]: %+v %v", got, err)
	}
}

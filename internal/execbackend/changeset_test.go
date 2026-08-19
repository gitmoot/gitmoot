package execbackend

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestChangeSetRoundTripsTrackedManifestAndIsIdempotent(t *testing.T) {
	host, sandbox, base := changeSetRepoPair(t)
	writeChangeSetFile(t, sandbox, "tracked.txt", "changed\n", 0o644)
	if err := os.Remove(filepath.Join(sandbox, "deleted.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(sandbox, "script.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(sandbox, "tracked-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("tracked.txt", filepath.Join(sandbox, "tracked-link")); err != nil {
		t.Fatal(err)
	}
	writeChangeSetFile(t, sandbox, "nested/untracked.txt", "new\x00bytes\n", 0o644)
	if err := os.Symlink("untracked.txt", filepath.Join(sandbox, "nested", "untracked-link")); err != nil {
		t.Fatal(err)
	}

	changes, err := BuildChangeSet(context.Background(), sandbox, base)
	if err != nil {
		t.Fatalf("BuildChangeSet: %v", err)
	}
	if len(changes.Patch) == 0 || len(changes.Manifest) != 6 {
		t.Fatalf("changeset patch=%d manifest=%+v, want tracked patch and 6 entries", len(changes.Patch), changes.Manifest)
	}
	if err := ImportChangeSet(context.Background(), host, changes); err != nil {
		t.Fatalf("ImportChangeSet: %v", err)
	}
	assertChangeSetTreesEqual(t, sandbox, host)
	if got := strings.TrimSpace(changeSetGit(t, host, "rev-parse", "HEAD")); got != base {
		t.Fatalf("host HEAD = %s, want unchanged %s", got, base)
	}

	// The same cumulative collection is what a malformed-result repair turn may
	// return. A blind second patch application would fail here.
	if err := ImportChangeSet(context.Background(), host, changes); err != nil {
		t.Fatalf("second ImportChangeSet was not a no-op: %v", err)
	}
	assertChangeSetTreesEqual(t, sandbox, host)
}

func TestChangeSetRoundTripsTrackedFileToDirectoryTransition(t *testing.T) {
	host, sandbox, base := changeSetRepoPair(t)
	if err := os.Remove(filepath.Join(sandbox, "deleted.txt")); err != nil {
		t.Fatal(err)
	}
	writeChangeSetFile(t, sandbox, "deleted.txt/child.txt", "replacement child\n", 0o644)

	changes, err := BuildChangeSet(context.Background(), sandbox, base)
	if err != nil {
		t.Fatalf("BuildChangeSet: %v", err)
	}
	if err := ImportChangeSet(context.Background(), host, changes); err != nil {
		t.Fatalf("ImportChangeSet: %v", err)
	}
	if got, want := changeSetGit(t, host, "status", "--porcelain=v1", "--untracked-files=all"), changeSetGit(t, sandbox, "status", "--porcelain=v1", "--untracked-files=all"); got != want {
		t.Fatalf("status differs\nwant:\n%s\ngot:\n%s", want, got)
	}
	if got := readChangeSetFile(t, host, "deleted.txt/child.txt"); got != "replacement child\n" {
		t.Fatalf("replacement child = %q", got)
	}
	if err := ImportChangeSet(context.Background(), host, changes); err != nil {
		t.Fatalf("idempotent ImportChangeSet: %v", err)
	}
}

func TestChangeSetFileToDirectoryTransitionRollsBack(t *testing.T) {
	host, sandbox, base := changeSetRepoPair(t)
	if err := os.Remove(filepath.Join(sandbox, "deleted.txt")); err != nil {
		t.Fatal(err)
	}
	writeChangeSetFile(t, sandbox, "deleted.txt/nested/child.txt", "replacement child\n", 0o644)
	changes, err := BuildChangeSet(context.Background(), sandbox, base)
	if err != nil {
		t.Fatal(err)
	}
	before := changeSetSnapshot(t, host)
	importer := changeSetImporter{afterMaterialize: func(path string, _ int) error {
		if path == "deleted.txt/nested/child.txt" {
			return context.Canceled
		}
		return nil
	}}
	err = importer.importChangeSet(context.Background(), host, changes)
	if err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("interrupted transition error = %v", err)
	}
	if after := changeSetSnapshot(t, host); after != before {
		t.Fatalf("host tree changed after transition rollback\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestChangeSetIdempotencyRejectsUnmanifestedDirectoryDescendant(t *testing.T) {
	host, sandbox, base := changeSetRepoPair(t)
	if err := os.Remove(filepath.Join(sandbox, "deleted.txt")); err != nil {
		t.Fatal(err)
	}
	writeChangeSetFile(t, sandbox, "deleted.txt/child.txt", "replacement child\n", 0o644)
	changes, err := BuildChangeSet(context.Background(), sandbox, base)
	if err != nil {
		t.Fatal(err)
	}
	if err := ImportChangeSet(context.Background(), host, changes); err != nil {
		t.Fatalf("first ImportChangeSet: %v", err)
	}
	writeChangeSetFile(t, host, "deleted.txt/host-only.txt", "concurrent host content\n", 0o644)

	err = ImportChangeSet(context.Background(), host, changes)
	if err == nil || !strings.Contains(err.Error(), "partial or different materialization") {
		t.Fatalf("ImportChangeSet with unmanifested descendant error = %v", err)
	}
	if got := readChangeSetFile(t, host, "deleted.txt/host-only.txt"); got != "concurrent host content\n" {
		t.Fatalf("unmanifested host descendant changed: %q", got)
	}
}

func TestChangeSetInterruptedMaterializationRollsBackEveryPath(t *testing.T) {
	host, sandbox, base := changeSetRepoPair(t)
	writeChangeSetFile(t, sandbox, "tracked.txt", "changed\n", 0o644)
	writeChangeSetFile(t, sandbox, "new-one.txt", "one\n", 0o644)
	writeChangeSetFile(t, sandbox, "new-two.txt", "two\n", 0o644)
	changes, err := BuildChangeSet(context.Background(), sandbox, base)
	if err != nil {
		t.Fatal(err)
	}
	before := changeSetSnapshot(t, host)
	importer := changeSetImporter{afterMaterialize: func(_ string, completed int) error {
		if completed == 2 {
			return context.Canceled
		}
		return nil
	}}
	err = importer.importChangeSet(context.Background(), host, changes)
	if err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("interrupted import error = %v", err)
	}
	if after := changeSetSnapshot(t, host); after != before {
		t.Fatalf("host tree changed after rollback\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestChangeSetRejectsUnsafePathsWithAttribution(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		kind ChangeKind
		mode uint32
		body []byte
		want string
	}{
		{name: "git metadata", path: "safe/.git/config", kind: ChangeDelete, want: "safe/.git/config"},
		{name: "absolute", path: "/tmp/escape", kind: ChangeDelete, want: "/tmp/escape"},
		{name: "traversal", path: "safe/../../escape", kind: ChangeDelete, want: "safe/../../escape"},
		{name: "escaping symlink", path: "safe/link", kind: ChangeWrite, mode: changeSetSymlinkMode, body: []byte("../../outside"), want: "safe/link"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entry := ChangeManifestEntry{Path: tc.path, Kind: tc.kind, Mode: tc.mode}
			if tc.kind == ChangeWrite {
				entry.Size = int64(len(tc.body))
				entry.Content = tc.body
				digest := sha256.Sum256(tc.body)
				entry.SHA256 = hex.EncodeToString(digest[:])
			}
			err := validateChangeSet(ChangeSet{Version: ChangeSetVersion, BaseHEAD: strings.Repeat("a", 40), Manifest: []ChangeManifestEntry{entry}})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("rejection error = %v, want attributed path %q", err, tc.want)
			}
		})
	}
}

func TestChangeSetRejectsSymlinkIntoGitMetadata(t *testing.T) {
	_, sandbox, base := changeSetRepoPair(t)
	if err := os.Symlink(".git/config", filepath.Join(sandbox, "metadata-link")); err != nil {
		t.Fatal(err)
	}
	_, err := BuildChangeSet(context.Background(), sandbox, base)
	if err == nil || !strings.Contains(err.Error(), "metadata-link") || !strings.Contains(err.Error(), ".git") {
		t.Fatalf("BuildChangeSet metadata symlink error = %v", err)
	}
}

func TestChangeSetRefusesHostHEADMovement(t *testing.T) {
	host, sandbox, base := changeSetRepoPair(t)
	writeChangeSetFile(t, sandbox, "tracked.txt", "sandbox\n", 0o644)
	changes, err := BuildChangeSet(context.Background(), sandbox, base)
	if err != nil {
		t.Fatal(err)
	}
	writeChangeSetFile(t, host, "host-only.txt", "host\n", 0o644)
	changeSetGit(t, host, "add", "host-only.txt")
	changeSetGit(t, host, "commit", "-m", "host moved")
	err = ImportChangeSet(context.Background(), host, changes)
	if err == nil || !strings.Contains(err.Error(), "host HEAD") || !strings.Contains(err.Error(), base) {
		t.Fatalf("ImportChangeSet moved-HEAD error = %v", err)
	}
	if got := readChangeSetFile(t, host, "tracked.txt"); got != "original\n" {
		t.Fatalf("tracked.txt = %q after refused import", got)
	}
}

func TestChangeSetRefusesHostHEADMovementDuringStaging(t *testing.T) {
	host, sandbox, base := changeSetRepoPair(t)
	writeChangeSetFile(t, sandbox, "tracked.txt", "sandbox\n", 0o644)
	changes, err := BuildChangeSet(context.Background(), sandbox, base)
	if err != nil {
		t.Fatal(err)
	}
	materializationStarted := false
	importer := changeSetImporter{
		beforeLock: func() error {
			writeChangeSetFile(t, host, "host-race.txt", "moved while staging\n", 0o644)
			changeSetGit(t, host, "add", "host-race.txt")
			changeSetGit(t, host, "commit", "-m", "move during import")
			return nil
		},
		afterMaterialize: func(string, int) error {
			materializationStarted = true
			return nil
		},
	}
	err = importer.importChangeSet(context.Background(), host, changes)
	if err == nil || !strings.Contains(err.Error(), "host HEAD moved") {
		t.Fatalf("in-flight moved-HEAD error = %v", err)
	}
	if got := readChangeSetFile(t, host, "tracked.txt"); got != "original\n" {
		t.Fatalf("tracked.txt = %q after in-flight HEAD movement", got)
	}
	if materializationStarted {
		t.Fatal("materialization started after in-flight HEAD movement; import must refuse before touching the host tree")
	}
}

func TestChangeSetRollsBackIfHostHEADMovesDuringMaterialization(t *testing.T) {
	host, sandbox, base := changeSetRepoPair(t)
	writeChangeSetFile(t, sandbox, "tracked.txt", "sandbox\n", 0o644)
	changes, err := BuildChangeSet(context.Background(), sandbox, base)
	if err != nil {
		t.Fatal(err)
	}
	writeChangeSetFile(t, host, "racing-commit.txt", "new head\n", 0o644)
	changeSetGit(t, host, "add", "racing-commit.txt")
	changeSetGit(t, host, "commit", "-m", "prepare racing head")
	racingHEAD := strings.TrimSpace(changeSetGit(t, host, "rev-parse", "HEAD"))
	changeSetGit(t, host, "reset", "--hard", base)
	ref := strings.TrimSpace(changeSetGit(t, host, "symbolic-ref", "HEAD"))
	commonDir := strings.TrimSpace(changeSetGit(t, host, "rev-parse", "--path-format=absolute", "--git-common-dir"))
	materializationStarted := false
	importer := changeSetImporter{afterMaterialize: func(string, int) error {
		if materializationStarted {
			return nil
		}
		materializationStarted = true
		// This deliberately bypasses Git's ref lock to model a hostile writer.
		// Ordinary git update-ref/commit cannot move the ref while import holds it.
		return os.WriteFile(filepath.Join(commonDir, filepath.FromSlash(ref)), []byte(racingHEAD+"\n"), 0o644)
	}}
	err = importer.importChangeSet(context.Background(), host, changes)
	if err == nil || !strings.Contains(err.Error(), "host HEAD moved") {
		t.Fatalf("mid-materialize moved-HEAD error = %v", err)
	}
	if !materializationStarted {
		t.Fatal("test did not reach materialization")
	}
	if got := readChangeSetFile(t, host, "tracked.txt"); got != "original\n" {
		t.Fatalf("tracked.txt = %q after final HEAD check rollback", got)
	}
}

func TestChangeSetLocksSymbolicHEADDuringMaterialization(t *testing.T) {
	host, sandbox, base := changeSetRepoPair(t)
	writeChangeSetFile(t, sandbox, "tracked.txt", "sandbox\n", 0o644)
	changes, err := BuildChangeSet(context.Background(), sandbox, base)
	if err != nil {
		t.Fatal(err)
	}
	writeChangeSetFile(t, host, "alternate.txt", "alternate head\n", 0o644)
	changeSetGit(t, host, "add", "alternate.txt")
	changeSetGit(t, host, "commit", "-m", "alternate head")
	alternateHEAD := strings.TrimSpace(changeSetGit(t, host, "rev-parse", "HEAD"))
	changeSetGit(t, host, "reset", "--hard", base)
	checkoutSucceeded := false
	importer := changeSetImporter{afterMaterialize: func(string, int) error {
		cmd := exec.Command("git", "-C", host, "checkout", "--detach", alternateHEAD)
		if output, err := cmd.CombinedOutput(); err == nil {
			checkoutSucceeded = true
		} else if !strings.Contains(string(output), "HEAD.lock") && !strings.Contains(string(output), "Unable to create") {
			return fmt.Errorf("checkout failed for an unexpected reason: %w: %s", err, output)
		}
		return nil
	}}
	if err := importer.importChangeSet(context.Background(), host, changes); err != nil {
		t.Fatalf("ImportChangeSet: %v", err)
	}
	if checkoutSucceeded {
		t.Fatal("concurrent checkout moved symbolic HEAD during materialization")
	}
	if got := strings.TrimSpace(changeSetGit(t, host, "rev-parse", "HEAD")); got != base {
		t.Fatalf("host HEAD = %s after blocked checkout, want %s", got, base)
	}
}

func TestChangeSetForbidsSandboxCreatedCommit(t *testing.T) {
	_, sandbox, base := changeSetRepoPair(t)
	writeChangeSetFile(t, sandbox, "tracked.txt", "committed in sandbox\n", 0o644)
	changeSetGit(t, sandbox, "add", "tracked.txt")
	changeSetGit(t, sandbox, "commit", "-m", "forbidden")
	_, err := BuildChangeSet(context.Background(), sandbox, base)
	if err == nil || !strings.Contains(err.Error(), "sandbox-created commits are forbidden") {
		t.Fatalf("BuildChangeSet commit error = %v", err)
	}
}

func TestChangeSetRejectsTrackedHashMismatchBeforeMaterialization(t *testing.T) {
	host, sandbox, base := changeSetRepoPair(t)
	writeChangeSetFile(t, sandbox, "tracked.txt", "tampered manifest\n", 0o644)
	changes, err := BuildChangeSet(context.Background(), sandbox, base)
	if err != nil {
		t.Fatal(err)
	}
	for index := range changes.Manifest {
		if changes.Manifest[index].Path == "tracked.txt" {
			changes.Manifest[index].SHA256 = strings.Repeat("0", sha256.Size*2)
		}
	}
	err = ImportChangeSet(context.Background(), host, changes)
	if err == nil || !strings.Contains(err.Error(), `path "tracked.txt" failed staged content verification`) {
		t.Fatalf("hash mismatch error = %v", err)
	}
	if got := readChangeSetFile(t, host, "tracked.txt"); got != "original\n" {
		t.Fatalf("tracked.txt materialized despite hash mismatch: %q", got)
	}
}

func TestChangeSetManifestEntryBoundIsEnforced(t *testing.T) {
	entries := make([]ChangeManifestEntry, MaxChangeSetEntries+1)
	for index := range entries {
		entries[index] = ChangeManifestEntry{Path: fmt.Sprintf("path-%d", index), Kind: ChangeDelete, Tracked: true}
	}
	err := validateChangeSet(ChangeSet{Version: ChangeSetVersion, BaseHEAD: strings.Repeat("a", 40), Manifest: entries})
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("limit %d", MaxChangeSetEntries)) {
		t.Fatalf("manifest bound error = %v", err)
	}
}

func TestChangeSetRefusesHostPathChangedFromBase(t *testing.T) {
	host, sandbox, base := changeSetRepoPair(t)
	writeChangeSetFile(t, sandbox, "tracked.txt", "sandbox\n", 0o644)
	changes, err := BuildChangeSet(context.Background(), sandbox, base)
	if err != nil {
		t.Fatal(err)
	}
	writeChangeSetFile(t, host, "tracked.txt", "concurrent host edit\n", 0o644)
	err = ImportChangeSet(context.Background(), host, changes)
	if err == nil || !strings.Contains(err.Error(), `host path "tracked.txt" changed from base`) {
		t.Fatalf("host path collision error = %v", err)
	}
	if got := readChangeSetFile(t, host, "tracked.txt"); got != "concurrent host edit\n" {
		t.Fatalf("host edit overwritten despite refusal: %q", got)
	}
}

func changeSetRepoPair(t *testing.T) (host, sandbox, base string) {
	t.Helper()
	host = filepath.Join(t.TempDir(), "host")
	if err := os.MkdirAll(host, 0o755); err != nil {
		t.Fatal(err)
	}
	changeSetGit(t, host, "init")
	changeSetGit(t, host, "config", "user.email", "tests@gitmoot.local")
	changeSetGit(t, host, "config", "user.name", "Gitmoot Tests")
	writeChangeSetFile(t, host, "tracked.txt", "original\n", 0o644)
	writeChangeSetFile(t, host, "deleted.txt", "delete me\n", 0o644)
	writeChangeSetFile(t, host, "script.sh", "#!/bin/sh\n", 0o644)
	if err := os.Symlink("deleted.txt", filepath.Join(host, "tracked-link")); err != nil {
		t.Fatal(err)
	}
	changeSetGit(t, host, "add", "-A")
	changeSetGit(t, host, "commit", "-m", "base")
	base = strings.TrimSpace(changeSetGit(t, host, "rev-parse", "HEAD"))
	sandbox = filepath.Join(filepath.Dir(host), "sandbox")
	cmd := exec.Command("git", "clone", "--no-hardlinks", host, sandbox)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git clone: %v: %s", err, output)
	}
	changeSetGit(t, sandbox, "config", "user.email", "tests@gitmoot.local")
	changeSetGit(t, sandbox, "config", "user.name", "Gitmoot Tests")
	return host, sandbox, base
}

func changeSetGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
	}
	return string(output)
}

func writeChangeSetFile(t *testing.T, root, name, content string, mode os.FileMode) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func readChangeSetFile(t *testing.T, root, name string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

func assertChangeSetTreesEqual(t *testing.T, want, got string) {
	t.Helper()
	wantStatus := changeSetGit(t, want, "status", "--porcelain=v1", "--untracked-files=all")
	gotStatus := changeSetGit(t, got, "status", "--porcelain=v1", "--untracked-files=all")
	if gotStatus != wantStatus {
		t.Fatalf("status differs\nwant:\n%s\ngot:\n%s", wantStatus, gotStatus)
	}
	for _, path := range []string{"tracked.txt", "script.sh", "tracked-link", "nested/untracked.txt", "nested/untracked-link"} {
		wantNode, err := inspectNode(want, path)
		if err != nil {
			t.Fatal(err)
		}
		gotNode, err := inspectNode(got, path)
		if err != nil {
			t.Fatal(err)
		}
		if !sameNode(wantNode, gotNode) {
			t.Fatalf("node %s differs: want %+v got %+v", path, wantNode, gotNode)
		}
	}
	if _, err := os.Lstat(filepath.Join(got, "deleted.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted.txt still exists: %v", err)
	}
}

func changeSetSnapshot(t *testing.T, root string) string {
	t.Helper()
	status := changeSetGit(t, root, "status", "--porcelain=v1", "--untracked-files=all")
	var builder strings.Builder
	builder.WriteString(status)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if rel == ".git" || strings.HasPrefix(rel, ".git"+string(filepath.Separator)) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		content, mode, err := readNode(root, filepath.ToSlash(rel), info)
		if err != nil {
			return err
		}
		fmt.Fprintf(&builder, "%s %o %x\n", filepath.ToSlash(rel), mode, sha256.Sum256(content))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return builder.String()
}

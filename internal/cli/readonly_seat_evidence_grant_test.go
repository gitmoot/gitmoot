package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/runtime"
)

// TestReviewEvidenceReadFilesGrantsTheStoreAndNothingElse is #1839's evidence
// guard: a read-only review seat could not read the workflow store at all, so
// it had no way to enumerate the prior verdicts on the head it was reviewing.
//
// It also pins the SECURITY SHAPE, which is the reason this is files and not a
// directory: credential-bearing state lives beside the database, so a grant of
// the parent would hand a reviewer host credentials to buy it evidence.
func TestReviewEvidenceReadFilesGrantsTheStoreAndNothingElse(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := os.MkdirAll(paths.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.Database, []byte("sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A credential that lives in the same directory and must NOT be granted.
	secret := filepath.Join(paths.Home, "bridge.token")
	if err := os.WriteFile(secret, []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}

	files := reviewEvidenceReadFiles(paths)
	if len(files) == 0 {
		t.Fatal("no evidence files granted: a seat cannot enumerate prior verdicts, which is the #1839 defect")
	}
	found := false
	for _, file := range files {
		if file == paths.Database {
			found = true
		}
		if file == secret {
			t.Errorf("the credential beside the store was granted: %s", file)
		}
		if file == paths.Home {
			t.Errorf("the store DIRECTORY was granted, which exposes everything beside the database: %s", file)
		}
		if info, err := os.Stat(file); err != nil || info.IsDir() {
			t.Errorf("granted evidence path %q is not an existing file (err=%v)", file, err)
		}
	}
	if !found {
		t.Errorf("granted files %v do not include the database %q", files, paths.Database)
	}

	// SIDECARS: granted when present, skipped when absent - a missing WAL is
	// the normal case and readableFiles refuses a path that does not exist, so
	// a blanket list would fail every seat on a quiet database.
	if err := os.WriteFile(paths.Database+"-wal", []byte("wal"), 0o600); err != nil {
		t.Fatal(err)
	}
	withWAL := reviewEvidenceReadFiles(paths)
	if len(withWAL) != len(files)+1 {
		t.Errorf("granted %d files with a WAL present, want %d: sqlite needs its sidecar to read a live database", len(withWAL), len(files)+1)
	}
	for _, file := range withWAL {
		if strings.HasSuffix(file, "-shm") {
			t.Errorf("granted a sidecar that does not exist: %s", file)
		}
	}
}

// TestReviewEvidenceReadFilesIsQuietWithoutAStore is the success-path control:
// the guard must not refuse or fabricate a grant when there is no database, so
// a valid read-only review on a fresh home still starts.
func TestReviewEvidenceReadFilesIsQuietWithoutAStore(t *testing.T) {
	paths := config.PathsForHome(t.TempDir())
	if files := reviewEvidenceReadFiles(paths); len(files) != 0 {
		t.Fatalf("granted %v with no store on disk, want none", files)
	}
	if files := reviewEvidenceReadFiles(config.Paths{}); len(files) != 0 {
		t.Fatalf("granted %v for an empty Paths, want none", files)
	}
}

// TestReadOnlyRuntimeSandboxGrantsIncludesTheStore drives the PRODUCTION grant
// builder, not the helper.
//
// The helper tests above passed while the wiring was removed: a mutant that
// deleted the append in readOnlyRuntimeSandboxGrants SURVIVED them. That is the
// "pins a helper the production path never reaches" trap, so this asks the
// builder what a real seat would actually be granted.
func TestReadOnlyRuntimeSandboxGrantsIncludesTheStore(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := os.MkdirAll(paths.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.Database, []byte("sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}

	checkout := t.TempDir()
	runGit(t, checkout, "init", "-b", "main")
	runGit(t, checkout, "config", "user.email", "gitmoot@example.com")
	runGit(t, checkout, "config", "user.name", "Gitmoot")
	runGit(t, checkout, "commit", "--allow-empty", "-m", "init")

	agent := runtime.Agent{Name: "seat", Runtime: runtime.CodexRuntime, ReadOnlySeat: true}
	grants, err := readOnlyRuntimeSandboxGrants(home, agent, checkout, true)
	if err != nil {
		t.Fatalf("readOnlyRuntimeSandboxGrants: %v", err)
	}

	granted := false
	for _, file := range grants.readFiles {
		if file == paths.Database {
			granted = true
		}
	}
	if !granted {
		t.Fatalf("readFiles = %v, want the workflow store %q: without it a review seat cannot enumerate prior verdicts", grants.readFiles, paths.Database)
	}
	for _, root := range grants.reads {
		if root == paths.Home {
			t.Errorf("reads include the gitmoot home %q, which exposes every credential beside the store", root)
		}
	}
}

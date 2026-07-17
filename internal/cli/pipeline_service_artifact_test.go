package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/proof"
)

func TestCopyPipelineServiceStageOutRejectsSymlinkEscape(t *testing.T) {
	paths := config.Paths{Home: t.TempDir()}
	worktree := filepath.Join(paths.Home, "workspaces", "service-stage")
	out := filepath.Join(worktree, "out")
	if err := os.MkdirAll(out, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("must-not-copy"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(out, "escape")); err != nil {
		t.Fatal(err)
	}

	const runID = "psr-0123456789abcdef0123456789abcdef"
	err := copyPipelineServiceStageOut(paths, runID, "build", worktree)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("copyPipelineServiceStageOut error = %v, want symlink rejection", err)
	}
	_, base, _, _, pathErr := pipelineServiceRunPaths(paths, runID)
	if pathErr != nil {
		t.Fatal(pathErr)
	}
	if _, statErr := os.Stat(filepath.Join(base, pipelineServiceArtifactsDir, "build")); !os.IsNotExist(statErr) {
		t.Fatalf("rejected collection published a stage directory: %v", statErr)
	}
}

// A crash between MkdirTemp and the atomic rename can orphan a hidden
// ".<stage>-XXXX" staging dir under artifacts/; neither delivery nor the
// aggregate-size accounting may ever ingest those partial bytes.
func TestLoadPipelineServiceArtifactsSkipsOrphanTempDir(t *testing.T) {
	paths := config.Paths{Home: t.TempDir()}
	const runID = "psr-0123456789abcdef0123456789abcdef"
	_, base, _, _, err := pipelineServiceRunPaths(paths, runID)
	if err != nil {
		t.Fatal(err)
	}
	artifactsRoot := filepath.Join(base, pipelineServiceArtifactsDir)
	// A real committed stage output.
	if err := os.MkdirAll(filepath.Join(artifactsRoot, "build"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artifactsRoot, "build", "kit.txt"), []byte("kit"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A crash-orphaned hidden staging dir with partial bytes.
	if err := os.MkdirAll(filepath.Join(artifactsRoot, ".build-orphan"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artifactsRoot, ".build-orphan", "partial.bin"), []byte("garbage-partial"), 0o600); err != nil {
		t.Fatal(err)
	}

	entries, err := loadPipelineServiceArtifacts(paths, runID)
	if err != nil {
		t.Fatalf("loadPipelineServiceArtifacts error = %v", err)
	}
	if len(entries) != 1 || entries[0].Relpath != "artifacts/build/kit.txt" {
		t.Fatalf("loaded artifacts = %+v, want only artifacts/build/kit.txt (orphan skipped)", entries)
	}

	committed, err := pipelineServiceArtifactsCommittedBytes(artifactsRoot)
	if err != nil {
		t.Fatalf("committed bytes error = %v", err)
	}
	if committed != int64(len("kit")) {
		t.Fatalf("committed bytes = %d, want %d (orphan bytes excluded)", committed, len("kit"))
	}
}

func TestVerifyPipelineServiceArtifactProofRejectsDigestMismatch(t *testing.T) {
	root := db.Job{ID: "root", RootID: "root", Type: "ask", State: "succeeded", UpdatedAt: "2026-07-17 00:00:00"}
	manifest := proof.Project(root, []db.Job{root}, nil, nil, nil)
	want := proof.ArtifactEvidence{
		Relpath: "artifacts/build/kit.txt", Stage: "build", Size: 3,
		SHA256: "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
	}
	manifest, err := proof.WithArtifactNodes(manifest, []proof.ArtifactEvidence{want}, "2026-07-17T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	actual := want
	actual.SHA256 = "cb8379ac2098aa165029e3938a51da0bcecfc008fd6795f401178647f96c5b34"
	if err := verifyPipelineServiceArtifactProof(manifest, []proof.ArtifactEvidence{actual}); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("verifyPipelineServiceArtifactProof error = %v, want digest mismatch", err)
	}
	base := t.TempDir()
	artifactPath := filepath.Join(base, filepath.FromSlash(want.Relpath))
	if err := os.MkdirAll(filepath.Dir(artifactPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(artifactPath, []byte("xyz"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "bundle.zip")
	if err := writePipelineServiceArchive(base, archive, []byte(`{}`), []byte(`{}`), true); err != nil {
		t.Fatal(err)
	}
	if err := verifyPipelineServiceArchiveArtifacts(archive, manifest); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("verifyPipelineServiceArchiveArtifacts error = %v, want digest mismatch", err)
	}
}

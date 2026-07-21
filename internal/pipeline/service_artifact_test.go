package pipeline

import (
	"github.com/gitmoot/gitmoot/internal/config"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	_, base, _, _, pathErr := PipelineServiceRunPaths(paths, runID)
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
	_, base, _, _, err := PipelineServiceRunPaths(paths, runID)
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

	entries, err := LoadPipelineServiceArtifacts(paths, runID)
	if err != nil {
		t.Fatalf("LoadPipelineServiceArtifacts error = %v", err)
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

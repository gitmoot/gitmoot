package cli

import (
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/proof"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

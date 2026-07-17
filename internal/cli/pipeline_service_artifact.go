package cli

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/pipeline"
	"github.com/gitmoot/gitmoot/internal/proof"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

const (
	pipelineServiceArtifactMaxBytes      int64 = 64 << 20
	pipelineServiceArtifactsDir                = "artifacts"
	pipelineServiceArtifactFailureMarker       = ".artifact-collection-failed"
)

var (
	errPipelineServiceArtifactCollectionFailed = errors.New("artifact_collection_failed")
	errPipelineServiceArtifactBundleTooLarge   = errors.New("artifact_bundle_too_large")
)

// pipelineServiceArtifactPrecleanupHook is wired into workflow.Engine's
// terminal read-only cleanup. AdvanceJob invokes it while the service shell
// worktree still exists and cleanup removes the worktree immediately after it
// returns. Pipeline stage settlement happens on a later daemon scan, so this is
// the last reliable collection point that preserves normal disposal.
func pipelineServiceArtifactPrecleanupHook(store *db.Store, paths config.Paths) func(context.Context, string, string, workflow.JobPayload) error {
	return func(ctx context.Context, jobID, jobType string, payload workflow.JobPayload) error {
		if payload.Sender != workflow.PipelineJobSender || payload.RuntimeOverride != runtime.ShellRuntime ||
			!payload.ReadOnlyWorktree || strings.TrimSpace(payload.WorktreePath) == "" ||
			!pipelineServiceRunIDPattern.MatchString(strings.TrimSpace(payload.RootJobID)) {
			return nil
		}
		runID := strings.TrimSpace(payload.RootJobID)
		if err := collectPipelineServiceStageArtifacts(ctx, store, paths, runID, jobID, jobType, payload); err != nil {
			markErr := markPipelineServiceArtifactCollectionFailed(paths, runID, err)
			return errors.Join(err, markErr)
		}
		return nil
	}
}

func collectPipelineServiceStageArtifacts(ctx context.Context, store *db.Store, paths config.Paths, runID, jobID, jobType string, payload workflow.JobPayload) error {
	if jobType != "ask" {
		return fmt.Errorf("service shell job %q has unexpected type %q", jobID, jobType)
	}
	run, ok, err := store.GetPipelineRun(ctx, runID)
	if err != nil {
		return err
	}
	if !ok || run.Trigger != "service" {
		return fmt.Errorf("service shell job %q has no service run", jobID)
	}
	job, err := store.GetJob(ctx, jobID)
	if err != nil {
		return err
	}
	if job.State != string(workflow.JobSucceeded) {
		return nil // failed/blocked attempts never contribute deliverables
	}
	stage, spec, err := servicePipelineStageForJob(ctx, store, paths, runID, jobID)
	if err != nil {
		return err
	}
	if payload.Result == nil || !serviceContainsString(spec.EffectiveSuccessDecisions(stage), payload.Result.Decision) {
		return nil // a terminal job can still be a pipeline-level non-success decision
	}
	return copyPipelineServiceStageOut(paths, runID, stage.ID, payload.WorktreePath)
}

func servicePipelineStageForJob(ctx context.Context, store *db.Store, paths config.Paths, runID, jobID string) (pipeline.Stage, pipeline.Spec, error) {
	_, _, sourceSpec, _, err := pipelineServiceRunPaths(paths, runID)
	if err != nil {
		return pipeline.Stage{}, pipeline.Spec{}, err
	}
	raw, err := os.ReadFile(sourceSpec)
	if err != nil {
		return pipeline.Stage{}, pipeline.Spec{}, fmt.Errorf("read frozen service spec: %w", err)
	}
	spec, err := pipeline.Load(raw)
	if err != nil {
		return pipeline.Stage{}, pipeline.Spec{}, fmt.Errorf("parse frozen service spec: %w", err)
	}
	rows, err := store.ListPipelineRunStages(ctx, runID)
	if err != nil {
		return pipeline.Stage{}, pipeline.Spec{}, err
	}
	stageID := ""
	for _, row := range rows {
		if row.JobID == jobID {
			stageID = row.StageID
			break
		}
	}
	for _, stage := range spec.Stages {
		if stage.ID == stageID {
			return stage, spec, nil
		}
	}
	// A pool worker can finish in the narrow interval after enqueue and before the
	// scanner persists stage.job_id. Recover the identity from the deterministic
	// stage/attempt ids rather than dropping the output.
	for _, stage := range spec.Stages {
		for attempt := 0; attempt <= stage.Retry; attempt++ {
			if pipelineStageJobID(runID, stage.ID, attempt) == jobID {
				return stage, spec, nil
			}
		}
	}
	return pipeline.Stage{}, pipeline.Spec{}, fmt.Errorf("service job %q does not identify a frozen stage", jobID)
}

func copyPipelineServiceStageOut(paths config.Paths, runID, stageID, worktree string) error {
	if !containedRelativePath(stageID) || strings.ContainsAny(stageID, `/\\`) {
		return fmt.Errorf("invalid service stage id %q", stageID)
	}
	worktree = filepath.Clean(strings.TrimSpace(worktree))
	if rel, err := filepath.Rel(filepath.Clean(paths.Home), worktree); err != nil || !containedRelativePath(rel) {
		return errors.New("service stage worktree is outside gitmoot home")
	}
	outRoot := filepath.Join(worktree, "out")
	if rel, err := filepath.Rel(worktree, outRoot); err != nil || !containedRelativePath(rel) {
		return errors.New("service stage out directory escaped worktree")
	}
	info, err := os.Lstat(outRoot)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("service stage out must be a real directory")
	}
	_, base, _, _, err := pipelineServiceRunPaths(paths, runID)
	if err != nil {
		return err
	}
	artifactsRoot := filepath.Join(base, pipelineServiceArtifactsDir)
	if err := os.MkdirAll(artifactsRoot, 0o700); err != nil {
		return err
	}
	// Bytes already committed by earlier stages of this run count toward the same
	// aggregate 64 MiB budget, so start the running total there and enforce the cap
	// INSIDE the copy loop — before any oversized bytes are written to durable home
	// storage. This makes the copy self-bounding (never more than the cap) rather
	// than copying an unbounded tree and only rejecting it later at finalize.
	committed, err := pipelineServiceArtifactsCommittedBytes(artifactsRoot)
	if err != nil {
		return err
	}
	temp, err := os.MkdirTemp(artifactsRoot, "."+stageID+"-")
	if err != nil {
		return err
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(temp)
		}
	}()
	staged := int64(0)
	err = filepath.WalkDir(outRoot, func(source string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(outRoot, source)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if !containedRelativePath(rel) {
			return fmt.Errorf("artifact path %q escapes stage out", rel)
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("artifact path %q is a symlink", rel)
		}
		destination := filepath.Join(temp, rel)
		if dstRel, err := filepath.Rel(temp, destination); err != nil || !containedRelativePath(dstRel) {
			return fmt.Errorf("artifact destination %q escapes staging", rel)
		}
		if entry.IsDir() {
			return os.MkdirAll(destination, 0o700)
		}
		fileInfo, err := entry.Info()
		if err != nil {
			return err
		}
		if !fileInfo.Mode().IsRegular() {
			return fmt.Errorf("artifact path %q is not a regular file", rel)
		}
		staged += fileInfo.Size()
		if committed+staged > pipelineServiceArtifactMaxBytes {
			return errPipelineServiceArtifactBundleTooLarge
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			return err
		}
		return copyPipelineServiceArtifactFile(source, destination)
	})
	if err != nil {
		return err
	}
	target := filepath.Join(artifactsRoot, stageID)
	if err := os.RemoveAll(target); err != nil {
		return err
	}
	if err := os.Rename(temp, target); err != nil {
		return err
	}
	keep = true
	return nil
}

func copyPipelineServiceArtifactFile(source, destination string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	syncErr := out.Sync()
	closeErr := out.Close()
	return errors.Join(copyErr, syncErr, closeErr)
}

// pipelineServiceArtifactsCommittedBytes sums the regular-file bytes already
// committed under artifactsRoot by earlier stages, skipping hidden staging dirs
// (".<stage>-XXXX") so an in-progress or crash-orphaned temp tree never counts
// toward — and is never double-counted against — the aggregate budget.
func pipelineServiceArtifactsCommittedBytes(artifactsRoot string) (int64, error) {
	if _, err := os.Stat(artifactsRoot); os.IsNotExist(err) {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	var total int64
	err := filepath.WalkDir(artifactsRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != artifactsRoot && strings.HasPrefix(entry.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}

// markPipelineServiceArtifactCollectionFailed records a durable, fail-closed
// marker so finalize refuses to produce a receipt. It preserves the specific
// cause (e.g. artifact_bundle_too_large) so the caller sees why, defaulting to
// the generic collection-failed reason.
func markPipelineServiceArtifactCollectionFailed(paths config.Paths, runID string, cause error) error {
	root, _, _, _, err := pipelineServiceRunPaths(paths, runID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	reason := errPipelineServiceArtifactCollectionFailed.Error()
	if errors.Is(cause, errPipelineServiceArtifactBundleTooLarge) {
		reason = errPipelineServiceArtifactBundleTooLarge.Error()
	}
	return os.WriteFile(filepath.Join(root, pipelineServiceArtifactFailureMarker), []byte(reason+"\n"), 0o600)
}

// loadPipelineServiceArtifacts hashes the durable staging tree and enforces the
// aggregate 64 MiB cap before proof or archive creation. It rejects symlinks and
// non-regular files again so a local post-collection mutation fails closed.
func loadPipelineServiceArtifacts(paths config.Paths, runID string) ([]proof.ArtifactEvidence, error) {
	root, base, _, _, err := pipelineServiceRunPaths(paths, runID)
	if err != nil {
		return nil, err
	}
	if marker, err := os.ReadFile(filepath.Join(root, pipelineServiceArtifactFailureMarker)); err == nil {
		if strings.TrimSpace(string(marker)) == errPipelineServiceArtifactBundleTooLarge.Error() {
			return nil, errPipelineServiceArtifactBundleTooLarge
		}
		return nil, errPipelineServiceArtifactCollectionFailed
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	artifactsRoot := filepath.Join(base, pipelineServiceArtifactsDir)
	if _, err := os.Stat(artifactsRoot); os.IsNotExist(err) {
		return []proof.ArtifactEvidence{}, nil
	} else if err != nil {
		return nil, err
	}
	entries := make([]proof.ArtifactEvidence, 0)
	var total int64
	err = filepath.WalkDir(artifactsRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			// Skip hidden staging dirs (".<stage>-XXXX"): a crash between MkdirTemp
			// and the atomic rename can orphan a partial tree here, and it must never
			// be ingested as a delivered artifact.
			if path != artifactsRoot && strings.HasPrefix(entry.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(base, path)
		if err != nil || !containedRelativePath(rel) {
			return errors.New("collected artifact escaped service bundle")
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("collected artifact %q is a symlink", rel)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("collected artifact %q is not a regular file", rel)
		}
		total += info.Size()
		if total > pipelineServiceArtifactMaxBytes {
			return errPipelineServiceArtifactBundleTooLarge
		}
		digest, err := hashPipelineServiceArtifactHex(path)
		if err != nil {
			return err
		}
		slashRel := filepath.ToSlash(rel)
		parts := strings.Split(slashRel, "/")
		if len(parts) < 3 || parts[0] != pipelineServiceArtifactsDir {
			return fmt.Errorf("collected artifact %q lacks a stage namespace", slashRel)
		}
		entries = append(entries, proof.ArtifactEvidence{Relpath: slashRel, Stage: parts[1], Size: info.Size(), SHA256: digest})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Relpath < entries[j].Relpath })
	return entries, nil
}

func hashPipelineServiceArtifactHex(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func verifyPipelineServiceArtifactProof(manifest proof.Manifest, actual []proof.ArtifactEvidence) error {
	committed, err := proof.ArtifactEntries(manifest)
	if err != nil {
		return err
	}
	if len(committed) != len(actual) {
		return fmt.Errorf("artifact proof count mismatch: manifest=%d actual=%d", len(committed), len(actual))
	}
	for i := range actual {
		if committed[i].Relpath != actual[i].Relpath || committed[i].Size != actual[i].Size || committed[i].SHA256 != actual[i].SHA256 {
			return fmt.Errorf("artifact proof digest mismatch for %q", actual[i].Relpath)
		}
	}
	return nil
}

// verifyPipelineServiceArchiveArtifacts binds the bytes in the completed,
// caller-facing archive to the artifact nodes, closing the gap between hashing
// the staging tree and packaging it. It also rejects missing or extra artifact
// entries.
func verifyPipelineServiceArchiveArtifacts(archivePath string, manifest proof.Manifest) error {
	committed, err := proof.ArtifactEntries(manifest)
	if err != nil {
		return err
	}
	want := make(map[string]proof.ArtifactEvidence, len(committed))
	for _, artifact := range committed {
		want[artifact.Relpath] = artifact
	}
	archive, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer archive.Close()
	seen := make(map[string]struct{}, len(committed))
	for _, file := range archive.File {
		if !strings.HasPrefix(file.Name, pipelineServiceArtifactsDir+"/") {
			continue
		}
		expected, ok := want[file.Name]
		if !ok {
			return fmt.Errorf("archive contains uncommitted artifact %q", file.Name)
		}
		if _, duplicate := seen[file.Name]; duplicate {
			return fmt.Errorf("archive contains duplicate artifact %q", file.Name)
		}
		if file.UncompressedSize64 > uint64(pipelineServiceArtifactMaxBytes) || int64(file.UncompressedSize64) != expected.Size {
			return fmt.Errorf("archive artifact size mismatch for %q", file.Name)
		}
		reader, err := file.Open()
		if err != nil {
			return err
		}
		hash := sha256.New()
		_, copyErr := io.Copy(hash, reader)
		closeErr := reader.Close()
		if err := errors.Join(copyErr, closeErr); err != nil {
			return err
		}
		if hex.EncodeToString(hash.Sum(nil)) != expected.SHA256 {
			return fmt.Errorf("archive artifact digest mismatch for %q", file.Name)
		}
		seen[file.Name] = struct{}{}
	}
	if len(seen) != len(want) {
		return fmt.Errorf("archive artifact count mismatch: archive=%d manifest=%d", len(seen), len(want))
	}
	return nil
}

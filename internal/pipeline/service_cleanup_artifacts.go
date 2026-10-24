package pipeline

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// Canonical stage publication replaces directories, never file contents. Hard
// links preserve the old generation without copying its bytes again or keeping
// the disposable workspace alive. The manifest and payload bind that snapshot.
func preservePipelineCleanupArtifacts(paths config.Paths, runID, stageID string, job db.Job, worktree string) (workflow.CleanupArtifactSnapshot, error) {
	resourceID := db.CleanupObligationResourceID(job.ID, worktree)
	snapshot := workflow.CleanupArtifactSnapshot{
		ResourceID: resourceID, Generation: job.LifecycleGeneration,
		Directory: workflow.CleanupArtifactDirectory(paths.Home, resourceID, job.LifecycleGeneration),
		Files:     make([]workflow.CleanupArtifactFile, 0),
	}
	_, base, _, _, err := PipelineServiceRunPaths(paths, runID)
	if err != nil {
		return snapshot, err
	}
	sourceRoot := filepath.Join(base, pipelineServiceArtifactsDir, stageID)
	parent := filepath.Dir(snapshot.Directory)
	if err := os.MkdirAll(parent, 0700); err != nil {
		return snapshot, err
	}
	temp, err := os.MkdirTemp(parent, ".snapshot-")
	if err != nil {
		return snapshot, err
	}
	defer os.RemoveAll(temp)
	if err := os.Mkdir(filepath.Join(temp, "files"), 0700); err != nil {
		return snapshot, err
	}
	var total int64
	err = filepath.WalkDir(sourceRoot, func(source string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if source == sourceRoot && os.IsNotExist(walkErr) {
				return nil
			}
			return walkErr
		}
		rel, err := filepath.Rel(sourceRoot, source)
		if err != nil {
			return err
		}
		if rel == "." {
			if !entry.IsDir() {
				return errors.New("published service artifact root is not a directory")
			}
			return nil
		}
		if !containedRelativePath(rel) || entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("unsafe published artifact %q", rel)
		}
		destination := filepath.Join(temp, "files", rel)
		if entry.IsDir() {
			return os.MkdirAll(destination, 0700)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("published artifact %q is not regular", rel)
		}
		total += info.Size()
		if total > pipelineServiceArtifactMaxBytes {
			return ErrPipelineServiceArtifactBundleTooLarge
		}
		digest, err := hashPipelineServiceArtifactHex(source)
		if err != nil {
			return err
		}
		if err := os.Link(source, destination); err != nil {
			return err
		}
		snapshot.Files = append(snapshot.Files, workflow.CleanupArtifactFile{
			Path: filepath.ToSlash(filepath.Join("files", rel)), Size: info.Size(), SHA256: digest,
		})
		return nil
	})
	if err != nil {
		return snapshot, err
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return snapshot, err
	}
	manifest, err := os.OpenFile(filepath.Join(temp, "manifest.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return snapshot, err
	}
	_, writeErr := manifest.Write(encoded)
	if err := errors.Join(writeErr, manifest.Sync(), manifest.Close()); err != nil {
		return snapshot, err
	}
	var directories []string
	if err := filepath.WalkDir(temp, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			directories = append(directories, path)
		}
		return nil
	}); err != nil {
		return snapshot, err
	}
	for i := len(directories) - 1; i >= 0; i-- {
		if err := syncCleanupArtifactDirectory(directories[i]); err != nil {
			return snapshot, err
		}
	}
	if err := os.Rename(temp, snapshot.Directory); err != nil {
		// A previous or concurrent publication may have completed this immutable
		// generation. It is reusable only if its manifest and bytes agree.
		existing, readErr := os.ReadFile(filepath.Join(snapshot.Directory, "manifest.json"))
		if readErr != nil {
			return snapshot, errors.Join(err, readErr)
		}
		if !bytes.Equal(existing, encoded) {
			return snapshot, errors.New("artifact snapshot changed within one lifecycle generation")
		}
	}
	for directory := parent; ; directory = filepath.Dir(directory) {
		if err := syncCleanupArtifactDirectory(directory); err != nil {
			return snapshot, err
		}
		if directory == filepath.Clean(paths.Home) {
			break
		}
		if directory == filepath.Dir(directory) {
			return snapshot, errors.New("artifact snapshot escaped durable home")
		}
	}
	if err := workflow.ValidateCleanupArtifactSnapshot(paths.Home, resourceID, job.LifecycleGeneration, snapshot); err != nil {
		return snapshot, err
	}
	return snapshot, nil
}

func syncCleanupArtifactDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

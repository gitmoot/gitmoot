package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
)

// CleanupArtifactSnapshot preserves required deliverables independently of both
// the workspace and the mutable current-stage artifact publication directory.
type CleanupArtifactSnapshot struct {
	ResourceID string                `json:"resource_id"`
	Generation int64                 `json:"lifecycle_generation"`
	Directory  string                `json:"directory"`
	Files      []CleanupArtifactFile `json:"files"`
}

type CleanupArtifactFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

func CleanupArtifactDirectory(home, resourceID string, generation int64) string {
	return filepath.Join(home, "cleanup-artifacts", resourceID, strconv.FormatInt(generation, 10))
}

// ValidateCleanupArtifactSnapshot verifies every required file, not merely the
// presence of a collector success marker. No symlink below the durable home is
// accepted; otherwise a receipt could point back into a disposable workspace.
func ValidateCleanupArtifactSnapshot(home, resourceID string, generation int64, snapshot CleanupArtifactSnapshot) error {
	if snapshot.ResourceID != resourceID || snapshot.Generation != generation ||
		snapshot.Directory != CleanupArtifactDirectory(home, resourceID, generation) {
		return errors.New("artifact snapshot does not belong to this resource generation")
	}
	resolvedHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		return err
	}
	resolvedRoot, err := filepath.EvalSymlinks(snapshot.Directory)
	if err != nil {
		return err
	}
	if resolvedRoot != CleanupArtifactDirectory(resolvedHome, resourceID, generation) {
		return errors.New("artifact snapshot directory was substituted")
	}
	info, err := os.Stat(resolvedRoot)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("artifact snapshot is not a directory")
	}
	for _, entry := range snapshot.Files {
		if !filepath.IsLocal(entry.Path) || entry.Path == "." || entry.Size < 0 {
			return fmt.Errorf("invalid artifact snapshot entry %q", entry.Path)
		}
		path := filepath.Join(resolvedRoot, filepath.FromSlash(entry.Path))
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return err
		}
		if resolved != path {
			return fmt.Errorf("artifact snapshot entry %q was substituted", entry.Path)
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		info, statErr := file.Stat()
		if statErr != nil || !info.Mode().IsRegular() || info.Size() != entry.Size {
			_ = file.Close()
			return errors.Join(fmt.Errorf("artifact snapshot entry %q changed type or size", entry.Path), statErr)
		}
		digest := sha256.New()
		_, copyErr := io.Copy(digest, file)
		if err := errors.Join(copyErr, file.Close()); err != nil {
			return err
		}
		if hex.EncodeToString(digest.Sum(nil)) != entry.SHA256 {
			return fmt.Errorf("artifact snapshot entry %q changed content", entry.Path)
		}
	}
	return nil
}

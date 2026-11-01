package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Bounds for private review evidence (#2476). They keep one review's upload
// small and predictable: a reviewer gets the exact logs and screenshots the
// requester pinned, never a directory or an archive of unknown size.
const (
	ReviewEvidenceMaxFiles     = 32
	ReviewEvidenceMaxFileBytes = 8 << 20
	ReviewEvidenceMaxBytes     = 16 << 20
	reviewEvidenceManifestMax  = 1 << 20
)

// ReviewEvidenceFile is one hash-pinned private file a remote review receives.
// Path is read by the daemon only; the reviewer sees the bytes at SandboxName
// under the instance evidence directory, never a host path.
type ReviewEvidenceFile struct {
	Path        string `json:"path"`
	Bytes       int64  `json:"bytes"`
	SHA256      string `json:"sha256"`
	SandboxName string `json:"sandbox_name"`
}

// ReviewEvidence is the pinned evidence set of one review job.
type ReviewEvidence struct {
	ManifestPath   string               `json:"manifest_path"`
	ManifestSHA256 string               `json:"manifest_sha256"`
	HeadSHA        string               `json:"head_sha"`
	Files          []ReviewEvidenceFile `json:"files"`
}

type reviewEvidenceManifest struct {
	SchemaVersion int    `json:"schema_version"`
	HeadSHA       string `json:"head_sha"`
	Files         []struct {
		Path   string `json:"path"`
		Bytes  int64  `json:"bytes"`
		SHA256 string `json:"sha256"`
	} `json:"files"`
}

// LoadReviewEvidence reads a manifest, checks it against head and the bounds,
// and verifies every named file's size and sha256 NOW, so a request can never
// queue evidence that is already wrong. The result pins what was verified.
func LoadReviewEvidence(manifestPath, head string) (*ReviewEvidence, error) {
	manifestPath = strings.TrimSpace(manifestPath)
	if !filepath.IsAbs(manifestPath) {
		return nil, fmt.Errorf("evidence manifest %q must be an absolute path", manifestPath)
	}
	body, manifestSum, err := readPinnedRegularFile(manifestPath, reviewEvidenceManifestMax)
	if err != nil {
		return nil, fmt.Errorf("evidence manifest: %w", err)
	}
	var manifest reviewEvidenceManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return nil, fmt.Errorf("evidence manifest %s is not valid JSON: %w", manifestPath, err)
	}
	if manifest.SchemaVersion != 1 {
		return nil, fmt.Errorf("evidence manifest %s: schema_version %d, want 1", manifestPath, manifest.SchemaVersion)
	}
	head = strings.ToLower(strings.TrimSpace(head))
	if manifestHead := strings.ToLower(strings.TrimSpace(manifest.HeadSHA)); manifestHead == "" || manifestHead != head {
		return nil, fmt.Errorf("evidence manifest %s is for head %q, not the reviewed head %q", manifestPath, manifest.HeadSHA, head)
	}
	if len(manifest.Files) == 0 || len(manifest.Files) > ReviewEvidenceMaxFiles {
		return nil, fmt.Errorf("evidence manifest %s names %d files; allowed 1-%d", manifestPath, len(manifest.Files), ReviewEvidenceMaxFiles)
	}
	evidence := &ReviewEvidence{ManifestPath: manifestPath, ManifestSHA256: manifestSum, HeadSHA: head}
	var total int64
	seen := map[string]bool{}
	for index, entry := range manifest.Files {
		file := ReviewEvidenceFile{
			Path:   strings.TrimSpace(entry.Path),
			Bytes:  entry.Bytes,
			SHA256: strings.ToLower(strings.TrimSpace(entry.SHA256)),
		}
		if !filepath.IsAbs(file.Path) {
			return nil, fmt.Errorf("evidence file %d: path %q must be absolute", index+1, file.Path)
		}
		if seen[file.Path] {
			return nil, fmt.Errorf("evidence file %d: %s is listed twice", index+1, file.Path)
		}
		seen[file.Path] = true
		if file.Bytes <= 0 || file.Bytes > ReviewEvidenceMaxFileBytes {
			return nil, fmt.Errorf("evidence file %s: %d bytes; allowed 1-%d", file.Path, file.Bytes, ReviewEvidenceMaxFileBytes)
		}
		if total += file.Bytes; total > ReviewEvidenceMaxBytes {
			return nil, fmt.Errorf("evidence files total more than %d bytes", ReviewEvidenceMaxBytes)
		}
		if decoded, err := hex.DecodeString(file.SHA256); err != nil || len(decoded) != sha256.Size {
			return nil, fmt.Errorf("evidence file %s: sha256 %q is not 64 hex characters", file.Path, entry.SHA256)
		}
		if _, err := file.VerifiedBytes(); err != nil {
			return nil, err
		}
		file.SandboxName = reviewEvidenceSandboxName(index, file.Path)
		evidence.Files = append(evidence.Files, file)
	}
	return evidence, nil
}

// VerifiedBytes re-reads the file and returns its bytes only if size and sha256
// still match the pin. Drift is an error, never a silent substitution.
func (f ReviewEvidenceFile) VerifiedBytes() ([]byte, error) {
	body, sum, err := readPinnedRegularFile(f.Path, ReviewEvidenceMaxFileBytes)
	if err != nil {
		return nil, fmt.Errorf("evidence file %s: %w", f.Path, err)
	}
	if int64(len(body)) != f.Bytes || sum != f.SHA256 {
		return nil, fmt.Errorf("evidence file %s changed: now %d bytes sha256 %s, pinned %d bytes sha256 %s", f.Path, len(body), sum, f.Bytes, f.SHA256)
	}
	return body, nil
}

// readPinnedRegularFile reads a regular, non-symlink file of at most limit
// bytes and returns its content and lowercase hex sha256.
func readPinnedRegularFile(name string, limit int64) ([]byte, string, error) {
	info, err := os.Lstat(name)
	if err != nil {
		return nil, "", err
	}
	if !info.Mode().IsRegular() {
		return nil, "", errors.New("not a regular file (symlinks and directories are refused)")
	}
	file, err := os.Open(name)
	if err != nil {
		return nil, "", err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, "", err
	}
	if !os.SameFile(info, opened) {
		return nil, "", errors.New("file was replaced while it was being opened")
	}
	body, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(body)) > limit {
		return nil, "", fmt.Errorf("larger than %d bytes", limit)
	}
	sum := sha256.Sum256(body)
	return body, hex.EncodeToString(sum[:]), nil
}

// reviewEvidenceSandboxName is the file's name inside the instance: a
// two-digit position keeps same-named files from colliding, and the base name
// is reduced to a safe character set so it can never escape the directory.
func reviewEvidenceSandboxName(index int, hostPath string) string {
	base := path.Base(filepath.ToSlash(hostPath))
	var safe strings.Builder
	for _, r := range base {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			safe.WriteRune(r)
		default:
			safe.WriteByte('_')
		}
	}
	name := strings.TrimLeft(safe.String(), ".")
	if name == "" {
		name = "file"
	}
	return fmt.Sprintf("%02d-%s", index+1, name)
}

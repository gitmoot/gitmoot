// Package ompaddon embeds and installs the Gitmoot inbox add-on for OMP.
//
// The add-on is one TypeScript file that OMP loads from its user extensions
// directory. It registers each interactive OMP session under the Gitmoot OMP
// runtime registry (config.Paths.OMPRuntimeDir) and accepts inbox
// notifications from the daemon over a private Unix socket. Installing bakes
// the absolute registry directory into the file, so one installed copy always
// serves the Gitmoot home it was installed for.
package ompaddon

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

// FileName is the installed file name. OMP binds the extensions in one
// directory in byte order of their file names and runs input hooks in bind
// order; the "00-" prefix puts this add-on's input hook ahead of other user
// extensions so it sees an operator submission before slower hooks do.
const FileName = "00-gitmoot-inbox.ts"

// AgentDirEnv overrides OMP's agent directory, as in OMP itself.
const AgentDirEnv = "PI_CODING_AGENT_DIR"

const (
	versionPlaceholder     = `"__GITMOOT_OMP_ADDON_VERSION__"`
	registryDirPlaceholder = `"__GITMOOT_OMP_REGISTRY_DIR__"`
	// The registry dir plus "/<12 hex>.sock" must stay below the Unix socket
	// path limit the add-on enforces (100 bytes).
	maxRegistryDirBytes = 100 - len("/123456789012.sock") - 1
)

//go:embed gitmoot-inbox.ts
var source []byte

// Version identifies the add-on source this binary installs. It is derived
// from the source itself, so it changes exactly when the add-on changes and a
// rebuild of identical source never reports an install as outdated.
var Version = func() string {
	sum := sha256.Sum256(source)
	return hex.EncodeToString(sum[:6])
}()

// Status of an installed add-on file relative to what this binary installs.
type Status string

const (
	StatusInstalled Status = "installed"
	StatusOutdated  Status = "outdated"
	StatusMissing   Status = "missing"
)

// Inspection describes the add-on file at Path.
type Inspection struct {
	Path   string `json:"path"`
	Status Status `json:"status"`
	// Version and RegistryDir are what the installed file declares in its
	// header; empty when the file is missing or carries no header.
	Version     string `json:"version,omitempty"`
	RegistryDir string `json:"registry_dir,omitempty"`
	// Expected values for this binary and Gitmoot home.
	ExpectedVersion     string `json:"expected_version"`
	ExpectedRegistryDir string `json:"expected_registry_dir"`
}

// ExtensionsDir returns OMP's user extensions directory the way OMP resolves
// it without a named profile: $PI_CODING_AGENT_DIR/extensions when set (a
// relative value is taken from the working directory, with no ~ expansion),
// otherwise <userHome>/.omp/agent/extensions.
func ExtensionsDir(userHome string, getenv func(string) string) (string, error) {
	if dir := getenv(AgentDirEnv); dir != "" {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return "", err
		}
		return filepath.Join(abs, "extensions"), nil
	}
	return filepath.Join(userHome, ".omp", "agent", "extensions"), nil
}

// Render returns the add-on source with the registry directory baked in.
func Render(registryDir string) ([]byte, error) {
	if !filepath.IsAbs(registryDir) {
		return nil, fmt.Errorf("OMP registry directory %q is not absolute", registryDir)
	}
	if strings.ContainsFunc(registryDir, unicode.IsControl) {
		return nil, fmt.Errorf("OMP registry directory %q contains control characters", registryDir)
	}
	if len(registryDir) > maxRegistryDirBytes {
		return nil, fmt.Errorf("OMP registry directory %q is longer than %d bytes; Unix socket paths inside it would not fit", registryDir, maxRegistryDirBytes)
	}
	quotedDir, err := json.Marshal(filepath.Clean(registryDir))
	if err != nil {
		return nil, err
	}
	quotedVersion, err := json.Marshal(Version)
	if err != nil {
		return nil, err
	}
	out := bytes.ReplaceAll(source, []byte(registryDirPlaceholder), quotedDir)
	return bytes.ReplaceAll(out, []byte(versionPlaceholder), quotedVersion), nil
}

// Install writes the add-on into extensionsDir for registryDir. It leaves an
// identical file untouched and otherwise replaces the file atomically, so a
// starting OMP never loads a partial file. It reports whether it wrote.
func Install(extensionsDir, registryDir string) (string, bool, error) {
	want, err := Render(registryDir)
	if err != nil {
		return "", false, err
	}
	path := filepath.Join(extensionsDir, FileName)
	info, err := os.Lstat(path)
	switch {
	case err == nil && info.Mode().IsRegular():
		have, err := os.ReadFile(path)
		if err != nil {
			return path, false, err
		}
		if bytes.Equal(have, want) {
			if info.Mode().Perm() != 0o644 {
				return path, true, os.Chmod(path, 0o644)
			}
			return path, false, nil
		}
	case err == nil:
		return path, false, fmt.Errorf("%s exists and is not a regular file", path)
	case !errors.Is(err, fs.ErrNotExist):
		return path, false, err
	}
	if err := os.MkdirAll(extensionsDir, 0o755); err != nil {
		return path, false, err
	}
	if err := writeFileAtomic(path, want, 0o644); err != nil {
		return path, false, err
	}
	return path, true, nil
}

var (
	headerVersion     = regexp.MustCompile(`(?m)^// GITMOOT_OMP_ADDON_VERSION=("[^"\n]*")$`)
	headerRegistryDir = regexp.MustCompile(`(?m)^// GITMOOT_OMP_REGISTRY_DIR=("(?:[^"\\\n]|\\.)*")$`)
)

// Inspect compares the add-on file in extensionsDir with what Install would
// write for registryDir. Any difference, including a different baked registry
// directory or a hand edit, is StatusOutdated.
func Inspect(extensionsDir, registryDir string) (Inspection, error) {
	path := filepath.Join(extensionsDir, FileName)
	result := Inspection{Path: path, ExpectedVersion: Version, ExpectedRegistryDir: filepath.Clean(registryDir)}
	want, err := Render(registryDir)
	if err != nil {
		return result, err
	}
	have, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		result.Status = StatusMissing
		return result, nil
	}
	if err != nil {
		return result, err
	}
	result.Version = headerString(headerVersion, have)
	result.RegistryDir = headerString(headerRegistryDir, have)
	if bytes.Equal(have, want) {
		result.Status = StatusInstalled
	} else {
		result.Status = StatusOutdated
	}
	return result, nil
}

func headerString(pattern *regexp.Regexp, content []byte) string {
	match := pattern.FindSubmatch(content)
	if match == nil {
		return ""
	}
	var value string
	if json.Unmarshal(match[1], &value) != nil {
		return ""
	}
	return value
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename temp file: %w", err)
	}
	committed = true
	return nil
}

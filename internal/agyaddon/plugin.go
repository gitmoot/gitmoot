package agyaddon

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/gitmoot/gitmoot/internal/shellquote"
)

const (
	// PluginName is the directory name under ~/.gemini/config/plugins/.
	PluginName = "gitmoot"
	// ConfigDirEnv overrides ~/.gemini/config when set.
	ConfigDirEnv = "ANTIGRAVITY_CONFIG_DIR"

	pluginManifestFile = "plugin.json"
	hooksFile          = "hooks.json"
	metaFile           = "gitmoot-plugin.meta.json"
	hookTimeoutSeconds = 5
)

// Version identifies the plugin layout this binary installs. It changes when
// the hook layout changes, not on unrelated rebuilds.
var Version = func() string {
	sum := sha256.Sum256([]byte("gitmoot-agy-plugin-v1:SessionStart+PreInvocation:agy-hook"))
	return hex.EncodeToString(sum[:6])
}()

// Status describes an installed Antigravity plugin relative to what this binary installs.
type Status string

const (
	StatusInstalled Status = "installed"
	StatusOutdated  Status = "outdated"
	StatusMissing   Status = "missing"
)

// Inspection describes the plugin directory at Path.
type Inspection struct {
	Path                string `json:"path"`
	Status              Status `json:"status"`
	Version             string `json:"version,omitempty"`
	RegistryDir         string `json:"registry_dir,omitempty"`
	GitmootBin          string `json:"gitmoot_bin,omitempty"`
	ExpectedVersion     string `json:"expected_version"`
	ExpectedRegistryDir string `json:"expected_registry_dir"`
	ExpectedGitmootBin  string `json:"expected_gitmoot_bin"`
}

// RenderedPlugin contains the exact files written into the plugin directory.
type RenderedPlugin struct {
	Manifest []byte
	Hooks    []byte
	Meta     []byte
}

type pluginMeta struct {
	Version     string `json:"version"`
	RegistryDir string `json:"registryDir"`
	GitmootBin  string `json:"gitmootBin"`
}

// PluginDir returns Antigravity's native plugin directory for Gitmoot:
// $ANTIGRAVITY_CONFIG_DIR/plugins/gitmoot when set, otherwise
// <userHome>/.gemini/config/plugins/gitmoot.
func PluginDir(userHome string, getenv func(string) string) (string, error) {
	if getenv != nil {
		if dir := strings.TrimSpace(getenv(ConfigDirEnv)); dir != "" {
			abs, err := filepath.Abs(dir)
			if err != nil {
				return "", err
			}
			return filepath.Join(abs, "plugins", PluginName), nil
		}
	}
	return filepath.Join(userHome, ".gemini", "config", "plugins", PluginName), nil
}

// Render generates plugin.json, hooks.json, and gitmoot-plugin.meta.json for
// the given registry directory and gitmoot executable path.
func Render(registryDir, gitmootBin string) (RenderedPlugin, error) {
	registryDir = filepath.Clean(strings.TrimSpace(registryDir))
	if !filepath.IsAbs(registryDir) {
		return RenderedPlugin{}, fmt.Errorf("agy registry directory %q is not absolute", registryDir)
	}
	if strings.ContainsFunc(registryDir, unicode.IsControl) {
		return RenderedPlugin{}, fmt.Errorf("agy registry directory %q contains control characters", registryDir)
	}
	gitmootBin = strings.TrimSpace(gitmootBin)
	if gitmootBin == "" {
		gitmootBin = "gitmoot"
	}
	if strings.ContainsFunc(gitmootBin, unicode.IsControl) {
		return RenderedPlugin{}, fmt.Errorf("gitmoot binary path %q contains control characters", gitmootBin)
	}

	manifest, err := json.MarshalIndent(map[string]string{
		"name": PluginName,
	}, "", "  ")
	if err != nil {
		return RenderedPlugin{}, err
	}
	manifest = append(manifest, '\n')

	hookCmd := fmt.Sprintf("%s plugin agy-hook --registry-dir %s",
		shellquote.Posix(gitmootBin), shellquote.Posix(registryDir))
	handler := []map[string]any{
		{
			"type":    "command",
			"command": hookCmd,
			"timeout": hookTimeoutSeconds,
		},
	}
	hooks, err := json.MarshalIndent(map[string]any{
		"gitmoot-inbox": map[string]any{
			"SessionStart":  handler,
			"PreInvocation": handler,
		},
	}, "", "  ")
	if err != nil {
		return RenderedPlugin{}, err
	}
	hooks = append(hooks, '\n')

	meta, err := json.MarshalIndent(pluginMeta{
		Version:     Version,
		RegistryDir: registryDir,
		GitmootBin:  gitmootBin,
	}, "", "  ")
	if err != nil {
		return RenderedPlugin{}, err
	}
	meta = append(meta, '\n')

	return RenderedPlugin{Manifest: manifest, Hooks: hooks, Meta: meta}, nil
}

// Install writes the native Antigravity plugin into pluginDir atomically.
// Existing identical files are left untouched; it returns whether any file changed.
func Install(pluginDir, registryDir, gitmootBin string) (string, bool, error) {
	rendered, err := Render(registryDir, gitmootBin)
	if err != nil {
		return "", false, err
	}
	pluginDir = filepath.Clean(pluginDir)
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		return pluginDir, false, err
	}
	changed := false
	for _, file := range []struct {
		name string
		data []byte
	}{
		{pluginManifestFile, rendered.Manifest},
		{hooksFile, rendered.Hooks},
		{metaFile, rendered.Meta},
	} {
		target := filepath.Join(pluginDir, file.name)
		wrote, err := syncFile(target, file.data, 0o644)
		if err != nil {
			return pluginDir, changed, err
		}
		if wrote {
			changed = true
		}
	}
	return pluginDir, changed, nil
}

// Inspect checks whether pluginDir matches what Install would write.
func Inspect(pluginDir, registryDir, gitmootBin string) (Inspection, error) {
	pluginDir = filepath.Clean(pluginDir)
	registryDir = filepath.Clean(strings.TrimSpace(registryDir))
	if strings.TrimSpace(gitmootBin) == "" {
		gitmootBin = "gitmoot"
	}
	result := Inspection{
		Path:                pluginDir,
		ExpectedVersion:     Version,
		ExpectedRegistryDir: registryDir,
		ExpectedGitmootBin:  gitmootBin,
	}
	want, err := Render(registryDir, gitmootBin)
	if err != nil {
		return result, err
	}
	if _, err := os.Stat(pluginDir); errors.Is(err, fs.ErrNotExist) {
		result.Status = StatusMissing
		return result, nil
	} else if err != nil {
		return result, err
	}

	if rawMeta, err := os.ReadFile(filepath.Join(pluginDir, metaFile)); err == nil {
		var meta pluginMeta
		if json.Unmarshal(rawMeta, &meta) == nil {
			result.Version = meta.Version
			result.RegistryDir = meta.RegistryDir
			result.GitmootBin = meta.GitmootBin
		}
	}

	allMissing := true
	allMatch := true
	for _, file := range []struct {
		name string
		want []byte
	}{
		{pluginManifestFile, want.Manifest},
		{hooksFile, want.Hooks},
		{metaFile, want.Meta},
	} {
		have, err := os.ReadFile(filepath.Join(pluginDir, file.name))
		if errors.Is(err, fs.ErrNotExist) {
			allMatch = false
			continue
		}
		if err != nil {
			return result, err
		}
		allMissing = false
		if !bytes.Equal(have, file.want) {
			allMatch = false
		}
	}
	switch {
	case allMissing:
		result.Status = StatusMissing
	case allMatch:
		result.Status = StatusInstalled
	default:
		result.Status = StatusOutdated
	}
	return result, nil
}

func syncFile(path string, want []byte, perm os.FileMode) (bool, error) {
	info, err := os.Lstat(path)
	switch {
	case err == nil && info.Mode().IsRegular():
		have, err := os.ReadFile(path)
		if err != nil {
			return false, err
		}
		if bytes.Equal(have, want) {
			if info.Mode().Perm() != perm {
				return true, os.Chmod(path, perm)
			}
			return false, nil
		}
	case err == nil:
		return false, fmt.Errorf("%s exists and is not a regular file", path)
	case !errors.Is(err, fs.ErrNotExist):
		return false, err
	}
	return true, writeFileAtomic(path, want, perm)
}

package cli

import (
	"crypto/sha256"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/jerryfane/gitmoot/internal/runtime"
)

var runtimeAuthEnvVars = []string{
	runtime.ClaudeOAuthTokenEnv,
	runtime.AnthropicAPIKeyEnv,
	runtime.AnthropicAuthTokenEnv,
}

const (
	runtimeAuthFileName       = "runtime-auth.env"
	legacyRuntimeAuthFileName = "daemon-runtime.env"
	runtimeAuthMinValueBytes  = 16
	runtimeAuthMaxValueBytes  = 4096
)

const runtimeAuthFilePerm os.FileMode = 0o600

var (
	runtimeAuthBootstrapMu sync.Mutex
	runtimeAuthEnvLookup   = os.LookupEnv
	runtimeAuthLogf        = log.Printf
)

type runtimeAuthFile struct {
	Path    string
	Exists  bool
	Values  map[string]string
	Mode    os.FileMode
	ModTime time.Time
}

func runtimeAuthFilePath(homeDir string) string {
	return filepath.Join(homeDir, runtimeAuthFileName)
}

func legacyRuntimeAuthFilePath(homeDir string) string {
	return filepath.Join(homeDir, legacyRuntimeAuthFileName)
}

func collectRuntimeAuthEnv(lookup func(string) (string, bool)) map[string]string {
	values := map[string]string{}
	if lookup == nil {
		return values
	}
	for _, name := range runtimeAuthEnvVars {
		if value, ok := lookup(name); ok && strings.TrimSpace(value) != "" {
			values[name] = value
		}
	}
	return values
}

func loadRuntimeAuthFile(homeDir string) (runtimeAuthFile, error) {
	path := runtimeAuthFilePath(homeDir)
	state := runtimeAuthFile{Path: path, Values: map[string]string{}}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return state, nil
	}
	if err != nil {
		return state, fmt.Errorf("read %s: %w", path, err)
	}
	state.Exists = true
	state.Mode = info.Mode().Perm()
	state.ModTime = info.ModTime()
	if !info.Mode().IsRegular() {
		return state, fmt.Errorf("runtime auth file %s is not a regular file", path)
	}
	if state.Mode != runtimeAuthFilePerm {
		return state, fmt.Errorf("runtime auth file %s has permissions %04o; want 0600", path, state.Mode)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return state, fmt.Errorf("read runtime auth file %s: %w", path, err)
	}
	values, err := parseRuntimeAuthFile(path, data)
	if err != nil {
		return state, err
	}
	state.Values = values
	return state, nil
}

func parseRuntimeAuthFile(path string, data []byte) (map[string]string, error) {
	values := map[string]string{}
	for lineNumber, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSuffix(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		name = strings.TrimSpace(name)
		if !ok || !managedRuntimeAuthVar(name) {
			return nil, fmt.Errorf("runtime auth file %s line %d: expected NAME=VALUE for a managed Claude auth variable", path, lineNumber+1)
		}
		if _, duplicate := values[name]; duplicate {
			return nil, fmt.Errorf("runtime auth file %s line %d: duplicate %s", path, lineNumber+1, name)
		}
		if err := validateRuntimeAuthValue(value); err != nil {
			return nil, fmt.Errorf("runtime auth file %s line %d (%s): %w", path, lineNumber+1, name, err)
		}
		values[name] = value
	}
	return values, nil
}

func managedRuntimeAuthVar(name string) bool {
	for _, managed := range runtimeAuthEnvVars {
		if name == managed {
			return true
		}
	}
	return false
}

func validateRuntimeAuthValue(value string) error {
	switch {
	case value == "":
		return fmt.Errorf("value must be non-empty")
	case len(value) < runtimeAuthMinValueBytes:
		return fmt.Errorf("value is too short (minimum %d bytes)", runtimeAuthMinValueBytes)
	case len(value) > runtimeAuthMaxValueBytes:
		return fmt.Errorf("value is too long (maximum %d bytes)", runtimeAuthMaxValueBytes)
	case strings.IndexFunc(value, unicode.IsSpace) >= 0:
		return fmt.Errorf("value must not contain whitespace")
	case strings.ContainsRune(value, '\x00'):
		return fmt.Errorf("value must not contain NUL")
	default:
		return nil
	}
}

func writeRuntimeAuthFile(path string, values map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create runtime auth directory: %w", err)
	}
	var body strings.Builder
	for _, name := range runtimeAuthEnvVars {
		value, ok := values[name]
		if !ok {
			continue
		}
		if err := validateRuntimeAuthValue(value); err != nil {
			return fmt.Errorf("invalid %s: %w", name, err)
		}
		body.WriteString(name)
		body.WriteByte('=')
		body.WriteString(value)
		body.WriteByte('\n')
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".runtime-auth-*.tmp")
	if err != nil {
		return fmt.Errorf("create runtime auth temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	fail := func(err error) error {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(body.String()); err != nil {
		return fail(fmt.Errorf("write runtime auth temp file: %w", err))
	}
	if err := tmp.Chmod(runtimeAuthFilePerm); err != nil {
		return fail(fmt.Errorf("secure runtime auth temp file: %w", err))
	}
	if err := tmp.Sync(); err != nil {
		return fail(fmt.Errorf("sync runtime auth temp file: %w", err))
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close runtime auth temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace runtime auth file: %w", err)
	}
	if err := os.Chmod(path, runtimeAuthFilePerm); err != nil {
		return fmt.Errorf("secure runtime auth file: %w", err)
	}
	return nil
}

// bootstrapRuntimeAuth performs the one-release transition only when the new
// authoritative file is absent. Legacy persisted auth wins over ambient auth;
// an existing runtime-auth.env is never rewritten from either source.
func bootstrapRuntimeAuth(homeDir string, lookup func(string) (string, bool), logf func(string, ...any)) (bool, error) {
	runtimeAuthBootstrapMu.Lock()
	defer runtimeAuthBootstrapMu.Unlock()

	path := runtimeAuthFilePath(homeDir)
	if _, err := os.Stat(path); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, fmt.Errorf("inspect runtime auth file %s: %w", path, err)
	}

	legacy, exists, err := loadLegacyRuntimeAuthFile(legacyRuntimeAuthFilePath(homeDir))
	if err != nil {
		return false, err
	}
	if exists {
		if err := writeRuntimeAuthFile(path, legacy); err != nil {
			return false, err
		}
		if logf != nil {
			logf("imported Claude runtime auth from legacy %s into %s", legacyRuntimeAuthFileName, runtimeAuthFileName)
		}
		return true, nil
	}

	ambient := collectRuntimeAuthEnv(lookup)
	if len(ambient) == 0 {
		return false, nil
	}
	if err := writeRuntimeAuthFile(path, ambient); err != nil {
		return false, err
	}
	if logf != nil {
		logf("seeded %s from ambient Claude auth environment", runtimeAuthFileName)
	}
	return true, nil
}

func loadLegacyRuntimeAuthFile(path string) (map[string]string, bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("inspect legacy runtime auth file %s: %w", path, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != runtimeAuthFilePerm {
		return nil, true, fmt.Errorf("legacy runtime auth file %s must be a regular 0600 file", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, true, fmt.Errorf("read legacy runtime auth file %s: %w", path, err)
	}
	values := map[string]string{}
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		name = strings.TrimSpace(name)
		if !ok || !managedRuntimeAuthVar(name) || strings.TrimSpace(value) == "" {
			continue
		}
		if err := validateRuntimeAuthValue(value); err != nil {
			return nil, true, fmt.Errorf("legacy runtime auth file %s (%s): %w", path, name, err)
		}
		values[name] = value
	}
	return values, true, nil
}

// runtimeAuthInjectionEnv implements the blank-out rule. Once the authoritative
// file selects at least one managed variable, all three names are appended; any
// absent variable is explicitly empty so ambient Claude auth cannot outrank it.
func runtimeAuthInjectionEnv(state runtimeAuthFile) []string {
	if !state.Exists || len(state.Values) == 0 {
		return nil
	}
	env := make([]string, 0, len(runtimeAuthEnvVars))
	for _, name := range runtimeAuthEnvVars {
		env = append(env, name+"="+state.Values[name])
	}
	return env
}

func runtimeAuthSource(state runtimeAuthFile, lookup func(string) (string, bool)) string {
	if state.Exists && len(state.Values) > 0 {
		return runtimeAuthFileName
	}
	ambient := collectRuntimeAuthEnv(lookup)
	if len(ambient) > 0 {
		if state.Exists {
			return "ambient environment (runtime-auth.env explicitly empty)"
		}
		return "ambient environment"
	}
	if state.Exists {
		return "Claude credential store (runtime-auth.env explicitly empty)"
	}
	return "Claude credential store"
}

func runtimeAuthEffectiveLookup(state runtimeAuthFile, ambient func(string) (string, bool)) func(string) (string, bool) {
	if state.Exists && len(state.Values) > 0 {
		return func(name string) (string, bool) {
			value, ok := state.Values[name]
			return value, ok
		}
	}
	if ambient == nil {
		return func(string) (string, bool) { return "", false }
	}
	return ambient
}

func warnRuntimeAuthConflicts(state runtimeAuthFile, lookup func(string) (string, bool), logf func(string, ...any)) {
	if !state.Exists || len(state.Values) == 0 || lookup == nil || logf == nil {
		return
	}
	var conflicts []string
	for _, name := range runtimeAuthEnvVars {
		fileValue, fileOK := state.Values[name]
		ambientValue, ambientOK := lookup(name)
		if fileOK && ambientOK && strings.TrimSpace(ambientValue) != "" && fileValue != ambientValue {
			conflicts = append(conflicts, fmt.Sprintf("%s file=%s ambient=%s", name, maskedAuthFingerprint(fileValue), maskedAuthFingerprint(ambientValue)))
		}
	}
	if len(conflicts) > 0 {
		logf("WARNING: %s wins over conflicting ambient Claude auth: %s", runtimeAuthFileName, strings.Join(conflicts, "; "))
	}
}

func maskedAuthFingerprint(value string) string {
	runes := []rune(value)
	if len(runes) > 12 {
		return string(runes[:8]) + "..." + string(runes[len(runes)-4:])
	}
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("short(len=%d,sha256=%x)", len(runes), sum[:4])
}

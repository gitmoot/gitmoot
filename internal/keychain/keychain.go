// Package keychain resolves, validates and reads the operator keychain file
// (keychain.env by default, [credentials].keychain_path when configured).
//
// The file holds credential VALUES; the store's keychain_keys registry holds
// only names and delivery metadata. Every reader goes through one safety gate
// (absolute path, regular file, mode 0600, owned by the operator, outside the
// Gitmoot home and every managed checkout) whose check order is fixed so an
// unsafe file is never read or parsed after an earlier failure.
package keychain

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
)

const fileMode os.FileMode = 0o600

// File statuses reported by Classify. Values are stable: the dashboard Config
// page renders them.
const (
	StatusNone        = "none"
	StatusOK          = "ok"
	StatusMissing     = "missing"
	StatusBadMode     = "bad_mode"
	StatusBadOwner    = "bad_owner"
	StatusBadLocation = "bad_location"
	StatusInvalid     = "invalid"
)

var namePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidateName reports whether name is a valid environment-style key name.
func ValidateName(name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("must match [A-Za-z_][A-Za-z0-9_]*")
	}
	return nil
}

// ReservedName reports whether name sits in the GITMOOT_* namespace, which
// gitmoot reserves for its own environment.
func ReservedName(name string) bool {
	return strings.HasPrefix(name, "GITMOOT_")
}

// Inspection is the names-only, value-free result of Classify.
type Inspection struct {
	Path   string
	Status string
	Names  map[string]struct{}
}

// ResolvePath returns the keychain file path for the store's home: the
// configured [credentials].keychain_path, or keychain.env under the operator's
// ~/.config/gitmoot.
func ResolvePath(store *db.Store, home string) (string, error) {
	paths, err := pathsForStore(store, home)
	if err != nil {
		return "", err
	}
	cfg, err := config.LoadCredentialsConfig(paths)
	if err != nil {
		return "", fmt.Errorf("load credentials config: %w", err)
	}
	if cfg.KeychainPath != "" {
		// No absolute check here on purpose: config.LoadCredentialsConfig above
		// already refuses a relative [credentials].keychain_path, and a second
		// check would be unreachable code that looks like a guard. Measured, not
		// assumed - disabling a duplicate check here changed no test outcome,
		// which is exactly how dead safety code hides.
		return cfg.KeychainPath, nil
	}
	// The DEFAULT path is derived from paths.Home, which is not operator-checked.
	// A relative home resolves keychain.env inside the working directory, which
	// is the class that published a token in #1810.
	baseHome := filepath.Dir(paths.Home)
	if !filepath.IsAbs(baseHome) {
		return "", fmt.Errorf("refusing to resolve keychain.env under relative path %q: a credential must never resolve inside the working directory", baseHome)
	}
	return filepath.Join(baseHome, ".config", "gitmoot", "keychain.env"), nil
}

// LoadValidated resolves the keychain path, runs the safety gate and returns
// the parsed values.
func LoadValidated(ctx context.Context, store *db.Store, home string) (string, map[string]string, error) {
	path, err := ResolvePath(store, home)
	if err != nil {
		return "", nil, err
	}
	file, _, err := openValidated(ctx, store, home, path)
	if err != nil {
		return "", nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		return "", nil, fmt.Errorf("read keychain %s: %w", path, err)
	}
	values, err := parse(path, data)
	if err != nil {
		return "", nil, err
	}
	for name := range values {
		if ReservedName(name) {
			return "", nil, fmt.Errorf("keychain %s key %q uses reserved GITMOOT_* namespace", path, name)
		}
	}
	return path, values, nil
}

// Classify returns the live advisory status and names of the keychain file at
// path. It never returns file contents or an error string.
func Classify(ctx context.Context, store *db.Store, home, path string) Inspection {
	path = strings.TrimSpace(path)
	inspection := Inspection{Path: path, Status: StatusNone, Names: map[string]struct{}{}}
	if path == "" {
		return inspection
	}
	file, status, err := openValidated(ctx, store, home, path)
	if err != nil {
		inspection.Status = status
		return inspection
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		inspection.Status = StatusInvalid
		return inspection
	}
	names, err := parseNames(path, data)
	if err != nil {
		inspection.Status = StatusInvalid
		return inspection
	}
	for name := range names {
		if ReservedName(name) {
			inspection.Status = StatusInvalid
			return inspection
		}
	}
	inspection.Status = StatusOK
	inspection.Names = names
	return inspection
}

// openValidated is the shared safety gate for value loading and value-free
// status inspection. Its check order is fixed so unsafe files are never read
// or parsed after an earlier failure.
func openValidated(ctx context.Context, store *db.Store, home, declared string) (*os.File, string, error) {
	declared = strings.TrimSpace(declared)
	if !filepath.IsAbs(declared) {
		return nil, StatusInvalid, fmt.Errorf("keychain %q must be absolute", declared)
	}
	file, err := os.Open(declared)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, StatusMissing, fmt.Errorf("keychain %s does not exist", declared)
		}
		return nil, StatusInvalid, fmt.Errorf("open keychain %s: %w", declared, err)
	}
	closeWith := func(status string, err error) (*os.File, string, error) {
		file.Close()
		return nil, status, err
	}
	info, err := file.Stat()
	if err != nil {
		return closeWith(StatusInvalid, fmt.Errorf("stat keychain %s: %w", declared, err))
	}
	if !info.Mode().IsRegular() {
		return closeWith(StatusInvalid, fmt.Errorf("keychain %s is not a regular file", declared))
	}
	if info.Mode().Perm() != fileMode {
		return closeWith(StatusBadMode, fmt.Errorf("keychain %s has mode %04o; want 0600", declared, info.Mode().Perm()))
	}
	owner, err := ownerUID(info)
	if err != nil {
		return closeWith(StatusInvalid, fmt.Errorf("keychain %s: %w", declared, err))
	}
	if owner != CurrentUID() {
		return closeWith(StatusBadOwner, fmt.Errorf("keychain %s is owned by uid %d; want operator uid %d", declared, owner, CurrentUID()))
	}
	if err := validateLocation(ctx, store, home, declared); err != nil {
		return closeWith(StatusBadLocation, err)
	}
	return file, StatusOK, nil
}

func validateLocation(ctx context.Context, store *db.Store, home, declared string) error {
	if store == nil {
		return errors.New("keychain validation requires a store")
	}
	resolved, err := resolveSafetyPath(declared)
	if err != nil {
		return fmt.Errorf("resolve keychain %s: %w", declared, err)
	}
	gitmootHome := ""
	if databasePath := strings.TrimSpace(store.DatabasePath()); databasePath != "" && databasePath != ":memory:" {
		gitmootHome = filepath.Dir(databasePath)
	} else {
		paths, err := pathsFromFlag(home)
		if err != nil {
			return err
		}
		gitmootHome = paths.Home
	}
	protected := []struct{ label, path string }{{"Gitmoot home", gitmootHome}}
	repos, err := store.ListRepos(ctx)
	if err != nil {
		return err
	}
	for _, repo := range repos {
		label := "managed checkout " + repo.Owner + "/" + repo.Name
		for _, checkout := range []string{repo.CheckoutPath, repo.PrimaryCheckoutPath} {
			if strings.TrimSpace(checkout) != "" {
				protected = append(protected, struct{ label, path string }{label, checkout})
			}
		}
	}
	for _, item := range protected {
		protectedPath, err := resolveSafetyPath(item.path)
		if err != nil {
			return fmt.Errorf("resolve %s %q: %w", item.label, item.path, err)
		}
		if pathWithin(resolved, protectedPath) {
			return fmt.Errorf("keychain %s resolves inside %s %q", declared, item.label, protectedPath)
		}
	}
	return nil
}

// resolveSafetyPath resolves symlinks on the longest existing prefix of an
// absolute path, so a not-yet-created file still compares by its real parent.
func resolveSafetyPath(path string) (string, error) {
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("path must be absolute")
	}
	probe := path
	var suffix []string
	for {
		resolved, err := filepath.EvalSymlinks(probe)
		if err == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return "", err
		}
		suffix = append(suffix, filepath.Base(probe))
		probe = parent
	}
}

func pathWithin(path, directory string) bool {
	rel, err := filepath.Rel(directory, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func pathsForStore(store *db.Store, home string) (config.Paths, error) {
	if strings.TrimSpace(home) != "" {
		return pathsFromFlag(home)
	}
	if store != nil {
		databasePath := strings.TrimSpace(store.DatabasePath())
		if databasePath != "" && databasePath != ":memory:" {
			gitmootHome := filepath.Dir(databasePath)
			return config.PathsForHome(filepath.Dir(gitmootHome)), nil
		}
	}
	return pathsFromFlag(home)
}

func pathsFromFlag(home string) (config.Paths, error) {
	if home != "" {
		return config.PathsForHome(home), nil
	}
	return config.DefaultPaths()
}

// parse reads blank lines, comments, optional "export ", and one KEY=VALUE
// assignment per line. Values are never included in errors.
func parse(filePath string, data []byte) (map[string]string, error) {
	values := make(map[string]string)
	for i, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}
		name, value, ok := strings.Cut(line, "=")
		name = strings.TrimSpace(name)
		if !ok {
			return nil, fmt.Errorf("env file %s line %d: expected KEY=VALUE", filePath, i+1)
		}
		if err := ValidateName(name); err != nil {
			return nil, fmt.Errorf("env file %s line %d key %q: %w", filePath, i+1, name, err)
		}
		if _, duplicate := values[name]; duplicate {
			return nil, fmt.Errorf("env file %s line %d: duplicate key %q", filePath, i+1, name)
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && ((value[0] == '\'' && value[len(value)-1] == '\'') || (value[0] == '"' && value[len(value)-1] == '"')) {
			value = value[1 : len(value)-1]
		}
		if strings.ContainsRune(value, '\x00') {
			return nil, fmt.Errorf("env file %s line %d key %q contains NUL", filePath, i+1, name)
		}
		values[name] = value
	}
	return values, nil
}

// parseNames validates an env file and returns only its names. The right-hand
// side of each assignment is inspected only for structural validity and is
// never retained in the result.
func parseNames(filePath string, data []byte) (map[string]struct{}, error) {
	names := make(map[string]struct{})
	for i, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}
		equals := strings.IndexByte(line, '=')
		if equals < 0 {
			return nil, fmt.Errorf("env file %s line %d: expected KEY=VALUE", filePath, i+1)
		}
		name := strings.TrimSpace(line[:equals])
		if err := ValidateName(name); err != nil {
			return nil, fmt.Errorf("env file %s line %d key %q: %w", filePath, i+1, name, err)
		}
		if _, duplicate := names[name]; duplicate {
			return nil, fmt.Errorf("env file %s line %d: duplicate key %q", filePath, i+1, name)
		}
		if strings.ContainsRune(line[equals+1:], '\x00') {
			return nil, fmt.Errorf("env file %s line %d key %q contains NUL", filePath, i+1, name)
		}
		names[name] = struct{}{}
	}
	return names, nil
}

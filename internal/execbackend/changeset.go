package execbackend

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

const (
	ChangeSetVersion         = 1
	MaxChangeSetEntries      = 4096
	MaxChangeSetPatchBytes   = 16 << 20
	MaxChangeSetContentBytes = 64 << 20
	MaxChangeSetFileBytes    = 16 << 20
	changeSetRegularMode     = 0o100644
	changeSetExecutableMode  = 0o100755
	changeSetSymlinkMode     = 0o120000
	changeSetDirectoryMode   = 0o040000
)

type ChangeKind string

const (
	ChangeWrite  ChangeKind = "write"
	ChangeDelete ChangeKind = "delete"
)

// ChangeSet is the bounded, transport-safe description of one sandbox tree's
// uncommitted changes relative to BaseHEAD. Patch contains only tracked changes;
// Manifest authenticates every changed path and carries untracked content.
type ChangeSet struct {
	Version  int                   `json:"version"`
	BaseHEAD string                `json:"base_head"`
	Patch    []byte                `json:"patch,omitempty"`
	Manifest []ChangeManifestEntry `json:"manifest"`
}

type ChangeManifestEntry struct {
	// Path carries raw Git filename bytes in a Go string. Its JSON codec uses
	// []byte encoding so non-UTF-8 Unix filenames survive transport unchanged.
	Path    string     `json:"-"`
	Kind    ChangeKind `json:"kind"`
	Tracked bool       `json:"tracked"`
	Mode    uint32     `json:"mode,omitempty"`
	Size    int64      `json:"size,omitempty"`
	SHA256  string     `json:"sha256,omitempty"`
	Content []byte     `json:"content,omitempty"`
}

type changeManifestEntryJSON struct {
	Path    []byte     `json:"path"`
	Kind    ChangeKind `json:"kind"`
	Tracked bool       `json:"tracked"`
	Mode    uint32     `json:"mode,omitempty"`
	Size    int64      `json:"size,omitempty"`
	SHA256  string     `json:"sha256,omitempty"`
	Content []byte     `json:"content,omitempty"`
}

func (entry ChangeManifestEntry) MarshalJSON() ([]byte, error) {
	return json.Marshal(changeManifestEntryJSON{
		Path:    []byte(entry.Path),
		Kind:    entry.Kind,
		Tracked: entry.Tracked,
		Mode:    entry.Mode,
		Size:    entry.Size,
		SHA256:  entry.SHA256,
		Content: entry.Content,
	})
}

func (entry *ChangeManifestEntry) UnmarshalJSON(data []byte) error {
	var wire changeManifestEntryJSON
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*entry = ChangeManifestEntry{
		Path:    string(wire.Path),
		Kind:    wire.Kind,
		Tracked: wire.Tracked,
		Mode:    wire.Mode,
		Size:    wire.Size,
		SHA256:  wire.SHA256,
		Content: wire.Content,
	}
	return nil
}

// BuildChangeSet captures uncommitted sandbox changes. expectedBase is supplied
// by the host-side provision record; requiring sandbox HEAD to remain there is
// the v1 prohibition on sandbox-created commits.
func BuildChangeSet(ctx context.Context, sandbox, expectedBase string) (ChangeSet, error) {
	expectedBase = strings.TrimSpace(expectedBase)
	if expectedBase == "" {
		return ChangeSet{}, errors.New("changeset expected base HEAD is required")
	}
	head, err := gitOutput(ctx, sandbox, "rev-parse", "HEAD")
	if err != nil {
		return ChangeSet{}, fmt.Errorf("read sandbox HEAD: %w", err)
	}
	head = strings.TrimSpace(head)
	if head != expectedBase {
		return ChangeSet{}, fmt.Errorf("sandbox-created commits are forbidden: sandbox HEAD %s, expected base %s", head, expectedBase)
	}
	patch, err := gitBytes(ctx, sandbox, "diff", "--binary", "--full-index", "--no-ext-diff", "--no-renames", "HEAD", "--")
	if err != nil {
		return ChangeSet{}, fmt.Errorf("capture tracked patch: %w", err)
	}
	if len(patch) > MaxChangeSetPatchBytes {
		return ChangeSet{}, fmt.Errorf("changeset tracked patch is %d bytes (limit %d)", len(patch), MaxChangeSetPatchBytes)
	}
	trackedRaw, err := gitBytes(ctx, sandbox, "diff", "--name-only", "--no-renames", "-z", "HEAD", "--")
	if err != nil {
		return ChangeSet{}, fmt.Errorf("list tracked changes: %w", err)
	}
	untrackedRaw, err := gitBytes(ctx, sandbox, "ls-files", "--others", "--exclude-standard", "-z", "--")
	if err != nil {
		return ChangeSet{}, fmt.Errorf("list untracked changes: %w", err)
	}
	deletedRaw, err := gitBytes(ctx, sandbox, "diff", "--diff-filter=D", "--name-only", "--no-renames", "-z", "HEAD", "--")
	if err != nil {
		return ChangeSet{}, fmt.Errorf("list tracked deletions: %w", err)
	}

	tracked := nulPaths(trackedRaw)
	untracked := nulPaths(untrackedRaw)
	deleted := make(map[string]struct{})
	for _, path := range nulPaths(deletedRaw) {
		deleted[path] = struct{}{}
	}
	if len(tracked)+len(untracked) > MaxChangeSetEntries {
		return ChangeSet{}, fmt.Errorf("changeset has %d paths (limit %d)", len(tracked)+len(untracked), MaxChangeSetEntries)
	}
	manifest := make([]ChangeManifestEntry, 0, len(tracked)+len(untracked))
	seen := make(map[string]struct{}, len(tracked)+len(untracked))
	var contentBytes int64
	for _, item := range []struct {
		paths   []string
		tracked bool
	}{{tracked, true}, {untracked, false}} {
		for _, path := range item.paths {
			if _, ok := seen[path]; ok {
				continue
			}
			seen[path] = struct{}{}
			if err := validateChangePath(path); err != nil {
				return ChangeSet{}, err
			}
			_, trackedDeletion := deleted[path]
			entry, err := manifestEntry(sandbox, path, item.tracked, item.tracked && trackedDeletion)
			if err != nil {
				return ChangeSet{}, err
			}
			contentBytes += int64(len(entry.Content))
			if contentBytes > MaxChangeSetContentBytes {
				return ChangeSet{}, fmt.Errorf("changeset manifest content is %d bytes (limit %d)", contentBytes, MaxChangeSetContentBytes)
			}
			manifest = append(manifest, entry)
		}
	}
	sort.Slice(manifest, func(i, j int) bool { return manifest[i].Path < manifest[j].Path })
	return ChangeSet{Version: ChangeSetVersion, BaseHEAD: expectedBase, Patch: patch, Manifest: manifest}, nil
}

func manifestEntry(root, path string, tracked, trackedDeletion bool) (ChangeManifestEntry, error) {
	entry := ChangeManifestEntry{Path: path, Tracked: tracked}
	if trackedDeletion {
		entry.Kind = ChangeDelete
		return entry, nil
	}
	info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(path)))
	if errors.Is(err, fs.ErrNotExist) {
		if !tracked {
			return entry, fmt.Errorf("changeset path %q disappeared while collecting", path)
		}
		entry.Kind = ChangeDelete
		return entry, nil
	}
	if err != nil {
		return entry, fmt.Errorf("inspect changeset path %q: %w", path, err)
	}
	entry.Kind = ChangeWrite
	content, mode, err := readNode(root, path, info)
	if err != nil {
		return entry, err
	}
	if len(content) > MaxChangeSetFileBytes {
		return entry, fmt.Errorf("changeset path %q is %d bytes (limit %d)", path, len(content), MaxChangeSetFileBytes)
	}
	entry.Mode = mode
	entry.Size = int64(len(content))
	sum := sha256.Sum256(content)
	entry.SHA256 = hex.EncodeToString(sum[:])
	if !tracked {
		entry.Content = content
	}
	return entry, nil
}

func readNode(root, path string, info fs.FileInfo) ([]byte, uint32, error) {
	full := filepath.Join(root, filepath.FromSlash(path))
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(full)
		if err != nil {
			return nil, 0, fmt.Errorf("read changeset symlink %q: %w", path, err)
		}
		if err := validateSymlinkTarget(path, target); err != nil {
			return nil, 0, err
		}
		return []byte(target), changeSetSymlinkMode, nil
	}
	if !info.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("changeset path %q has unsupported file type %s", path, info.Mode().Type())
	}
	if info.Size() > MaxChangeSetFileBytes {
		return nil, 0, fmt.Errorf("changeset path %q is %d bytes (limit %d)", path, info.Size(), MaxChangeSetFileBytes)
	}
	content, err := os.ReadFile(full)
	if err != nil {
		return nil, 0, fmt.Errorf("read changeset path %q: %w", path, err)
	}
	mode := uint32(changeSetRegularMode)
	if info.Mode().Perm()&0o111 != 0 {
		mode = changeSetExecutableMode
	}
	return content, mode, nil
}

// ImportChangeSet applies a verified ChangeSet to worktree. It is idempotent:
// an already-materialized target is accepted without applying the patch again.
func ImportChangeSet(ctx context.Context, worktree string, changes ChangeSet) error {
	return (changeSetImporter{}).importChangeSet(ctx, worktree, changes)
}

type changeSetImporter struct {
	afterMaterialize func(path string, completed int) error
	beforeLock       func() error
}

type nodeState struct {
	exists  bool
	mode    uint32
	size    int64
	sha256  string
	content []byte
}

func (i changeSetImporter) importChangeSet(ctx context.Context, worktree string, changes ChangeSet) (retErr error) {
	if err := validateChangeSet(changes); err != nil {
		return err
	}
	initialHEAD, err := gitOutput(ctx, worktree, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("read host HEAD: %w", err)
	}
	initialHEAD = strings.TrimSpace(initialHEAD)
	if initialHEAD != changes.BaseHEAD {
		return fmt.Errorf("refuse changeset import: host HEAD %s differs from base %s", initialHEAD, changes.BaseHEAD)
	}

	stage, err := createStagingWorktree(ctx, worktree, changes.BaseHEAD)
	if err != nil {
		return err
	}
	defer func() {
		cleanupCtx := context.WithoutCancel(ctx)
		if cleanupErr := removeStagingWorktree(cleanupCtx, worktree, stage); retErr == nil && cleanupErr != nil {
			retErr = cleanupErr
		}
	}()

	baseStates := make(map[string]nodeState, len(changes.Manifest))
	for _, entry := range changes.Manifest {
		state, err := inspectNode(stage, entry.Path)
		if err != nil {
			return err
		}
		baseStates[entry.Path] = state
	}
	if len(changes.Patch) > 0 {
		cmd := exec.CommandContext(ctx, "git", "-C", stage, "apply", "--binary", "--whitespace=nowarn", "-")
		cmd.Stdin = bytes.NewReader(changes.Patch)
		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("apply tracked patch in staging: %w: %s", err, strings.TrimSpace(string(output)))
		}
	}
	for _, entry := range changes.Manifest {
		if entry.Tracked || entry.Kind == ChangeDelete {
			continue
		}
		if err := writeEntry(stage, entry); err != nil {
			return fmt.Errorf("stage changeset path %q: %w", entry.Path, err)
		}
	}
	if err := verifyStaging(ctx, stage, changes); err != nil {
		return err
	}

	desiredStates := make(map[string]nodeState, len(changes.Manifest))
	for _, entry := range changes.Manifest {
		desired, err := inspectNode(stage, entry.Path)
		if err != nil {
			return err
		}
		desiredStates[entry.Path] = desired
	}

	if i.beforeLock != nil {
		if err := i.beforeLock(); err != nil {
			return fmt.Errorf("before changeset host lock: %w", err)
		}
	}
	unlock, err := lockHostHEAD(worktree)
	if err != nil {
		return err
	}
	defer func() {
		if unlockErr := unlock(); retErr == nil && unlockErr != nil {
			retErr = unlockErr
		}
	}()
	lockedHEAD, err := gitOutput(ctx, worktree, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("recheck locked host HEAD: %w", err)
	}
	if strings.TrimSpace(lockedHEAD) != initialHEAD {
		return fmt.Errorf("refuse changeset import: host HEAD moved from %s to %s", initialHEAD, strings.TrimSpace(lockedHEAD))
	}
	alreadyApplied := true
	atBase := true
	currentStates := make(map[string]nodeState, len(changes.Manifest))
	for _, entry := range changes.Manifest {
		current, err := inspectNode(worktree, entry.Path)
		if err != nil {
			return err
		}
		currentStates[entry.Path] = current
		if !sameNode(current, desiredStates[entry.Path]) {
			alreadyApplied = false
		}
		if !sameNode(current, baseStates[entry.Path]) {
			atBase = false
		}
	}
	structuralMatch, err := structuralDirectoriesEqual(stage, worktree, changes.Manifest, desiredStates)
	if err != nil {
		return err
	}
	if !structuralMatch {
		alreadyApplied = false
	}
	if alreadyApplied {
		return nil
	}
	if !atBase {
		for _, entry := range changes.Manifest {
			current := currentStates[entry.Path]
			if !sameNode(current, baseStates[entry.Path]) && !sameNode(current, desiredStates[entry.Path]) {
				return fmt.Errorf("refuse changeset import: host path %q changed from base", entry.Path)
			}
		}
		return errors.New("refuse changeset import: host worktree contains a partial or different materialization")
	}

	backup, err := snapshotPaths(worktree, changes.Manifest)
	if err != nil {
		return err
	}
	createdDirs := make(map[string]struct{})
	mutatedPaths := make(map[string]struct{})
	rollback := func(cause error) error {
		if err := restorePaths(worktree, changes.Manifest, backup, createdDirs, mutatedPaths); err != nil {
			return fmt.Errorf("%v; rollback failed: %w", cause, err)
		}
		return cause
	}
	for index, entry := range materializationOrder(changes.Manifest) {
		if err := ctx.Err(); err != nil {
			return rollback(fmt.Errorf("changeset import interrupted before path %q: %w", entry.Path, err))
		}
		// Mark the path before mutation begins: materializeEntry may fail after
		// removing or replacing the node, so rollback must own that attempt.
		mutatedPaths[entry.Path] = struct{}{}
		if err := materializeEntry(worktree, stage, entry, createdDirs); err != nil {
			return rollback(fmt.Errorf("materialize changeset path %q: %w", entry.Path, err))
		}
		if i.afterMaterialize != nil {
			if err := i.afterMaterialize(entry.Path, index+1); err != nil {
				return rollback(fmt.Errorf("changeset import interrupted after path %q: %w", entry.Path, err))
			}
		}
	}
	materialized, err := materializationMatches(worktree, stage, changes.Manifest, desiredStates)
	if err != nil {
		return rollback(fmt.Errorf("verify materialized changeset: %w", err))
	}
	if !materialized {
		return rollback(errors.New("verify materialized changeset: host tree differs from authenticated staging tree"))
	}
	finalHEAD, err := gitOutput(ctx, worktree, "rev-parse", "HEAD")
	if err != nil {
		return rollback(fmt.Errorf("final host HEAD check: %w", err))
	}
	if strings.TrimSpace(finalHEAD) != initialHEAD {
		return rollback(fmt.Errorf("refuse changeset import: host HEAD moved from %s to %s", initialHEAD, strings.TrimSpace(finalHEAD)))
	}
	return nil
}

func validateChangeSet(changes ChangeSet) error {
	if changes.Version != ChangeSetVersion {
		return fmt.Errorf("unsupported changeset version %d (want %d)", changes.Version, ChangeSetVersion)
	}
	if strings.TrimSpace(changes.BaseHEAD) == "" {
		return errors.New("changeset base HEAD is required")
	}
	if len(changes.Patch) > MaxChangeSetPatchBytes {
		return fmt.Errorf("changeset tracked patch is %d bytes (limit %d)", len(changes.Patch), MaxChangeSetPatchBytes)
	}
	if len(changes.Manifest) > MaxChangeSetEntries {
		return fmt.Errorf("changeset has %d paths (limit %d)", len(changes.Manifest), MaxChangeSetEntries)
	}
	seen := make(map[string]struct{}, len(changes.Manifest))
	var contentBytes int64
	for _, entry := range changes.Manifest {
		if err := validateChangePath(entry.Path); err != nil {
			return err
		}
		if _, ok := seen[entry.Path]; ok {
			return fmt.Errorf("changeset path %q appears more than once", entry.Path)
		}
		seen[entry.Path] = struct{}{}
		switch entry.Kind {
		case ChangeDelete:
			if entry.Mode != 0 || entry.Size != 0 || entry.SHA256 != "" || len(entry.Content) != 0 {
				return fmt.Errorf("changeset deletion %q carries file content or metadata", entry.Path)
			}
		case ChangeWrite:
			if entry.Mode != changeSetRegularMode && entry.Mode != changeSetExecutableMode && entry.Mode != changeSetSymlinkMode {
				return fmt.Errorf("changeset path %q has unsupported mode %o", entry.Path, entry.Mode)
			}
			if entry.Size < 0 || entry.Size > MaxChangeSetFileBytes {
				return fmt.Errorf("changeset path %q size %d exceeds limit %d", entry.Path, entry.Size, MaxChangeSetFileBytes)
			}
			if len(entry.SHA256) != sha256.Size*2 {
				return fmt.Errorf("changeset path %q has invalid SHA-256 %q", entry.Path, entry.SHA256)
			}
			if !entry.Tracked {
				if int64(len(entry.Content)) != entry.Size {
					return fmt.Errorf("changeset path %q content size %d differs from manifest size %d", entry.Path, len(entry.Content), entry.Size)
				}
				if entry.Mode == changeSetSymlinkMode {
					if err := validateSymlinkTarget(entry.Path, string(entry.Content)); err != nil {
						return err
					}
				}
				sum := sha256.Sum256(entry.Content)
				if hex.EncodeToString(sum[:]) != entry.SHA256 {
					return fmt.Errorf("changeset path %q content hash differs from manifest", entry.Path)
				}
			}
		default:
			return fmt.Errorf("changeset path %q has invalid kind %q", entry.Path, entry.Kind)
		}
		contentBytes += int64(len(entry.Content))
		if contentBytes > MaxChangeSetContentBytes {
			return fmt.Errorf("changeset manifest content is %d bytes (limit %d)", contentBytes, MaxChangeSetContentBytes)
		}
	}
	return nil
}

func validateChangePath(path string) error {
	if path == "" {
		return errors.New("changeset path is empty")
	}
	if filepath.IsAbs(path) || strings.HasPrefix(path, "/") || filepath.VolumeName(path) != "" || (len(path) >= 2 && path[1] == ':') {
		return fmt.Errorf("changeset path %q is absolute", path)
	}
	if strings.Contains(path, "\\") {
		return fmt.Errorf("changeset path %q contains a non-portable separator", path)
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
	if clean != path || clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("changeset path %q contains traversal", path)
	}
	for _, part := range strings.Split(path, "/") {
		if strings.EqualFold(part, ".git") {
			return fmt.Errorf("changeset path %q targets forbidden .git metadata", path)
		}
	}
	return nil
}

func validateSymlinkTarget(path, target string) error {
	if filepath.IsAbs(target) || filepath.VolumeName(target) != "" || (len(target) >= 2 && target[1] == ':') || strings.Contains(target, "\\") {
		return fmt.Errorf("changeset symlink %q escapes the worktree via target %q", path, target)
	}
	resolved := filepath.Clean(filepath.Join(filepath.Dir(filepath.FromSlash(path)), target))
	if resolved == ".." || strings.HasPrefix(resolved, ".."+string(filepath.Separator)) {
		return fmt.Errorf("changeset symlink %q escapes the worktree via target %q", path, target)
	}
	for _, part := range strings.Split(filepath.ToSlash(resolved), "/") {
		if strings.EqualFold(part, ".git") {
			return fmt.Errorf("changeset symlink %q targets forbidden .git metadata via %q", path, target)
		}
	}
	return nil
}

func verifyStaging(ctx context.Context, stage string, changes ChangeSet) error {
	actualRaw, err := gitBytes(ctx, stage, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--")
	if err != nil {
		return fmt.Errorf("list staged changeset paths: %w", err)
	}
	actual, err := porcelainPaths(actualRaw)
	if err != nil {
		return err
	}
	want := make([]string, 0, len(changes.Manifest))
	for _, entry := range changes.Manifest {
		want = append(want, entry.Path)
		state, err := inspectNode(stage, entry.Path)
		if err != nil {
			return err
		}
		if entry.Kind == ChangeDelete {
			if state.exists && !(state.mode == changeSetDirectoryMode && manifestHasDescendant(changes.Manifest, entry.Path)) {
				return fmt.Errorf("changeset path %q should be deleted in staging", entry.Path)
			}
			continue
		}
		if !state.exists || state.mode != entry.Mode || state.size != entry.Size || state.sha256 != entry.SHA256 {
			return fmt.Errorf("changeset path %q failed staged content verification", entry.Path)
		}
	}
	sort.Strings(actual)
	sort.Strings(want)
	if strings.Join(actual, "\x00") != strings.Join(want, "\x00") {
		return fmt.Errorf("changeset manifest paths %q differ from staged paths %q", want, actual)
	}
	return nil
}

func manifestHasDescendant(entries []ChangeManifestEntry, path string) bool {
	prefix := path + "/"
	for _, entry := range entries {
		if strings.HasPrefix(entry.Path, prefix) {
			return true
		}
	}
	return false
}

func porcelainPaths(raw []byte) ([]string, error) {
	fields := nulPaths(raw)
	paths := make([]string, 0, len(fields))
	for index := 0; index < len(fields); index++ {
		field := fields[index]
		if len(field) < 4 {
			return nil, fmt.Errorf("invalid git status record %q", field)
		}
		status := field[:2]
		path := field[3:]
		paths = append(paths, path)
		if strings.ContainsAny(status, "RC") {
			index++
			if index >= len(fields) {
				return nil, fmt.Errorf("invalid rename status record %q", field)
			}
			paths = append(paths, fields[index])
		}
	}
	sort.Strings(paths)
	return compactSorted(paths), nil
}

func inspectNode(root, path string) (nodeState, error) {
	reachable, err := changeSetPathReachable(root, path)
	if err != nil {
		return nodeState{}, err
	}
	if !reachable {
		return nodeState{}, nil
	}
	info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(path)))
	if errors.Is(err, fs.ErrNotExist) {
		return nodeState{}, nil
	}
	if err != nil {
		return nodeState{}, fmt.Errorf("inspect changeset path %q: %w", path, err)
	}
	if info.IsDir() {
		return nodeState{exists: true, mode: changeSetDirectoryMode}, nil
	}
	content, mode, err := readNode(root, path, info)
	if err != nil {
		return nodeState{}, err
	}
	sum := sha256.Sum256(content)
	return nodeState{exists: true, mode: mode, size: int64(len(content)), sha256: hex.EncodeToString(sum[:]), content: content}, nil
}

func changeSetPathReachable(root, path string) (bool, error) {
	current := root
	parts := strings.Split(path, "/")
	for _, part := range parts[:len(parts)-1] {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("inspect parent of changeset path %q: %w", path, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return false, fmt.Errorf("changeset path %q escapes through symlink parent %q", path, current)
		}
		if !info.IsDir() {
			return false, nil
		}
	}
	return true, nil
}

func sameNode(a, b nodeState) bool {
	return a.exists == b.exists && (!a.exists || (a.mode == b.mode && a.size == b.size && a.sha256 == b.sha256))
}

func materializationMatches(root, stage string, entries []ChangeManifestEntry, desired map[string]nodeState) (bool, error) {
	for _, entry := range entries {
		current, err := inspectNode(root, entry.Path)
		if err != nil {
			return false, err
		}
		if !sameNode(current, desired[entry.Path]) {
			return false, nil
		}
	}
	return structuralDirectoriesEqual(stage, root, entries, desired)
}

func structuralDirectoriesEqual(wantRoot, gotRoot string, entries []ChangeManifestEntry, desired map[string]nodeState) (bool, error) {
	for _, entry := range entries {
		if desired[entry.Path].mode != changeSetDirectoryMode {
			continue
		}
		want, wantDirectory, err := directoryDescendants(wantRoot, entry.Path)
		if err != nil {
			return false, err
		}
		got, gotDirectory, err := directoryDescendants(gotRoot, entry.Path)
		if err != nil {
			return false, err
		}
		if !wantDirectory || !gotDirectory || len(want) != len(got) {
			return false, nil
		}
		for path, wantState := range want {
			gotState, ok := got[path]
			if !ok || !sameNode(wantState, gotState) {
				return false, nil
			}
		}
	}
	return true, nil
}

func directoryDescendants(root, path string) (map[string]nodeState, bool, error) {
	directory := filepath.Join(root, filepath.FromSlash(path))
	info, err := os.Lstat(directory)
	if errors.Is(err, fs.ErrNotExist) || (err == nil && !info.IsDir()) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("inspect structural directory %q: %w", path, err)
	}
	states := make(map[string]nodeState)
	err = filepath.WalkDir(directory, func(full string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if full == directory {
			return nil
		}
		relative, err := filepath.Rel(directory, full)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		state, err := inspectNode(root, path+"/"+relative)
		if err != nil {
			return err
		}
		states[relative] = state
		return nil
	})
	if err != nil {
		return nil, false, fmt.Errorf("inspect descendants of structural directory %q: %w", path, err)
	}
	return states, true, nil
}

func materializeEntry(root, stage string, entry ChangeManifestEntry, createdDirs map[string]struct{}) error {
	path := filepath.Join(root, filepath.FromSlash(entry.Path))
	if entry.Kind == ChangeDelete {
		return removeNode(path)
	}
	if err := ensureParentDirs(root, entry.Path, createdDirs); err != nil {
		return err
	}
	staged, err := inspectNode(stage, entry.Path)
	if err != nil {
		return err
	}
	return replaceNode(path, staged)
}

func writeEntry(root string, entry ChangeManifestEntry) error {
	if err := ensureParentDirs(root, entry.Path, nil); err != nil {
		return err
	}
	return replaceNode(filepath.Join(root, filepath.FromSlash(entry.Path)), nodeState{
		exists: true, mode: entry.Mode, size: entry.Size, sha256: entry.SHA256, content: entry.Content,
	})
}

func replaceNode(path string, state nodeState) error {
	if err := removeNode(path); err != nil {
		return err
	}
	if state.mode == changeSetDirectoryMode {
		return os.Mkdir(path, 0o755)
	}
	if state.mode == changeSetSymlinkMode {
		return os.Symlink(string(state.content), path)
	}
	mode := fs.FileMode(0o644)
	if state.mode == changeSetExecutableMode {
		mode = 0o755
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".gitmoot-change-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(state.content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func removeNode(path string) error {
	err := os.Remove(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func ensureParentDirs(root, path string, created map[string]struct{}) error {
	current := root
	parts := strings.Split(path, "/")
	for _, part := range parts[:len(parts)-1] {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			if err := os.Mkdir(current, 0o755); err != nil {
				return err
			}
			if created != nil {
				created[current] = struct{}{}
			}
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("changeset path %q escapes through symlink parent %q", path, current)
		}
		if !info.IsDir() {
			return fmt.Errorf("changeset path %q has non-directory parent %q", path, current)
		}
	}
	return nil
}

func snapshotPaths(root string, entries []ChangeManifestEntry) (map[string]nodeState, error) {
	result := make(map[string]nodeState, len(entries))
	for _, entry := range entries {
		state, err := inspectNode(root, entry.Path)
		if err != nil {
			return nil, err
		}
		result[entry.Path] = state
	}
	return result, nil
}

func restorePaths(root string, entries []ChangeManifestEntry, backup map[string]nodeState, createdDirs, mutatedPaths map[string]struct{}) error {
	ordered := materializationOrder(entries)
	// Remove importer-owned leaf nodes first. Structural directories are pruned
	// in the next phase, after their materialized descendants are gone.
	for index := len(ordered) - 1; index >= 0; index-- {
		entry := ordered[index]
		if _, ok := mutatedPaths[entry.Path]; !ok {
			continue
		}
		path := filepath.Join(root, filepath.FromSlash(entry.Path))
		info, err := os.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect %q during rollback: %w", entry.Path, err)
		}
		if info.IsDir() {
			continue
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove %q during rollback: %w", entry.Path, err)
		}
	}
	dirs := make([]string, 0, len(createdDirs))
	for dir := range createdDirs {
		dirs = append(dirs, dir)
	}
	sort.Slice(dirs, func(a, b int) bool { return len(dirs[a]) > len(dirs[b]) })
	for _, dir := range dirs {
		info, err := os.Lstat(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		// Only directories created by this import belong to this pruning phase.
		if !info.IsDir() {
			continue
		}
		if err := os.Remove(dir); err != nil {
			return err
		}
	}
	// Restore ancestors before descendants when a directory was replaced by a
	// file. Reverse materialization order has exactly that property.
	for index := len(ordered) - 1; index >= 0; index-- {
		entry := ordered[index]
		if _, ok := mutatedPaths[entry.Path]; !ok {
			continue
		}
		state := backup[entry.Path]
		if !state.exists {
			continue
		}
		path := filepath.Join(root, filepath.FromSlash(entry.Path))
		if err := ensureParentDirs(root, entry.Path, nil); err != nil {
			return err
		}
		if err := replaceNode(path, state); err != nil {
			return fmt.Errorf("restore %q during rollback: %w", entry.Path, err)
		}
	}
	return nil
}

func materializationOrder(entries []ChangeManifestEntry) []ChangeManifestEntry {
	ordered := append([]ChangeManifestEntry(nil), entries...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Kind != ordered[j].Kind {
			return ordered[i].Kind == ChangeDelete
		}
		if ordered[i].Kind == ChangeDelete {
			return strings.Count(ordered[i].Path, "/") > strings.Count(ordered[j].Path, "/")
		}
		return strings.Count(ordered[i].Path, "/") < strings.Count(ordered[j].Path, "/")
	})
	return ordered
}

func createStagingWorktree(ctx context.Context, worktree, base string) (string, error) {
	parent := filepath.Dir(filepath.Clean(worktree))
	placeholder, err := os.MkdirTemp(parent, ".gitmoot-changeset-stage-")
	if err != nil {
		return "", fmt.Errorf("create changeset staging location: %w", err)
	}
	if err := os.Remove(placeholder); err != nil {
		return "", fmt.Errorf("prepare changeset staging location: %w", err)
	}
	cmd := exec.CommandContext(ctx, "git", "-C", worktree, "worktree", "add", "--detach", placeholder, base)
	if output, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("create changeset staging checkout: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return placeholder, nil
}

func removeStagingWorktree(ctx context.Context, worktree, stage string) error {
	cmd := exec.CommandContext(ctx, "git", "-C", worktree, "worktree", "remove", "--force", stage)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("remove changeset staging checkout %q: %w: %s", stage, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func lockHostHEAD(worktree string) (func() error, error) {
	ctx := context.Background()
	if format, err := gitOutput(ctx, worktree, "rev-parse", "--show-ref-format"); err == nil {
		format = strings.TrimSpace(format)
		// Older Git versions echo an unknown rev-parse option literally. They
		// predate reftable support, so that result still means the files backend.
		if format != "files" && format != "--show-ref-format" {
			return nil, fmt.Errorf("cannot lock host HEAD with unsupported git ref format %q", format)
		}
	}
	common, err := gitOutput(ctx, worktree, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return nil, fmt.Errorf("resolve host git common directory: %w", err)
	}
	gitDir, err := gitOutput(ctx, worktree, "rev-parse", "--path-format=absolute", "--git-dir")
	if err != nil {
		return nil, fmt.Errorf("resolve host git directory: %w", err)
	}
	ref, refErr := gitOutput(ctx, worktree, "symbolic-ref", "-q", "HEAD")
	lockPaths := []string{filepath.Join(strings.TrimSpace(gitDir), "HEAD.lock")}
	if refErr == nil {
		lockPaths = append(lockPaths, filepath.Join(strings.TrimSpace(common), filepath.FromSlash(strings.TrimSpace(ref)))+".lock")
	} else if !isExitCode(refErr, 1) {
		return nil, fmt.Errorf("resolve host HEAD ref: %w", refErr)
	}
	locked := make([]string, 0, len(lockPaths))
	unlock := func() error {
		var errs []error
		for index := len(locked) - 1; index >= 0; index-- {
			if err := os.Remove(locked[index]); err != nil && !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, fmt.Errorf("unlock host HEAD at %q: %w", locked[index], err))
			}
		}
		return errors.Join(errs...)
	}
	for _, lockPath := range lockPaths {
		if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
			_ = unlock()
			return nil, fmt.Errorf("prepare host HEAD lock: %w", err)
		}
		file, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			_ = unlock()
			return nil, fmt.Errorf("lock host HEAD at %q: %w", lockPath, err)
		}
		locked = append(locked, lockPath)
		if err := file.Close(); err != nil {
			_ = unlock()
			return nil, fmt.Errorf("close host HEAD lock: %w", err)
		}
	}
	return unlock, nil
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	content, err := gitBytes(ctx, dir, args...)
	return string(content), err
}

func gitBytes(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	output, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return output, nil
}

func isExitCode(err error, code int) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitCode() == code
}

func nulPaths(raw []byte) []string {
	parts := strings.Split(string(raw), "\x00")
	paths := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			paths = append(paths, part)
		}
	}
	return paths
}

func compactSorted(paths []string) []string {
	if len(paths) < 2 {
		return paths
	}
	out := paths[:1]
	for _, path := range paths[1:] {
		if path != out[len(out)-1] {
			out = append(out, path)
		}
	}
	return out
}

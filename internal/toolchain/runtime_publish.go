package toolchain

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// entrypointDigestName records, inside the engine-owned tree, the digest of the
// members that were published under that name.
const entrypointDigestName = ".staged-digest"

// launcherDirName holds the command a seat actually executes.
const launcherDirName = ".bin"

// stageSource is the set of open source descriptors a publish will use.
//
// ONE OPEN SET SERVES BOTH THE DIGEST AND THE COPY, which is what makes the
// published name describe the published bytes STRUCTURALLY rather than by a
// comparison (#1921 bridge F3, panel P2). The previous shape read the source
// twice - once to fingerprint, once to copy - so the name could describe a tree
// that was never published, and no amount of comparing could close that gap
// because both reads resolved names independently. Here the bytes are read from
// the same descriptors, rewound between passes; a source replaced in between
// cannot be observed by the second pass because there is no second resolution.
type stageSource struct {
	tree       *sourceTree
	members    []stagedMember
	entrypoint string
}

func openStageSource(boundary string, entrypoint string, packaged bool) (*stageSource, error) {
	return openStageSourceOwned(boundary, entrypoint, packaged, false)
}

// openStageSourceOwned opens a boundary, optionally marking it engine-owned so
// the operator-facing acceptance rules do not judge a tree this engine created.
func openStageSourceOwned(boundary string, entrypoint string, packaged bool, engineOwned bool) (*stageSource, error) {
	tree, err := openSourceTree(boundary)
	if err != nil {
		return nil, err
	}
	tree.engineOwned = engineOwned
	source := &stageSource{tree: tree, entrypoint: entrypoint}
	if packaged {
		members, err := tree.collectMembers(entrypoint)
		if err != nil {
			tree.close()
			return nil, err
		}
		source.members = members
		if !source.hasEntrypoint() {
			source.close()
			return nil, fmt.Errorf("%w: package boundary %q does not contain its entrypoint %q as an acceptable member", ErrRuntimeNotStageable, boundary, entrypoint)
		}
		return source, nil
	}
	handle, err := tree.openMember(entrypoint, false)
	if err != nil {
		tree.close()
		return nil, err
	}
	info, err := handle.Stat()
	if err != nil {
		_ = handle.Close()
		tree.close()
		return nil, fmt.Errorf("%w: stat %q: %v", ErrRuntimeNotStageable, entrypoint, err)
	}
	if err := acceptableMember(entrypoint, info, handle, entrypoint, engineOwned); err != nil {
		_ = handle.Close()
		tree.close()
		return nil, err
	}
	source.members = []stagedMember{{relative: entrypoint, file: handle, info: info}}
	return source, nil
}

func (s *stageSource) close() {
	if s == nil {
		return
	}
	closeMembers(s.members)
	s.tree.close()
}

func (s *stageSource) hasEntrypoint() bool {
	for _, member := range s.members {
		if member.relative == s.entrypoint {
			return true
		}
	}
	return false
}

// digest computes the length-framed digest of every member.
//
// FRAMING IS NOT DECORATION (#1921 panel P2). Concatenating name and bytes is
// ambiguous: a tree holding a="X", b="Y" produces the same stream as one holding
// only a="Xb\0Y", so layout could change between launches under the same
// published name with no SHA-256 collision. Every record now carries the byte
// length of its name and of its contents, so no two distinct trees share a
// stream.
func (s *stageSource) digest() (string, error) {
	sum := sha256.New()
	var scratch [8]byte
	binary.BigEndian.PutUint64(scratch[:], uint64(len(s.members)))
	if _, err := sum.Write(scratch[:]); err != nil {
		return "", err
	}
	for _, member := range s.members {
		if _, err := member.file.Seek(0, io.SeekStart); err != nil {
			return "", fmt.Errorf("%w: rewind %q: %v", ErrRuntimeNotStageable, member.relative, err)
		}
		binary.BigEndian.PutUint64(scratch[:], uint64(len(member.relative)))
		if _, err := sum.Write(scratch[:]); err != nil {
			return "", err
		}
		if _, err := io.WriteString(sum, member.relative); err != nil {
			return "", err
		}
		binary.BigEndian.PutUint64(scratch[:], uint64(member.info.Size()))
		if _, err := sum.Write(scratch[:]); err != nil {
			return "", err
		}
		written, err := io.Copy(sum, member.file)
		if err != nil {
			return "", fmt.Errorf("%w: hash %q: %v", ErrRuntimeNotStageable, member.relative, err)
		}
		if written != member.info.Size() {
			return "", fmt.Errorf("%w: %q changed size while being hashed (%d vs %d)", ErrRuntimeNotStageable, member.relative, written, member.info.Size())
		}
	}
	return hex.EncodeToString(sum.Sum(nil))[:16], nil
}

// copyInto writes every member into an already-created staging directory,
// reading from the SAME descriptors the digest used.
func (s *stageSource) copyInto(destination *os.Root, staging string) error {
	for _, member := range s.members {
		if _, err := member.file.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("%w: rewind %q: %v", ErrRuntimeNotStageable, member.relative, err)
		}
		target := filepath.Join(staging, member.relative)
		if parent := filepath.Dir(target); parent != "." {
			if err := destination.MkdirAll(parent, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
		}
		// EXECUTE BITS ARE PRESERVED because the copy must run; write bits are
		// not, so the staged tree is read-only to the seat that uses it.
		mode := os.FileMode(0o400)
		if member.info.Mode().Perm()&0o111 != 0 {
			mode = 0o500
		}
		written, err := destination.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if err != nil {
			return err
		}
		if _, err := io.Copy(written, member.file); err != nil {
			_ = written.Close()
			return err
		}
		if err := written.Close(); err != nil {
			return err
		}
	}
	return nil
}

// publishedDigest recomputes the length-framed digest of an ALREADY PUBLISHED
// tree using the same descriptor-bound, symlink-refusing traversal.
//
// This is what a rename loser and every reuse must satisfy (#1921 panel class 3
// and directive 122816): the previous code returned success after an Lstat of
// the winner's directory, so a seat could proceed against a tree this process
// never validated - the host-fallthrough the whole change exists to end.
func publishedDigest(publishedPath string, entrypoint string) (string, error) {
	source, err := openStageSource(publishedPath, entrypoint, true)
	if err != nil {
		return "", err
	}
	defer source.close()
	return source.digest()
}

// readRecordedDigests reads the two values recorded beside a published tree:
// the digest of its members, and the fingerprint it was published under. They
// differ when a script runtime's identity folds in its staged interpreter, and
// recording both is what lets reuse verify the bytes AND the claim without
// re-deriving either.
func readRecordedDigests(publishedPath string) (members string, fingerprint string, err error) {
	tree, err := openSourceTree(publishedPath)
	if err != nil {
		return "", "", err
	}
	defer tree.close()
	handle, err := tree.openMember(entrypointDigestName, false)
	if err != nil {
		return "", "", fmt.Errorf("%w: published tree %q records no digest", ErrRuntimeNotStageable, publishedPath)
	}
	defer handle.Close()
	recorded, err := io.ReadAll(io.LimitReader(handle, 128))
	if err != nil {
		return "", "", fmt.Errorf("%w: read recorded digest: %v", ErrRuntimeNotStageable, err)
	}
	lines := strings.Fields(string(recorded))
	if len(lines) != 2 {
		return "", "", fmt.Errorf("%w: published tree %q records %d digest value(s), want 2", ErrRuntimeNotStageable, publishedPath, len(lines))
	}
	return lines[0], lines[1], nil
}

// validatePublished re-proves a published tree before it is reused or accepted
// after losing a rename. It recomputes the length-framed digest of every
// published member with the same descriptor-bound, symlink-refusing traversal,
// requires it to equal the digest recorded at publish time, requires that
// record to equal the fingerprint being claimed, and requires the launcher to
// exist (directive 122816: no Lstat-only or name re-resolution path).
func validatePublished(publishedPath string, runtimeName string, entrypoint string, fingerprint string) error {
	recordedMembers, recordedFingerprint, err := readRecordedDigests(publishedPath)
	if err != nil {
		return err
	}
	if fingerprint != "" && recordedFingerprint != fingerprint {
		return fmt.Errorf("%w: published tree %q records fingerprint %q but %q was claimed", ErrRuntimeNotStageable, publishedPath, recordedFingerprint, fingerprint)
	}
	recomputed, err := publishedMembersDigest(publishedPath, entrypoint)
	if err != nil {
		return err
	}
	if recomputed != recordedMembers {
		return fmt.Errorf("%w: published tree %q hashes to %q but records %q", ErrRuntimeNotStageable, publishedPath, recomputed, recordedMembers)
	}
	launcher := filepath.Join(publishedPath, launcherDirName, runtimeName)
	info, err := os.Lstat(launcher)
	if err != nil || (!info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0) {
		return fmt.Errorf("%w: published launcher %q is missing", ErrRuntimeNotStageable, launcher)
	}
	return nil
}

// publishedMembersDigest hashes a published tree excluding engine metadata.
func publishedMembersDigest(publishedPath string, entrypoint string) (string, error) {
	tree, err := openSourceTree(publishedPath)
	if err != nil {
		return "", err
	}
	tree.engineOwned = true
	defer tree.close()
	members, err := tree.collectMembers(entrypoint)
	if err != nil {
		return "", err
	}
	defer closeMembers(members)
	kept := members[:0]
	for _, member := range members {
		if member.relative == entrypointDigestName || strings.HasPrefix(member.relative, launcherDirName+string(filepath.Separator)) {
			_ = member.file.Close()
			continue
		}
		kept = append(kept, member)
	}
	source := &stageSource{tree: tree, members: kept, entrypoint: entrypoint}
	return source.digest()
}

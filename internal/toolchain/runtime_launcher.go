package toolchain

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func shortDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:16]
}

// stageInterpreter returns the staged launcher of the interpreter this source's
// entrypoint needs, or "" when it needs none.
//
// THE SHEBANG IS READ FROM THE DESCRIPTOR THE COPY WILL USE, never by reopening
// the source by name (#1921 panel P2). The previous shape reopened the original
// path after copying, so the decision was taken on a file that could have
// changed, and any read failure was reported as "no interpreter needed" - which
// is fail-OPEN: it published a script whose interpreter the seat would then
// resolve from the inherited PATH.
//
// FOUR CASES, ALL EXPLICIT (directive 122816):
//   - env interpreter ("#!/usr/bin/env node"): the ARGUMENT is what must exist,
//     so that name is resolved and staged.
//   - direct interpreter ("#!/opt/py/bin/python3"): the absolute path is staged
//     and the launcher execs the STAGED copy, so the kernel never resolves the
//     original absolute shebang.
//   - missing or unreadable shebang: required-but-unstageable, which the caller
//     turns into an exit-126 command.
//   - no shebang at all (an ELF binary such as claude or kimi): no interpreter.
func (s *stageSource) stageInterpreter(gitmootHome string) (string, error) {
	var entrypoint *os.File
	for _, member := range s.members {
		if member.relative == s.entrypoint {
			entrypoint = member.file
		}
	}
	if entrypoint == nil {
		return "", fmt.Errorf("%w: entrypoint %q is not among the accepted members", ErrRuntimeNotStageable, s.entrypoint)
	}
	if _, err := entrypoint.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("%w: rewind entrypoint: %v", ErrRuntimeNotStageable, err)
	}
	header := make([]byte, 256)
	read, err := entrypoint.Read(header)
	if err != nil && read == 0 && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("%w: read entrypoint header: %v", ErrRuntimeNotStageable, err)
	}
	header = header[:read]
	if !strings.HasPrefix(string(header), "#!") {
		// A binary. Nothing to stage, and the launcher will be a direct link.
		return "", nil
	}
	line := string(header[2:])
	if end := strings.IndexAny(line, "\r\n"); end >= 0 {
		line = line[:end]
	} else if read == len(header) {
		// The shebang did not terminate inside the header we read, so we cannot
		// know what it names. Refusing beats guessing.
		return "", fmt.Errorf("%w: entrypoint shebang exceeds %d bytes and cannot be read completely", ErrRuntimeNotStageable, len(header))
	}
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return "", fmt.Errorf("%w: entrypoint declares an empty shebang", ErrRuntimeNotStageable)
	}
	candidate := fields[0]
	if filepath.Base(candidate) == "env" {
		if len(fields) < 2 {
			return "", fmt.Errorf("%w: entrypoint shebang %q names no interpreter", ErrRuntimeNotStageable, line)
		}
		candidate = fields[1]
	}

	target := candidate
	if !filepath.IsAbs(target) {
		located, lookErr := exec.LookPath(candidate)
		if lookErr != nil {
			return "", fmt.Errorf("%w: entrypoint needs interpreter %q, which cannot be resolved: %v", ErrRuntimeNotStageable, candidate, lookErr)
		}
		target = located
	}
	absolute, err := filepath.Abs(target)
	if err != nil {
		return "", fmt.Errorf("%w: interpreter %q: %v", ErrRuntimeNotStageable, target, err)
	}
	// A SYSTEM INTERPRETER STILL GETS AN EXPLICIT LAUNCHER, but no copy: the
	// sandbox grants those roots unconditionally, and copying /bin/sh would put
	// an engine copy of the shell ahead of the real one for no gain.
	if systemInterpreterRoots(absolute) {
		return absolute, nil
	}
	launcher, _, err := StageRuntime(gitmootHome, filepath.Base(candidate), absolute)
	return launcher, err
}

// publish copies the accepted members into the engine root and publishes them by
// rename, writing the recorded digest and the launcher before the rename so a
// partially built tree never acquires the published name.
func (s *stageSource) publish(root string, publishedName string, runtimeName string, entrypoint string, interpreter string, fingerprint string) error {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	destination, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer destination.Close()

	if err := checkFreeSpace(root); err != nil {
		return err
	}

	staging := publishedName + ".staging-" + randomSuffix()
	if err := destination.Mkdir(staging, 0o700); err != nil {
		return err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(filepath.Join(root, staging))
		}
	}()

	if err := s.copyInto(destination, staging); err != nil {
		return err
	}
	memberDigest, err := s.digest()
	if err != nil {
		return err
	}
	// BOTH VALUES ARE RECORDED: the member digest proves the bytes on reuse, and
	// the published fingerprint proves the claim - they differ when a script
	// runtime's identity folds in its staged interpreter.
	if err := writeEngineFile(destination, filepath.Join(staging, entrypointDigestName), memberDigest+"\n"+fingerprint+"\n", 0o400); err != nil {
		return err
	}
	if err := destination.Mkdir(filepath.Join(staging, launcherDirName), 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	published := filepath.Join(root, publishedName)
	if err := writeLauncher(destination, staging, published, runtimeName, entrypoint, interpreter); err != nil {
		return err
	}

	if err := destination.Rename(staging, publishedName); err != nil {
		// THE LOSER REVALIDATES IN FULL (directive 122816): descriptor-bound,
		// symlink-refusing, and the length-framed digest recomputed over every
		// published member. The previous code returned success after an Lstat of
		// the winner's directory, so a seat could proceed against a tree this
		// process never validated.
		if _, statErr := os.Lstat(published); statErr == nil {
			if validateErr := validatePublished(published, runtimeName, entrypoint, fingerprint); validateErr != nil {
				return fmt.Errorf("%w: lost the publish race and the winning tree does not validate: %v", ErrRuntimeNotStageable, validateErr)
			}
			return nil
		}
		return err
	}
	cleanup = false
	return nil
}

func writeEngineFile(destination *os.Root, target string, content string, mode os.FileMode) error {
	handle, err := destination.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(handle, content); err != nil {
		_ = handle.Close()
		return err
	}
	return handle.Close()
}

// writeLauncher writes the command a seat executes.
//
// FOR A SCRIPT IT IS AN EXPLICIT EXEC OF THE STAGED INTERPRETER (directive
// 122816), so the kernel never resolves the entrypoint's original absolute
// shebang - that path may not exist inside the seat's grants, and if it does it
// is the operator's copy, which is the exposure this change removes. Arguments
// are forwarded unchanged.
//
// For a binary the launcher is a relative symlink into the same published tree,
// which needs no shell and cannot point outside the grant.
func writeLauncher(destination *os.Root, staging string, published string, runtimeName string, entrypoint string, interpreter string) error {
	launcher := filepath.Join(staging, launcherDirName, runtimeName)
	if interpreter == "" {
		target, err := filepath.Rel(launcherDirName, entrypoint)
		if err != nil || strings.HasPrefix(target, ".."+string(filepath.Separator)+"..") {
			return fmt.Errorf("%w: entrypoint %q cannot be addressed from its launcher", ErrRuntimeNotStageable, entrypoint)
		}
		if err := destination.Symlink(target, launcher); err != nil {
			return fmt.Errorf("%w: create launcher: %v", ErrRuntimeNotStageable, err)
		}
		return nil
	}
	stagedEntrypoint := filepath.Join(published, entrypoint)
	script := "#!/bin/sh\nexec " + shellSingleQuote(interpreter) + " " + shellSingleQuote(stagedEntrypoint) + " \"$@\"\n"
	return writeEngineFile(destination, launcher, script, 0o500)
}

// shellSingleQuote quotes a path for /bin/sh. Staged paths are engine-generated,
// but a gitmoot home is operator-supplied and may contain anything.
func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

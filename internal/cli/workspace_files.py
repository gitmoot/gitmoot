# gitmoot workspace file operations (#2678). Runs inside the workspace VM as
# `python3 -c <confine + this> <config JSON | @config file>`, one process per
# operation, with workspace_confine.py prepended: every path is resolved from
# the identity-checked root, one component at a time, never through a
# symlink. The config names the operation (op) and its arguments; input
# bytes arrive inline (input_b64) or as a file the host uploaded under the
# guest home (input), which is unlinked as soon as it is open.
#
# Results go to stdout. A failure writes one line {"error":{"code","message"}}
# to stderr and exits 3 (a path that does not exist), 4 (a directory where a
# file was wanted), 5 (push: the branch has no commits past its base) or 2
# (anything else).
#
# Operations:
#   read     path           the file's bytes
#   write    path, mode?    input -> file, atomically (temp + rename)
#   stat     path           {"type","size","mtime"}
#   changes  since, state_dir, workspace
#                           {"reset","token","changed","deleted"}
#   pull     paths          a tar stream (files, directories, symlinks)
#   push     (input tar)    each entry applied atomically, modes kept
#   basediff base           NUL-separated paths whose content or mode differs
#                           from commit base (git diff), for mirror
#   bundle   branch, base   a git bundle of branch's commits past base
#   remove   paths          unlink files and symlinks (never followed), rmdir
#                           empty directories; {"removed": N}
import base64
import errno
import io
import json
import os
import secrets
import stat
import subprocess
import sys
import tarfile
import time

EXIT_MISSING = 3
EXIT_IS_DIR = 4
EXIT_NOTHING = 5
EXIT_FAILED = 2

# A file whose recorded ctime or mtime is this close to (or after) the start
# of the walk that recorded it may have changed since without any metadata
# moving: ctime advances on a coarse kernel clock, so a same-size write that
# restores the mtime within one tick leaves size, mtime, ctime and inode all
# equal. Such entries are reported again by the next diff, whatever their
# fingerprint (git's "racy clean" rule). changes may over-report; it must
# never under-report.
RACY_NS = 1000000000


class Failure(Exception):
    def __init__(self, exit_code, code, message):
        Exception.__init__(self, message)
        self.exit_code = exit_code
        self.code = code
        self.message = message


def fail(exit_code, code, message):
    raise Failure(exit_code, code, message)


def load_config():
    raw = sys.argv[1]
    if raw.startswith("@"):
        with open(raw[1:], "rb") as handle:
            data = handle.read()
        os.unlink(raw[1:])
        return json.loads(data)
    return json.loads(raw)


cfg = {}


def open_input():
    if cfg.get("input"):
        handle = open(cfg["input"], "rb")
        os.unlink(cfg["input"])
        return handle
    return io.BytesIO(base64.b64decode(cfg.get("input_b64") or ""))


def root_fd():
    return open_root(cfg["root"], cfg.get("root_dev"), cfg.get("root_ino"))


def write_all(fd, data):
    view = memoryview(data)
    while view:
        count = os.write(fd, view)
        view = view[count:]


def parent_and_name(relative, what, create=False):
    """(directory fd, final name) for relative; the root itself has no name."""
    parts = relative_parts(relative, what)
    try:
        dir_fd = open_dirs(root_fd(), parts[:-1], what, create=create)
    except Missing:
        if create:
            fail(EXIT_FAILED, "invalid_args", "a parent of %s is not a directory" % what)
        fail(EXIT_MISSING, "not_found", "%s does not exist in the workspace" % what)
    return dir_fd, (parts[-1] if parts else None)


def kind(mode):
    if stat.S_ISREG(mode):
        return "file"
    if stat.S_ISDIR(mode):
        return "dir"
    if stat.S_ISLNK(mode):
        return "symlink"
    return "other"


def open_regular(dir_fd, name, what):
    """Open a regular file for reading without following a symlink; a FIFO
    is never waited on."""
    try:
        fd = os.open(name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK | os.O_CLOEXEC, dir_fd=dir_fd)
    except FileNotFoundError:
        fail(EXIT_MISSING, "not_found", "%s does not exist in the workspace" % what)
    except OSError as error:
        if is_symlink_at(dir_fd, name):
            fail(EXIT_FAILED, "cwd_outside_root", "%s is a symlink; workspace paths never follow symlinks" % what)
        fail(EXIT_FAILED, "invalid_args", "%s cannot be opened: %s" % (what, error.strerror))
    info = os.fstat(fd)
    if stat.S_ISDIR(info.st_mode):
        os.close(fd)
        fail(EXIT_IS_DIR, "is_directory", "%s is a directory" % what)
    if not stat.S_ISREG(info.st_mode):
        os.close(fd)
        fail(EXIT_FAILED, "invalid_args", "%s is not a regular file" % what)
    return fd, info


def op_read():
    what = "path %s" % cfg["path"]
    dir_fd, name = parent_and_name(cfg["path"], what)
    if name is None:
        fail(EXIT_IS_DIR, "is_directory", "%s is the workspace root, a directory" % what)
    fd, _ = open_regular(dir_fd, name, what)
    while True:
        chunk = os.read(fd, 1 << 20)
        if not chunk:
            break
        write_all(1, chunk)


def op_stat():
    what = "path %s" % cfg["path"]
    dir_fd, name = parent_and_name(cfg["path"], what)
    try:
        info = os.fstat(dir_fd) if name is None else os.lstat(name, dir_fd=dir_fd)
    except (FileNotFoundError, NotADirectoryError):
        fail(EXIT_MISSING, "not_found", "%s does not exist in the workspace" % what)
    out = {"type": kind(info.st_mode), "size": info.st_size, "mtime": info.st_mtime_ns / 1e9}
    write_all(1, json.dumps(out).encode())


def place_file(dir_fd, name, source, mode, what):
    """Write source to name below dir_fd atomically: a temp file in the same
    directory, renamed over the old one. mode None keeps an existing file's
    mode, else 0o644. A symlink or directory there is refused."""
    try:
        existing = os.lstat(name, dir_fd=dir_fd)
    except FileNotFoundError:
        existing = None
    if existing is not None:
        if stat.S_ISLNK(existing.st_mode):
            fail(EXIT_FAILED, "cwd_outside_root", "%s is a symlink; workspace writes never replace or follow one" % what)
        if stat.S_ISDIR(existing.st_mode):
            fail(EXIT_IS_DIR, "is_directory", "%s is a directory" % what)
        if not stat.S_ISREG(existing.st_mode):
            fail(EXIT_FAILED, "invalid_args", "%s is not a regular file" % what)
        if mode is None:
            mode = stat.S_IMODE(existing.st_mode)
    if mode is None:
        mode = 0o644
    temp = ".gitmoot-write-" + secrets.token_hex(8)
    fd = os.open(temp, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW | os.O_CLOEXEC, 0o600, dir_fd=dir_fd)
    try:
        try:
            while True:
                chunk = source.read(1 << 20)
                if not chunk:
                    break
                write_all(fd, chunk)
            os.fchmod(fd, mode)
        finally:
            os.close(fd)
        os.rename(temp, name, src_dir_fd=dir_fd, dst_dir_fd=dir_fd)
    except BaseException as error:
        try:
            os.unlink(temp, dir_fd=dir_fd)
        except OSError:
            pass
        if isinstance(error, IsADirectoryError):
            fail(EXIT_IS_DIR, "is_directory", "%s is a directory" % what)
        raise


def op_write():
    what = "path %s" % cfg["path"]
    dir_fd, name = parent_and_name(cfg["path"], what, create=True)
    if name is None:
        fail(EXIT_IS_DIR, "is_directory", "%s is the workspace root, a directory" % what)
    mode = cfg.get("mode")
    with open_input() as source:
        place_file(dir_fd, name, source, None if mode is None else int(mode), what)


# changes


def encode_token(fields):
    raw = json.dumps(fields, separators=(",", ":")).encode()
    return "ws1." + base64.urlsafe_b64encode(raw).decode().rstrip("=")


def decode_token(token):
    if not isinstance(token, str) or not token.startswith("ws1."):
        return None
    body = token[4:]
    try:
        fields = json.loads(base64.urlsafe_b64decode(body + "=" * (-len(body) % 4)))
    except ValueError:
        return None
    if not isinstance(fields, dict):
        return None
    return fields


def read_generation(state_dir):
    """This VM's manifest generation, minted on first use. A new sandbox has
    a new home, so a new generation; a resync removes the file."""
    path = os.path.join(state_dir, "generation")
    try:
        with open(path) as handle:
            return int(handle.read().strip())
    except (OSError, ValueError):
        pass
    generation = secrets.randbelow(1 << 52) + 1
    temp = path + "." + secrets.token_hex(8)
    with open(temp, "w") as handle:
        handle.write("%d\n" % generation)
    try:
        os.link(temp, path)
    except FileExistsError:
        os.unlink(temp)
        with open(path) as handle:
            return int(handle.read().strip())
    os.unlink(temp)
    return generation


def walk_tree(top_fd):
    """Every entry below the root except the top-level .git, without
    following a symlink: rel -> [type, size, mtime_ns, ctime_ns, ino, mode]."""
    entries = {}
    stack = [("", top_fd)]
    while stack:
        prefix, fd = stack.pop()
        try:
            with os.scandir(fd) as listing:
                items = list(listing)
            for item in items:
                if prefix == "" and item.name == ".git":
                    continue
                try:
                    item.name.encode("utf-8")
                except UnicodeEncodeError:
                    continue  # not representable in the JSON contract
                rel = prefix + item.name
                try:
                    info = item.stat(follow_symlinks=False)
                except FileNotFoundError:
                    continue  # removed while walking
                entry_type = kind(info.st_mode)
                entries[rel] = [entry_type, info.st_size, info.st_mtime_ns, info.st_ctime_ns, info.st_ino, stat.S_IMODE(info.st_mode)]
                if entry_type != "dir":
                    continue
                try:
                    child = os.open(item.name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC, dir_fd=fd)
                except OSError:
                    continue  # replaced while walking; the next diff shows it
                opened = os.fstat(child)
                if (opened.st_dev, opened.st_ino) != (info.st_dev, info.st_ino):
                    os.close(child)
                    continue
                stack.append((rel + "/", child))
        finally:
            os.close(fd)
    return entries


def changed_since(previous, start_ns, rel, current):
    recorded = previous.get(rel)
    if recorded is None:
        return True
    if current[0] == "dir":
        # A directory's size and times move with its children, which are
        # reported themselves; only its identity and mode matter.
        return recorded[0] != "dir" or recorded[4] != current[4] or recorded[5] != current[5]
    if recorded != current:
        return True
    # Racily clean: recorded within RACY_NS of (or after) the start of the
    # walk that recorded it.
    return max(recorded[2], recorded[3]) >= start_ns - RACY_NS


def op_changes():
    state_dir = cfg["state_dir"]
    workspace = cfg["workspace"]
    os.makedirs(state_dir, 0o700, exist_ok=True)
    generation = read_generation(state_dir)
    # One manifest, the last issued token's, keyed by generation: a token
    # from another generation never finds it.
    manifest_path = os.path.join(state_dir, "manifest-%d.json" % generation)
    previous = None
    if cfg["since"] != "start":
        token = decode_token(cfg["since"])
        try:
            with open(manifest_path, "rb") as handle:
                stored = json.load(handle)
        except (OSError, ValueError):
            stored = None
        if (
            token is not None
            and stored is not None
            and token.get("w") == workspace
            and token.get("g") == generation
            and stored.get("w") == workspace
            and stored.get("g") == generation
            and token.get("t") == stored.get("t")
        ):
            previous = stored
    # Both clocks are read before the walk: t identifies the manifest,
    # started (wall clock, the clock ctime and mtime use) dates it for the
    # racy rule.
    mono = time.monotonic_ns()
    started = time.time_ns()
    entries = walk_tree(root_fd())
    fields = {"w": workspace, "g": generation, "t": mono}
    temp = manifest_path + "." + secrets.token_hex(8)
    with open(temp, "w") as handle:
        json.dump(dict(fields, s=started, e=entries), handle, separators=(",", ":"))
    os.rename(temp, manifest_path)

    changed = []
    for rel in sorted(entries):
        current = entries[rel]
        if previous is not None and not changed_since(previous["e"], previous["s"], rel, current):
            continue
        item = {"path": rel, "type": current[0], "size": current[1], "mtime": current[2] / 1e9, "mode": current[5]}
        changed.append(item)
    deleted = [] if previous is None else sorted(rel for rel in previous["e"] if rel not in entries)
    out = {
        "reset": cfg["since"] != "start" and previous is None,
        "token": encode_token(fields),
        "changed": changed,
        "deleted": deleted,
    }
    write_all(1, json.dumps(out, separators=(",", ":")).encode())


# pull and push


class Padded:
    """Exactly size bytes of fd: a file that shrank while being read is
    padded with zeros, one that grew is cut. Either way its ctime moved, so
    the next changes reports it again."""

    def __init__(self, fd, size):
        self.fd = fd
        self.left = size

    def read(self, count=-1):
        if count < 0 or count > self.left:
            count = self.left
        data = b""
        while len(data) < count:
            try:
                chunk = os.read(self.fd, count - len(data))
            except OSError:
                chunk = b""
            if not chunk:
                data += b"\0" * (count - len(data))
                break
            data += chunk
        self.left -= len(data)
        return data


def op_pull():
    out = tarfile.open(fileobj=sys.stdout.buffer, mode="w|", format=tarfile.PAX_FORMAT)
    for rel in cfg["paths"]:
        what = "path %s" % rel
        parts = relative_parts(rel, what)
        if not parts:
            continue
        try:
            dir_fd = open_dirs(root_fd(), parts[:-1], what)
        except Refused:
            continue  # missing, or behind a symlink: left out
        name = parts[-1]
        try:
            info = os.lstat(name, dir_fd=dir_fd)
            entry = tarfile.TarInfo("/".join(parts))
            entry.mode = stat.S_IMODE(info.st_mode)
            entry.mtime = info.st_mtime
            entry.uid = entry.gid = 0
            entry.uname = entry.gname = ""
            if stat.S_ISREG(info.st_mode):
                try:
                    fd, opened = open_regular(dir_fd, name, what)
                except Failure:
                    continue
                try:
                    entry.size = opened.st_size
                    entry.mode = stat.S_IMODE(opened.st_mode)
                    out.addfile(entry, Padded(fd, opened.st_size))
                finally:
                    os.close(fd)
            elif stat.S_ISDIR(info.st_mode):
                entry.type = tarfile.DIRTYPE
                out.addfile(entry)
            elif stat.S_ISLNK(info.st_mode):
                entry.type = tarfile.SYMTYPE
                entry.linkname = os.readlink(name, dir_fd=dir_fd)
                out.addfile(entry)
        except FileNotFoundError:
            pass
        finally:
            os.close(dir_fd)
    out.close()


def symlink_stays_inside(rel_parts, target):
    """A symlink at rel_parts may point only somewhere under the root."""
    if not target or "\0" in target:
        return False
    if os.path.isabs(target):
        root = os.path.normpath(cfg["root"])
        target = os.path.normpath(target)
        return target == root or target.startswith(root + "/")
    depth = len(rel_parts) - 1
    for part in target.split("/"):
        if part in ("", "."):
            continue
        if part == "..":
            depth -= 1
            if depth < 0:
                return False
        else:
            depth += 1
    return True


def push_parts(name):
    what = "entry %s" % name
    if "\0" in name or os.path.isabs(name):
        fail(EXIT_FAILED, "cwd_outside_root", "%s must be relative to the workspace root" % what)
    parts = [part for part in name.split("/") if part not in ("", ".")]
    if ".." in parts:
        fail(EXIT_FAILED, "cwd_outside_root", "%s contains .." % what)
    if not parts:
        fail(EXIT_FAILED, "invalid_args", "%s names the workspace root" % what)
    return parts, what


def op_push():
    applied = 0
    with open_input() as source:
        archive = tarfile.open(fileobj=source, mode="r|")
        for member in archive:
            parts, what = push_parts(member.name)
            if not (member.isreg() or member.isdir() or member.issym()):
                fail(EXIT_FAILED, "invalid_args", "%s is not a file, directory or symlink" % what)
            if member.issym() and not symlink_stays_inside(parts, member.linkname):
                fail(EXIT_FAILED, "cwd_outside_root", "%s points outside the workspace root (%s)" % (what, member.linkname))
            dir_fd = open_dirs(root_fd(), parts[:-1], what, create=True)
            name = parts[-1]
            mode = member.mode & 0o7777
            try:
                if member.isreg():
                    place_file(dir_fd, name, archive.extractfile(member), mode, what)
                elif member.isdir():
                    try:
                        os.mkdir(name, 0o700, dir_fd=dir_fd)
                    except FileExistsError:
                        pass
                    child = open_child(dir_fd, name, what)
                    try:
                        os.fchmod(child, mode)
                    finally:
                        os.close(child)
                else:
                    try:
                        existing = os.lstat(name, dir_fd=dir_fd)
                    except FileNotFoundError:
                        existing = None
                    if existing is not None and stat.S_ISDIR(existing.st_mode):
                        fail(EXIT_IS_DIR, "is_directory", "%s is a directory" % what)
                    temp = ".gitmoot-link-" + secrets.token_hex(8)
                    os.symlink(member.linkname, temp, dir_fd=dir_fd)
                    try:
                        os.rename(temp, name, src_dir_fd=dir_fd, dst_dir_fd=dir_fd)
                    except BaseException:
                        os.unlink(temp, dir_fd=dir_fd)
                        raise
            finally:
                os.close(dir_fd)
            applied += 1
    write_all(1, json.dumps({"applied": applied}).encode())


def remove_parts(rel):
    what = "path %s" % rel
    parts = relative_parts(rel, what)
    if not parts:
        fail(EXIT_FAILED, "invalid_args", "%s names the workspace root" % what)
    if parts[0] == ".git":
        fail(EXIT_FAILED, "invalid_args", "%s is in .git, which workspace remove never touches" % what)
    return parts, what


def check_removal(parts, what, planned):
    """Check one path before anything is removed: confinement (a symlink on
    the way is refused), and a directory must be empty once the paths listed
    before it in the batch are gone. Returns False for a missing path."""
    try:
        dir_fd = open_dirs(root_fd(), parts[:-1], what)
    except Missing:
        return False
    try:
        try:
            info = os.lstat(parts[-1], dir_fd=dir_fd)
        except FileNotFoundError:
            return False
        if stat.S_ISDIR(info.st_mode):
            child = open_child(dir_fd, parts[-1], what)
            with os.scandir(child) as listing:
                left = [item.name for item in listing if tuple(parts) + (item.name,) not in planned]
            os.close(child)
            if left:
                fail(EXIT_FAILED, "not_empty", "%s is a directory that is not empty (%s); list its entries before it" % (what, left[0]))
        return True
    finally:
        os.close(dir_fd)


def op_remove():
    # All or nothing: every path is checked (confinement, .git, non-empty
    # directories) before any is removed; the first failure removes nothing.
    targets = [remove_parts(rel) for rel in cfg["paths"]]
    planned = set()
    present = []
    for parts, what in targets:
        if check_removal(parts, what, planned):
            present.append((parts, what))
        planned.add(tuple(parts))
    removed = 0
    for parts, what in present:
        try:
            dir_fd = open_dirs(root_fd(), parts[:-1], what)
        except Missing:
            continue
        name = parts[-1]
        try:
            try:
                info = os.lstat(name, dir_fd=dir_fd)
                if stat.S_ISDIR(info.st_mode):
                    os.rmdir(name, dir_fd=dir_fd)
                else:
                    # A symlink is unlinked itself; its target is never touched.
                    os.unlink(name, dir_fd=dir_fd)
            except FileNotFoundError:
                continue  # listed twice, or removed meanwhile
            except OSError as error:
                if error.errno in (errno.ENOTEMPTY, errno.EEXIST):
                    fail(EXIT_FAILED, "not_empty", "%s is a directory that is not empty" % what)
                raise
            removed += 1
        finally:
            os.close(dir_fd)
    write_all(1, json.dumps({"removed": removed}).encode())



# commits out


def git(args, **kwargs):
    return subprocess.run(["git"] + args, stdin=subprocess.DEVNULL, **kwargs)


def enter_root():
    fd = root_fd()
    os.fchdir(fd)
    os.close(fd)


def op_basediff():
    enter_root()
    diff = git(["-c", "core.quotePath=false", "diff", "--no-renames", "--name-only", "-z", cfg["base"], "--"], stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    if diff.returncode != 0:
        fail(EXIT_FAILED, "internal", "git diff %s: %s" % (cfg["base"], diff.stderr.decode(errors="replace").strip()))
    write_all(1, diff.stdout)


def op_bundle():
    branch, base = cfg["branch"], cfg["base"]
    enter_root()
    head = git(["symbolic-ref", "-q", "HEAD"], stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
    current = head.stdout.decode(errors="replace").strip()
    if current != "refs/heads/" + branch:
        fail(EXIT_FAILED, "push_refused", "the workspace HEAD is %s, not the workspace branch %s; workspace push only pushes %s" % (current or "detached", branch, branch))
    tip = git(["rev-parse", "--verify", "--quiet", "refs/heads/%s^{commit}" % branch], stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
    if tip.returncode != 0:
        fail(EXIT_FAILED, "push_refused", "the workspace branch %s has no commit" % branch)
    if tip.stdout.decode().strip() == base:
        fail(EXIT_NOTHING, "nothing_to_push", "the workspace branch %s has no commits past its base %s" % (branch, base))
    sys.stdout.flush()
    created = git(["bundle", "create", "--quiet", "-", "refs/heads/" + branch, "^" + base], stdout=1, stderr=subprocess.PIPE)
    if created.returncode != 0:
        fail(EXIT_FAILED, "internal", "git bundle create: %s" % created.stderr.decode(errors="replace").strip())


OPS = {
    "read": op_read,
    "write": op_write,
    "stat": op_stat,
    "changes": op_changes,
    "pull": op_pull,
    "push": op_push,
    "basediff": op_basediff,
    "remove": op_remove,
    "bundle": op_bundle,
}


def report(exit_code, code, message):
    try:
        os.write(2, json.dumps({"error": {"code": code, "message": message}}).encode() + b"\n")
    finally:
        os._exit(exit_code)


def main():
    global cfg
    try:
        cfg = load_config()
        OPS[cfg["op"]]()
        sys.stdout.flush()
    except Failure as failure:
        report(failure.exit_code, failure.code, failure.message)
    except Refused as refusal:
        if isinstance(refusal, Missing):
            report(EXIT_MISSING, "not_found", refusal.message)
        report(EXIT_FAILED, refusal.code, refusal.message)
    except BrokenPipeError:
        report(EXIT_FAILED, "internal", "the host stopped reading")
    except OSError as error:
        if error.errno == errno.EACCES:
            report(EXIT_FAILED, "invalid_args", "permission denied: %s" % error)
        report(EXIT_FAILED, "internal", "%s" % error)
    except Exception as error:  # noqa: BLE001 - every failure is reported
        report(EXIT_FAILED, "internal", "%s: %s" % (type(error).__name__, error))
    os._exit(0)


# `python3 -c` runs this as __main__; a test may load the functions alone.
if __name__ == "__main__":
    main()

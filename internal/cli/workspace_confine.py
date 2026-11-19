# gitmoot workspace root confinement (#2678), shared by every script that
# runs inside the workspace VM: the exec supervisor and the file operations.
# The CLI prepends this file to each of them.
#
# The root is accepted only if its (st_dev, st_ino) matches the identity
# recorded at open; every path below it is then opened one component at a
# time from the root's descriptor without following symlinks. These stop
# accidental misuse (an argument naming a place outside the root); they are
# not a boundary against a hostile process already in the workspace, which
# runs as the owner of the root and every ancestor (see open_root).
import os
import stat


class Refused(Exception):
    """A path the workspace will not touch. code is a pinned error code."""

    def __init__(self, code, message):
        Exception.__init__(self, message)
        self.code = code
        self.message = message


class Missing(Refused):
    """A path component that does not exist or is not a directory."""

    def __init__(self, message):
        Refused.__init__(self, "invalid_args", message)


def is_symlink_at(fd, name):
    try:
        return stat.S_ISLNK(os.lstat(name, dir_fd=fd).st_mode)
    except OSError:
        return False


def open_child(fd, part, what):
    """Open one directory entry below fd without following a symlink."""
    try:
        return os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC, dir_fd=fd)
    except OSError as error:
        if is_symlink_at(fd, part):
            raise Refused("cwd_outside_root", "%s goes through a symlink; workspace paths never follow symlinks" % what)
        if isinstance(error, (FileNotFoundError, NotADirectoryError)):
            raise Missing("%s is not a directory in the workspace" % what)
        raise Refused("invalid_args", "%s is not a directory in the workspace" % what)


def open_root(root, root_dev, root_ino):
    """Open the workspace root and verify it by its recorded identity.

    The parent is opened by path as a best-effort hint: it follows whatever
    the path names now, including a parent that was replaced. The root's last
    component is opened from it with O_NOFOLLOW, and the result is accepted
    only if its (st_dev, st_ino) is the identity recorded at open. This stops
    accidental misuse; it is not a boundary against a process already running
    in the workspace (see the threat model in WORKSPACES.md).
    """
    root = os.path.normpath(root)
    parent, name = os.path.split(root)
    if not os.path.isabs(root) or name in ("", ".", ".."):
        raise Refused("invalid_args", "workspace root %s is not an absolute directory path" % root)
    parent_fd = os.open(parent, os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC)
    try:
        root_fd = open_child(parent_fd, name, "workspace root %s" % root)
    finally:
        os.close(parent_fd)
    # The path above is only a hint. What makes this the workspace root is its
    # identity, recorded when the workspace opened. This check and the
    # O_NOFOLLOW walk are defence in depth against accidental misuse (a stale
    # or mistyped argument, a root moved or replaced by mistake). They are NOT
    # a boundary against a hostile process already running in the workspace:
    # it runs as the user that owns the root and every ancestor, and can
    # defeat any path or identity check (for example by reclaiming the inode).
    # On E2B the VM is the isolation boundary; local has no isolation.
    expected = (int(root_dev or 0), int(root_ino or 0))
    if expected == (0, 0):
        os.close(root_fd)
        raise Refused("invalid_args", "the workspace root has no recorded identity")
    info = os.fstat(root_fd)
    if (info.st_dev, info.st_ino) != expected:
        os.close(root_fd)
        raise Refused("cwd_outside_root", "workspace root %s was replaced: it is not the directory the workspace opened" % root)
    return root_fd


def relative_parts(relative, what):
    """The components of a path relative to the root; never leaves it."""
    if "\0" in relative:
        raise Refused("invalid_args", "%s contains a NUL byte" % what)
    if os.path.isabs(relative):
        raise Refused("cwd_outside_root", "%s must be relative to the workspace root" % what)
    parts = []
    for part in relative.split("/"):
        if part in ("", "."):
            continue
        if part == "..":
            raise Refused("cwd_outside_root", "%s leaves the workspace root" % what)
        parts.append(part)
    return parts


def open_dirs(root_fd, parts, what, create=False):
    """Open the directory parts name below root_fd, one component at a time
    without following a symlink. root_fd is consumed (closed or returned).
    With create, missing directories are made (0o777 less the umask)."""
    fd = root_fd
    for part in parts:
        try:
            if create:
                try:
                    os.mkdir(part, 0o777, dir_fd=fd)
                except FileExistsError:
                    pass
            child = open_child(fd, part, what)
        except BaseException:
            os.close(fd)
            raise
        os.close(fd)
        fd = child
    return fd

# gitmoot workspace exec supervisor (#2678). Runs inside the workspace VM as
# `python3 -c <this> <config JSON>`; envd starts it with a stdin pipe that the
# host CLI feeds with SendInput. That pipe is the exec session's control
# channel, framed as [type:1][length:4 big-endian][payload]:
#   H  heartbeat, sent by the host every few seconds
#   D  bytes for the command's stdin (only with --stdin)
#   E  end of the command's stdin
#   K  kill: payload is one signal number for the command's process group
# The command runs in its own session and process group, with exactly the
# environment the host sent and stdin from /dev/null unless --stdin. When no
# frame arrives for `grace` seconds, or the channel closes, the host is gone
# (killed, SIGKILL included, or cut off) and the whole group is killed. A
# SIGTERM, SIGINT or SIGHUP to the supervisor itself also kills the group
# before the supervisor exits, so the watchdog never dies leaving the command
# behind. A forwarded signal escalates to SIGKILL after `kill_grace` seconds.
# The root is accepted only if its (st_dev, st_ino) matches the identity
# recorded at open; the cwd is then opened one component at a time below it
# without following symlinks, and the command starts in that pinned directory.
# These stop accidental misuse (a --cwd naming a place outside the root); they
# are not a boundary against a hostile process already in the workspace, which
# runs as the owner of the root and every ancestor (see open_root).
# Output is copied to the host and to a log file that `workspace close` copies
# out. After the group is dead the last bytes on stderr are a status trailer:
# MARK + JSON {exit_code, signal, timed_out, error} + newline. The supervisor
# exits with exit_code: the command's status, 128+n for signal n, 124 on
# timeout.
import json
import os
import signal
import stat
import struct
import subprocess
import sys
import threading
import time

MARK = b"\x1e\x1fgitmoot-ws-status "
LOG_CAP = 32 << 20

cfg = json.loads(sys.argv[1])
KILL_GRACE = float(cfg["kill_grace"])
lock = threading.Lock()
state = {"beat": time.monotonic(), "kill": None, "timed_out": False, "escalate_at": None}
# The last termination signal the supervisor itself received. Its handler only
# records it; the main loop acts on it, so a handler can never deadlock on lock.
received = []


def on_signal(signum, _frame):
    received.append(signum)


for _sig in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP):
    signal.signal(_sig, on_signal)


def finish(status):
    try:
        os.write(2, MARK + json.dumps(status).encode() + b"\n")
    finally:
        os._exit(status["exit_code"] & 0xFF)


def refuse(code, message):
    finish({"exit_code": 125, "signal": None, "timed_out": False, "error": {"code": code, "message": message}})


def open_child(fd, part, what):
    """Open one directory entry below fd without following a symlink."""
    try:
        return os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=fd)
    except FileNotFoundError:
        os.close(fd)
        refuse("invalid_args", "%s is not a directory in the workspace" % what)
    except OSError:
        try:
            is_link = stat.S_ISLNK(os.lstat(part, dir_fd=fd).st_mode)
        except OSError:
            is_link = False
        os.close(fd)
        if is_link:
            refuse("cwd_outside_root", "%s goes through a symlink; workspace paths never follow symlinks" % what)
        refuse("invalid_args", "%s is not a directory in the workspace" % what)


def open_root(root):
    """Open the workspace root itself without following a symlink.

    The root's parent (the guest home, or the local instance directory) sits
    in a directory workspace commands cannot write, so it cannot itself be
    swapped and is opened by path. Its entries can be: a command may replace
    the root with a symlink. So the root is opened from the parent's
    descriptor with O_NOFOLLOW, and a replaced root is refused rather than
    resolved. Nothing on the path is resolved by name and reopened.
    """
    root = os.path.normpath(root)
    parent, name = os.path.split(root)
    if not os.path.isabs(root) or name in ("", ".", ".."):
        refuse("invalid_args", "workspace root %s is not an absolute directory path" % root)
    parent_fd = os.open(parent, os.O_RDONLY | os.O_DIRECTORY)
    root_fd = open_child(parent_fd, name, "workspace root %s" % root)
    os.close(parent_fd)
    # The path above is only a hint. What makes this the workspace root is its
    # identity, recorded when the workspace opened. This check and the
    # O_NOFOLLOW walk are defence in depth against accidental misuse (a stale
    # or mistyped --cwd, a root moved or replaced by mistake). They are NOT a
    # boundary against a hostile process already running in the workspace: it
    # runs as the user that owns the root and every ancestor, and can defeat
    # any path or identity check (for example by reclaiming the inode). On E2B
    # the VM is the isolation boundary; local has no isolation.
    expected = (int(cfg.get("root_dev") or 0), int(cfg.get("root_ino") or 0))
    if expected == (0, 0):
        os.close(root_fd)
        refuse("invalid_args", "the workspace root has no recorded identity")
    info = os.fstat(root_fd)
    if (info.st_dev, info.st_ino) != expected:
        os.close(root_fd)
        refuse("cwd_outside_root", "workspace root %s was replaced: it is not the directory the workspace opened" % root)
    return root_fd


def open_cwd(root_fd, relative):
    """Open relative below root_fd without following any symlink."""
    fd = root_fd
    for part in relative.split("/"):
        if part in ("", "."):
            continue
        if part == "..":
            os.close(fd)
            refuse("cwd_outside_root", "cwd %s leaves the workspace root" % relative)
        child = open_child(fd, part, "cwd %s" % relative)
        os.close(fd)
        fd = child
    return fd


if os.path.isabs(cfg["cwd"]):
    refuse("cwd_outside_root", "cwd %s must be relative to the workspace root" % cfg["cwd"])
cwd_fd = open_cwd(open_root(cfg["root"]), cfg["cwd"])

log = None
log_size = 0
if cfg.get("log"):
    try:
        os.makedirs(os.path.dirname(cfg["log"]), exist_ok=True)
        log = open(cfg["log"], "ab")
        log.write(json.dumps({"argv": cfg["argv"], "cwd": cfg["cwd"], "started_at": time.time()}).encode() + b"\n")
    except OSError:
        log = None


def write_log(data):
    global log_size
    if log is None or log_size >= LOG_CAP:
        return
    with lock:
        chunk = data[: LOG_CAP - log_size]
        log.write(chunk)
        log_size += len(chunk)
        if log_size >= LOG_CAP:
            log.write(b"\n[gitmoot: log truncated at %d bytes]\n" % LOG_CAP)


if received:
    finish({"exit_code": 128 + received[-1], "signal": received[-1], "timed_out": False, "error": {"code": "supervisor_signal", "message": "the supervisor was signalled before the command started"}})
try:
    os.fchdir(cwd_fd)
    os.close(cwd_fd)
    proc = subprocess.Popen(
        cfg["argv"],
        env=cfg["env"],
        stdin=subprocess.PIPE if cfg["stdin"] else subprocess.DEVNULL,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        start_new_session=True,
    )
except FileNotFoundError as error:
    os.write(2, ("gitmoot workspace: %s: %s\n" % (cfg["argv"][0], error.strerror)).encode())
    finish({"exit_code": 127, "signal": None, "timed_out": False, "error": None})
except PermissionError as error:
    os.write(2, ("gitmoot workspace: %s: %s\n" % (cfg["argv"][0], error.strerror)).encode())
    finish({"exit_code": 126, "signal": None, "timed_out": False, "error": None})


def kill_group(sig, reason):
    with lock:
        if state["kill"] is None:
            state["kill"] = reason
            state["escalate_at"] = time.monotonic() + KILL_GRACE
    try:
        os.killpg(proc.pid, sig)
    except ProcessLookupError:
        pass


def pump(source, fd):
    while True:
        try:
            data = os.read(source.fileno(), 65536)
        except OSError:
            return
        if not data:
            return
        write_log(data)
        try:
            os.write(fd, data)
        except OSError:
            pass


stdin_queue = []
stdin_ready = threading.Condition()


def feed_stdin():
    while True:
        with stdin_ready:
            while not stdin_queue:
                stdin_ready.wait()
            item = stdin_queue.pop(0)
        try:
            if item is None:
                proc.stdin.close()
                return
            proc.stdin.write(item)
            proc.stdin.flush()
        except (BrokenPipeError, OSError, ValueError):
            return


def read_exact(count):
    data = b""
    while len(data) < count:
        chunk = os.read(0, count - len(data))
        if not chunk:
            return None
        data += chunk
    return data


def control():
    while True:
        header = read_exact(5)
        if header is None:
            kill_group(signal.SIGKILL, "control_closed")
            return
        kind, length = header[:1], struct.unpack(">I", header[1:])[0]
        payload = read_exact(length) if length else b""
        if payload is None:
            kill_group(signal.SIGKILL, "control_closed")
            return
        with lock:
            state["beat"] = time.monotonic()
        if kind == b"D" and proc.stdin is not None:
            with stdin_ready:
                stdin_queue.append(payload)
                stdin_ready.notify()
        elif kind == b"E" and proc.stdin is not None:
            with stdin_ready:
                stdin_queue.append(None)
                stdin_ready.notify()
        elif kind == b"K":
            kill_group(payload[0] if payload else signal.SIGTERM, "host_signal")


pumps = [
    threading.Thread(target=pump, args=(proc.stdout, 1), daemon=True),
    threading.Thread(target=pump, args=(proc.stderr, 2), daemon=True),
]
for thread in pumps:
    thread.start()
if proc.stdin is not None:
    threading.Thread(target=feed_stdin, daemon=True).start()
threading.Thread(target=control, daemon=True).start()

deadline = time.monotonic() + cfg["timeout"] if cfg["timeout"] > 0 else None
grace = cfg["grace"]
while True:
    try:
        proc.wait(timeout=0.05)
        break
    except subprocess.TimeoutExpired:
        pass
    now = time.monotonic()
    with lock:
        beat, escalate_at = state["beat"], state["escalate_at"]
    if received and state["kill"] is None:
        kill_group(signal.SIGTERM, "supervisor_signal")
    if now - beat > grace:
        kill_group(signal.SIGKILL, "watchdog")
    elif deadline is not None and now > deadline and state["kill"] is None:
        state["timed_out"] = True
        kill_group(signal.SIGTERM, "timeout")
    if escalate_at is not None and now > escalate_at:
        try:
            os.killpg(proc.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass

# Leave nothing of the command behind: whatever else is still in its group
# would hold the output pipes open and outlive the session.
try:
    os.killpg(proc.pid, signal.SIGKILL)
except ProcessLookupError:
    pass
for thread in pumps:
    thread.join(5)

code = proc.returncode
status = {"exit_code": code, "signal": None, "timed_out": state["timed_out"], "error": None}
if code < 0:
    status["signal"] = -code
    status["exit_code"] = 128 - code
if state["timed_out"]:
    status["exit_code"] = 124
if state["kill"] in ("watchdog", "control_closed"):
    status["error"] = {"code": state["kill"], "message": "the host session ended; the command's process group was killed"}
elif state["kill"] == "supervisor_signal":
    status["error"] = {"code": "supervisor_signal", "message": "the supervisor was signalled; the command's process group was killed"}
if log is not None:
    try:
        with lock:
            log.write(b"\n" + json.dumps(dict(status, ended_at=time.time())).encode() + b"\n")
            log.close()
    except OSError:
        pass
finish(status)

//go:build linux

package sandbox

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// A no-execution seat (#2364) may start exactly one program: its runtime. The
// mechanism is a one-time transition enforced from OUTSIDE the confined
// process, so nothing the runtime later does can re-open it:
//
//  1. The supervisor (sandbox-exec --no-execution) starts a child copy of
//     itself and keeps running as the runtime's parent.
//  2. The child applies the Landlock ruleset, installs a seccomp filter that
//     turns every execve and execveat into a user notification, hands the
//     notification descriptor to the supervisor, and execs the runtime.
//  3. The supervisor allows that first exec, which it verifies came from the
//     child as an execve, and answers every later exec from the runtime or any
//     descendant with EACCES. The decision is a count, not a path or
//     descriptor check, so no file, loader, descriptor number or re-exec of the
//     runtime itself can reach the allowed case again.
//
// The supervisor only reads notifications; it never inspects or trusts the
// confined process's memory.

// NoExecutionChildFD is the descriptor on which the child inherits the
// supervisor's socket.
const NoExecutionChildFD = 3

const (
	seccompRetAllow = 0x7fff0000
	seccompRetErrno = 0x00050000
	bpfLD           = 0x00
	bpfW            = 0x00
	bpfABS          = 0x20
	bpfJMP          = 0x05
	bpfJEQ          = 0x10
	bpfJGE          = 0x30
	bpfK            = 0x00
	bpfRET          = 0x06
	seccompDataNR   = 0
	seccompDataArch = 4
)

type seccompArch struct {
	audit    uint32
	execve   uint32
	execveat uint32
	setsid   uint32
	setpgid  uint32
	x32Bit   uint32 // amd64 only: numbers with this bit set are the x32 ABI
}

// seccompArches lists the architectures the no-execution sandbox has been
// proven on (#2364: amd64, kernel 6.8). Any other architecture fails closed
// until it is proven on real hardware; portability is not inferred.
var seccompArches = map[string]seccompArch{
	// Literal amd64 numbers: unix.SYS_* resolve to the BUILD architecture.
	"amd64": {audit: 0xc000003e, execve: 59, execveat: 322, setsid: 112, setpgid: 109, x32Bit: 0x40000000},
}

// seccompNotif and seccompNotifResp mirror struct seccomp_notif and
// struct seccomp_notif_resp (linux/seccomp.h); golang.org/x/sys does not
// export them.
type seccompNotif struct {
	ID    uint64
	Pid   uint32
	Flags uint32
	Nr    int32
	Arch  uint32
	IP    uint64
	Args  [6]uint64
}

type seccompNotifResp struct {
	ID    uint64
	Val   int64
	Error int32
	Flags uint32
}

func stmt(code uint16, k uint32) unix.SockFilter { return unix.SockFilter{Code: code, K: k} }

func jump(code uint16, k uint32, jt, jf uint8) unix.SockFilter {
	return unix.SockFilter{Code: code, Jt: jt, Jf: jf, K: k}
}

// noExecFilter sends every execve and execveat to the supervisor, fails every
// x32 syscall and any foreign architecture with EACCES, refuses setsid and
// setpgid with EPERM so no descendant can leave the seat's process group (the
// worker's group kill must reach all of them even if the supervisor is
// SIGKILLed), and allows the rest.
func noExecFilter(arch seccompArch) []unix.SockFilter {
	deny := seccompRetErrno | uint32(unix.EACCES)
	prog := []unix.SockFilter{
		stmt(bpfLD|bpfW|bpfABS, seccompDataArch),
		jump(bpfJMP|bpfJEQ|bpfK, arch.audit, 1, 0),
		stmt(bpfRET|bpfK, deny),
		stmt(bpfLD|bpfW|bpfABS, seccompDataNR),
	}
	if arch.x32Bit != 0 {
		prog = append(prog,
			jump(bpfJMP|bpfJGE|bpfK, arch.x32Bit, 0, 1),
			stmt(bpfRET|bpfK, deny),
		)
	}
	return append(prog,
		jump(bpfJMP|bpfJEQ|bpfK, arch.setsid, 1, 0),
		jump(bpfJMP|bpfJEQ|bpfK, arch.setpgid, 0, 1),
		stmt(bpfRET|bpfK, seccompRetErrno|uint32(unix.EPERM)),
		jump(bpfJMP|bpfJEQ|bpfK, arch.execve, 2, 0),
		jump(bpfJMP|bpfJEQ|bpfK, arch.execveat, 1, 0),
		stmt(bpfRET|bpfK, seccompRetAllow),
		stmt(bpfRET|bpfK, unix.SECCOMP_RET_USER_NOTIF),
	)
}

// NoExecutionSupported reports whether this host can run a no-execution seat.
// Supported scope is Linux on the architectures in seccompArches with a
// Landlock ABI new enough to drop Execute; anything else fails closed.
func NoExecutionSupported() error {
	if _, ok := seccompArches[runtime.GOARCH]; !ok {
		return fmt.Errorf("no-execution sandbox is not supported on linux/%s; proven only on linux/amd64", runtime.GOARCH)
	}
	return nil
}

// SuperviseNoExecution starts self with childArgs as the confined child, lets
// exactly one exec (the child starting the runtime) through, refuses every
// later exec, forwards termination signals, and returns the child's exit code.
func SuperviseNoExecution(self string, childArgs []string) (int, error) {
	if err := NoExecutionSupported(); err != nil {
		return 1, err
	}
	arch := seccompArches[runtime.GOARCH]
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return 1, fmt.Errorf("no-execution supervisor socket: %w", err)
	}
	parentEnd := os.NewFile(uintptr(pair[0]), "noexec-supervisor")
	childEnd := os.NewFile(uintptr(pair[1]), "noexec-child")
	defer parentEnd.Close()

	// Fail closed if the supervisor dies: the child (and so the runtime it
	// becomes) is SIGKILLed by the kernel on parent death. PDEATHSIG fires when
	// the creating THREAD exits, so that thread stays locked to this goroutine
	// for the supervisor's whole life.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	// Orphaned descendants of the runtime reparent here, so they can be
	// killed and reaped when the runtime exits instead of outliving the seat.
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		parentEnd.Close()
		childEnd.Close()
		return 1, fmt.Errorf("no-execution supervisor subreaper: %w", err)
	}
	cmd := exec.Command(self, childArgs...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.ExtraFiles = []*os.File{childEnd} // becomes NoExecutionChildFD
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		childEnd.Close()
		return 1, fmt.Errorf("start no-execution child: %w", err)
	}
	childEnd.Close()
	defer killAndReapDescendants()

	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT)
	defer signal.Stop(signals)
	go func() {
		for sig := range signals {
			_ = cmd.Process.Signal(sig)
		}
	}()

	listener, recvErr := receiveFD(int(parentEnd.Fd()))
	if recvErr == nil {
		go superviseExecs(listener, cmd.Process.Pid, arch)
	} else {
		// The child failed before handing over its listener, so it never
		// installed the filter and cannot exec anything: its error is on
		// stderr and its exit status is the result.
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	code := cmd.ProcessState.ExitCode()
	if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		code = 128 + int(status.Signal())
	}
	var exitErr *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exitErr) {
		return 1, waitErr
	}
	return code, nil
}

// killAndReapDescendants SIGKILLs every remaining child of the supervisor
// (orphans of the runtime reparent here as subreaper) and reaps them, until
// none is left. Run after the runtime exits on every path.
func killAndReapDescendants() {
	self := os.Getpid()
	for {
		killed := 0
		entries, _ := os.ReadDir("/proc")
		for _, entry := range entries {
			pid, err := strconv.Atoi(entry.Name())
			if err != nil || pid == self {
				continue
			}
			stat, err := os.ReadFile("/proc/" + entry.Name() + "/stat")
			if err != nil {
				continue
			}
			// ppid is the 2nd field after the parenthesised command name.
			fields := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+1:]))
			if len(fields) > 1 && fields[1] == strconv.Itoa(self) {
				_ = unix.Kill(pid, unix.SIGKILL)
				killed++
			}
		}
		for {
			if _, err := unix.Wait4(-1, nil, 0, nil); err != nil {
				break // ECHILD: nothing left to reap
			}
		}
		if killed == 0 {
			return
		}
	}
}

// superviseExecs answers seccomp exec notifications until no confined process
// remains. Only the first is allowed, and only if it is the child's execve.
func superviseExecs(listener int, childPid int, arch seccompArch) {
	defer unix.Close(listener)
	bootstrapped := false
	for {
		var req seccompNotif
		if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(listener), unix.SECCOMP_IOCTL_NOTIF_RECV, uintptr(unsafe.Pointer(&req))); errno != 0 {
			if errno == unix.EINTR {
				continue
			}
			return // ENOENT: every confined process has exited
		}
		resp := seccompNotifResp{ID: req.ID, Error: -int32(unix.EACCES)}
		if !bootstrapped && int(req.Pid) == childPid && uint32(req.Nr) == arch.execve {
			bootstrapped = true
			resp.Error = 0
			resp.Flags = unix.SECCOMP_USER_NOTIF_FLAG_CONTINUE
		}
		_, _, _ = unix.Syscall(unix.SYS_IOCTL, uintptr(listener), unix.SECCOMP_IOCTL_NOTIF_SEND, uintptr(unsafe.Pointer(&resp)))
	}
}

// execNoExecutionChild runs in the confined child after the Landlock ruleset
// is applied: it installs the exec-notify filter, hands the listener to the
// supervisor, and execs the runtime. The calling OS thread must be locked.
func execNoExecutionChild(executable string, argv, env []string) error {
	arch, ok := seccompArches[runtime.GOARCH]
	if !ok {
		return fmt.Errorf("no-execution sandbox is not supported on %s", runtime.GOARCH)
	}
	// Built before the filter, so nothing between filter and exec allocates.
	argvp, err := syscall.SlicePtrFromStrings(argv)
	if err != nil {
		return err
	}
	envp, err := syscall.SlicePtrFromStrings(env)
	if err != nil {
		return err
	}
	pathp, err := unix.BytePtrFromString(executable)
	if err != nil {
		return err
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("set no_new_privs: %w", err)
	}
	filter := noExecFilter(arch)
	program := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	listener, _, errno := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER,
		unix.SECCOMP_FILTER_FLAG_NEW_LISTENER, uintptr(unsafe.Pointer(&program)))
	if errno != 0 {
		return fmt.Errorf("install no-exec seccomp filter: %w", errno)
	}
	rights := unix.UnixRights(int(listener))
	if err := unix.Sendmsg(NoExecutionChildFD, []byte{0}, rights, nil, 0); err != nil {
		return fmt.Errorf("hand exec listener to supervisor: %w", err)
	}
	_ = unix.Close(int(listener))
	_ = unix.Close(NoExecutionChildFD)
	_, _, errno = unix.RawSyscall(unix.SYS_EXECVE, uintptr(unsafe.Pointer(pathp)),
		uintptr(unsafe.Pointer(&argvp[0])), uintptr(unsafe.Pointer(&envp[0])))
	return fmt.Errorf("exec runtime: %w", errno)
}

func receiveFD(socket int) (int, error) {
	buf := make([]byte, 1)
	oob := make([]byte, unix.CmsgSpace(4))
	_, oobn, _, _, err := unix.Recvmsg(socket, buf, oob, unix.MSG_CMSG_CLOEXEC)
	if err != nil {
		return -1, err
	}
	msgs, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil || len(msgs) != 1 {
		return -1, errors.New("no exec listener received from no-execution child")
	}
	fds, err := unix.ParseUnixRights(&msgs[0])
	if err != nil || len(fds) != 1 {
		return -1, errors.New("no exec listener received from no-execution child")
	}
	return fds[0], nil
}

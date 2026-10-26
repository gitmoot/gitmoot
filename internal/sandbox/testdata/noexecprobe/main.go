// Command noexecprobe is the runtime the no-execution sandbox kernel tests
// (#2364) start under `sandbox-exec --no-execution`. Each mode attempts one
// way of running a second program, or reports one property of the process,
// and prints a single result line the test asserts on.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: noexecprobe MODE ...")
		os.Exit(2)
	}
	args := os.Args[2:]
	switch os.Args[1] {
	case "read":
		data, err := os.ReadFile(args[0])
		report("read", err, strings.TrimSpace(string(data)))
	case "exec":
		report("exec", syscall.Exec(args[0], args, os.Environ()), "")
	case "self":
		self, err := os.Executable()
		if err == nil {
			err = syscall.Exec(self, []string{self, "read", "/proc/self/status"}, os.Environ())
		}
		report("self", err, "")
	case "execveat":
		fd, err := unix.Open(args[0], unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err == nil {
			path, _ := unix.BytePtrFromString("")
			argv, _ := slicePtr([]string{args[0]})
			envv, _ := slicePtr(os.Environ())
			_, _, errno := unix.Syscall6(unix.SYS_EXECVEAT, uintptr(fd), uintptr(unsafe.Pointer(path)),
				uintptr(unsafe.Pointer(&argv[0])), uintptr(unsafe.Pointer(&envv[0])), unix.AT_EMPTY_PATH, 0)
			err = errno
		}
		report("execveat", err, "")
	case "written":
		target := filepath.Join(args[0], "copied-true")
		data, err := os.ReadFile("/bin/true")
		if err == nil {
			err = os.WriteFile(target, data, 0o755)
		}
		if err == nil {
			err = syscall.Exec(target, []string{target}, os.Environ())
		}
		report("written", err, "")
	case "setsid":
		_, err := unix.Setsid()
		report("setsid", err, "")
	case "setpgid":
		report("setpgid", unix.Setpgid(0, 0), "")
	case "fds":
		entries, err := os.ReadDir("/proc/self/fd")
		var targets []string
		for _, entry := range entries {
			target, readErr := os.Readlink("/proc/self/fd/" + entry.Name())
			if readErr == nil {
				targets = append(targets, entry.Name()+"="+target)
			}
		}
		report("fds", err, strings.Join(targets, " "))
	case "exitcode":
		code, _ := strconv.Atoi(args[0])
		fmt.Println("EXIT", code)
		os.Exit(code)
	case "orphan":
		// A descendant that outlives the runtime: forked without exec (the
		// only kind a no-execution seat can make), it pauses forever.
		pid := rawFork(func() {
			for {
				unix.RawSyscall(unix.SYS_PAUSE, 0, 0, 0)
			}
		})
		writeFile(args[0], strconv.Itoa(pid))
		fmt.Println("OK orphan", pid)
	case "orphan-chain":
		// Two generations that outlive the runtime: A stays alive and has its
		// own child B, so B is reparented to the supervisor only after A is
		// killed. A forks with a raw syscall: ForkLock is held in the child.
		pid := rawFork(func() {
			if inner, _, _ := unix.RawSyscall(unix.SYS_FORK, 0, 0, 0); inner == 0 {
				for {
					unix.RawSyscall(unix.SYS_PAUSE, 0, 0, 0)
				}
			}
			for {
				unix.RawSyscall(unix.SYS_PAUSE, 0, 0, 0)
			}
		})
		fmt.Println("OK orphan-chain", pid)
	case "hold":
		// Writes its pid, forks a descendant that tries to exec /bin/true
		// after a delay (so after the supervisor may be gone), and waits.
		writeFile(args[0], strconv.Itoa(os.Getpid()))
		result, err := os.OpenFile(args[1], os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			report("hold", err, "")
			return
		}
		path, _ := unix.BytePtrFromString("/bin/true")
		argv, _ := slicePtr([]string{"/bin/true"})
		envv, _ := slicePtr(nil)
		delay := unix.Timespec{Sec: 1, Nsec: 500_000_000}
		var message [32]byte
		resultFD := result.Fd()
		childPid := rawFork(func() {
			unix.RawSyscall(unix.SYS_NANOSLEEP, uintptr(unsafe.Pointer(&delay)), 0, 0)
			_, _, errno := unix.RawSyscall(unix.SYS_EXECVE, uintptr(unsafe.Pointer(path)),
				uintptr(unsafe.Pointer(&argv[0])), uintptr(unsafe.Pointer(&envv[0])))
			n := copy(message[:], "DENIED errno=")
			n += copy(message[n:], strconv.Itoa(int(errno)))
			unix.RawSyscall(unix.SYS_WRITE, resultFD, uintptr(unsafe.Pointer(&message[0])), uintptr(n))
			unix.RawSyscall(unix.SYS_EXIT_GROUP, 0, 0, 0)
		})
		fmt.Println("OK hold child", childPid)
		time.Sleep(time.Minute)
	default:
		fmt.Println("unknown mode", os.Args[1])
		os.Exit(2)
	}
}

// rawFork forks the calling process without exec and runs child in the child,
// which must use raw syscalls only. It returns the child's pid in the parent.
func rawFork(child func()) int {
	syscall.ForkLock.Lock()
	pid, _, errno := unix.RawSyscall(unix.SYS_FORK, 0, 0, 0)
	if errno != 0 {
		syscall.ForkLock.Unlock()
		fmt.Println("FAIL fork", errno)
		os.Exit(1)
	}
	if pid == 0 {
		child()
		unix.RawSyscall(unix.SYS_EXIT_GROUP, 0, 0, 0)
	}
	syscall.ForkLock.Unlock()
	return int(pid)
}

func writeFile(path, content string) {
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		fmt.Println("FAIL write", err)
		os.Exit(1)
	}
}

// report prints "OK <mode> <detail>" or "DENIED <mode> errno=<n>" and exits
// non-zero on a denial, so a test reads one line per probe.
func report(mode string, err error, detail string) {
	if err == nil {
		fmt.Println("OK", mode, detail)
		return
	}
	if errno, ok := err.(syscall.Errno); ok {
		fmt.Printf("DENIED %s errno=%d\n", mode, int(errno))
		os.Exit(3)
	}
	var pathErr *os.PathError
	if pe, ok := err.(*os.PathError); ok {
		pathErr = pe
	}
	if pathErr != nil {
		if errno, ok := pathErr.Err.(syscall.Errno); ok {
			fmt.Printf("DENIED %s errno=%d\n", mode, int(errno))
			os.Exit(3)
		}
	}
	fmt.Printf("FAIL %s %v\n", mode, err)
	os.Exit(1)
}

// slicePtr builds a NULL-terminated C string array for a raw exec call.
func slicePtr(values []string) ([]*byte, error) {
	out := make([]*byte, 0, len(values)+1)
	for _, value := range values {
		ptr, err := unix.BytePtrFromString(value)
		if err != nil {
			return nil, err
		}
		out = append(out, ptr)
	}
	return append(out, nil), nil
}

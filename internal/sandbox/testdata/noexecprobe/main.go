// Command noexecprobe is the runtime the no-execution sandbox kernel tests
// (#2364) start under `sandbox-exec --no-execution`. Each mode attempts one
// way of running a second program, or reports one property of the process,
// and prints a single result line the test asserts on.
package main

import (
	"fmt"
	"io"
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
	case "fdwrite":
		// Writes through a raw descriptor number the runtime was never meant to
		// have (#2360 inherited-authority-descriptor case) and reports its
		// identity first, so a pass names exactly what was reachable.
		fd, _ := strconv.Atoi(args[0])
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			report("fdwrite", err, "")
			return
		}
		_, err := unix.Write(fd, []byte("WRITTEN-THROUGH-INHERITED-FD\n"))
		report("fdwrite", err, fmt.Sprintf("fd=%d dev=%d ino=%d uid=%d euid=%d", fd, st.Dev, st.Ino, os.Getuid(), os.Geteuid()))
	case "stdio":
		// Ordinary I/O must keep working: read stdin, write stdout and stderr.
		data, err := io.ReadAll(os.Stdin)
		if err == nil {
			_, err = fmt.Fprintf(os.Stderr, "stderr-ok\n")
		}
		report("stdio", err, strings.TrimSpace(string(data)))
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
		// own live child B. B sends its pid back through a pipe before the
		// runtime exits, so the run proves B exists; a failed inner fork is
		// reported instead of silently testing one generation.
		var p [2]int
		if err := unix.Pipe2(p[:], unix.O_CLOEXEC); err != nil {
			report("orphan-chain", err, "")
			return
		}
		var pidBytes [4]byte
		pidA := rawFork(func() {
			inner, _, errno := unix.RawSyscall(unix.SYS_FORK, 0, 0, 0)
			if errno != 0 {
				pidBytes = [4]byte{0xff, 0xff, 0xff, 0xff}
				unix.RawSyscall(unix.SYS_WRITE, uintptr(p[1]), uintptr(unsafe.Pointer(&pidBytes[0])), 4)
			} else if inner == 0 {
				self, _, _ := unix.RawSyscall(unix.SYS_GETPID, 0, 0, 0)
				pidBytes = [4]byte{byte(self), byte(self >> 8), byte(self >> 16), byte(self >> 24)}
				unix.RawSyscall(unix.SYS_WRITE, uintptr(p[1]), uintptr(unsafe.Pointer(&pidBytes[0])), 4)
			}
			for {
				unix.RawSyscall(unix.SYS_PAUSE, 0, 0, 0)
			}
		})
		_ = unix.Close(p[1])
		var got [4]byte
		if n, err := unix.Read(p[0], got[:]); err != nil || n != 4 {
			fmt.Println("FAIL orphan-chain no readiness from B", n, err)
			os.Exit(1)
		}
		if got == [4]byte{0xff, 0xff, 0xff, 0xff} {
			fmt.Println("FAIL orphan-chain inner fork failed")
			os.Exit(1)
		}
		pidB := int(got[0]) | int(got[1])<<8 | int(got[2])<<16 | int(got[3])<<24
		writeFile(args[0], fmt.Sprintf("%d %d", pidA, pidB))
		fmt.Println("OK orphan-chain", pidA, pidB)
	case "hold":
		// Writes its pid, forks a descendant that tries to exec /bin/true
		// after a delay (so after the supervisor may be gone), and waits.
		writeFile(args[0], strconv.Itoa(os.Getpid()))
		// A raw descriptor, not an *os.File: an os.File's finalizer may
		// close the descriptor the forked descendant later writes to.
		resultFD, err := unix.Open(args[1], unix.O_WRONLY|unix.O_CREAT|unix.O_TRUNC, 0o600)
		if err != nil {
			report("hold", err, "")
			return
		}
		path, _ := unix.BytePtrFromString("/bin/true")
		argv, _ := slicePtr([]string{"/bin/true"})
		envv, _ := slicePtr(nil)
		delay := unix.Timespec{Sec: 1, Nsec: 500_000_000}
		message := [16]byte{'D', 'E', 'N', 'I', 'E', 'D', ' ', 'e', 'r', 'r', 'n', 'o', '='}
		childPid := rawFork(func() {
			// Raw syscalls and no allocation only: this is a fork of a
			// multithreaded Go process.
			unix.RawSyscall(unix.SYS_NANOSLEEP, uintptr(unsafe.Pointer(&delay)), 0, 0)
			_, _, errno := unix.RawSyscall(unix.SYS_EXECVE, uintptr(unsafe.Pointer(path)),
				uintptr(unsafe.Pointer(&argv[0])), uintptr(unsafe.Pointer(&envv[0])))
			n := 13
			if errno >= 100 {
				message[n] = byte('0' + errno/100%10)
				n++
			}
			if errno >= 10 {
				message[n] = byte('0' + errno/10%10)
				n++
			}
			message[n] = byte('0' + errno%10)
			n++
			unix.RawSyscall(unix.SYS_WRITE, uintptr(resultFD), uintptr(unsafe.Pointer(&message[0])), uintptr(n))
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

//go:build linux

package agyaddon

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const maxAncestorWalkDepth = 10

// ProcessAlive reports whether pid currently exists.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// ProcessStartTime returns the kernel boot-relative start time (field 22 of
// /proc/<pid>/stat, in clock ticks) to distinguish a live process from a later
// process that reused the same PID.
func ProcessStartTime(pid int) (uint64, error) {
	_, _, startTime, err := readProcStat(pid)
	return startTime, err
}

// FindAncestorAgyPID walks the parent chain starting at startPID and returns
// the nearest ancestor whose /proc/<pid>/stat comm is "agy", along with its
// kernel start time.
func FindAncestorAgyPID(startPID int) (int, uint64, error) {
	pid := startPID
	for range maxAncestorWalkDepth {
		if pid <= 1 {
			break
		}
		comm, ppid, startTime, err := readProcStat(pid)
		if err != nil {
			return 0, 0, err
		}
		if comm == "agy" {
			return pid, startTime, nil
		}
		if ppid <= 1 || ppid == pid {
			break
		}
		pid = ppid
	}
	return 0, 0, fmt.Errorf("no ancestor agy process found from pid %d", startPID)
}

// readProcStat parses comm, ppid (field 4) and starttime (field 22) from
// /proc/<pid>/stat. Splitting on the last ')' handles process names containing
// spaces or parentheses safely.
func readProcStat(pid int) (comm string, ppid int, startTime uint64, err error) {
	if pid <= 0 {
		return "", 0, 0, fmt.Errorf("invalid pid %d", pid)
	}
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", 0, 0, err
	}
	line := strings.TrimSpace(string(raw))
	open := strings.IndexByte(line, '(')
	closeIdx := strings.LastIndexByte(line, ')')
	if open < 0 || closeIdx <= open || closeIdx+2 >= len(line) {
		return "", 0, 0, fmt.Errorf("malformed /proc/%d/stat", pid)
	}
	comm = line[open+1 : closeIdx]
	fields := strings.Fields(line[closeIdx+2:])
	// fields[0] is state (field 3), fields[1] is ppid (field 4),
	// fields[19] is starttime (field 22).
	if len(fields) <= 19 {
		return "", 0, 0, fmt.Errorf("truncated /proc/%d/stat", pid)
	}
	ppid, err = strconv.Atoi(fields[1])
	if err != nil {
		return "", 0, 0, fmt.Errorf("parse ppid in /proc/%d/stat: %w", pid, err)
	}
	startTime, err = strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return "", 0, 0, fmt.Errorf("parse starttime in /proc/%d/stat: %w", pid, err)
	}
	return comm, ppid, startTime, nil
}

// VerifyPIDOwnsLoopbackPort confirms that lsAddress is strictly loopback and
// that pid currently holds a TCP LISTEN socket bound to that loopback port.
// This prevents a stale registration from sending a CSRF token to an unrelated
// process that later bound the same ephemeral port.
func VerifyPIDOwnsLoopbackPort(pid int, lsAddress string) error {
	if pid <= 0 {
		return fmt.Errorf("invalid pid %d", pid)
	}
	_, port, err := ValidateLoopbackAddress(lsAddress)
	if err != nil {
		return err
	}
	inodes, err := listeningLoopbackInodes(port)
	if err != nil {
		return err
	}
	if len(inodes) == 0 {
		return fmt.Errorf("no loopback listener on %s", lsAddress)
	}
	fdDir := fmt.Sprintf("/proc/%d/fd", pid)
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", fdDir, err)
	}
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join(fdDir, entry.Name()))
		if err != nil {
			continue
		}
		if !strings.HasPrefix(target, "socket:[") || !strings.HasSuffix(target, "]") {
			continue
		}
		inode := target[len("socket:[") : len(target)-1]
		if inodes[inode] {
			return nil
		}
	}
	return fmt.Errorf("pid %d does not own loopback listener %s", pid, lsAddress)
}

func listeningLoopbackInodes(port uint16) (map[string]bool, error) {
	inodes := make(map[string]bool)
	var firstErr error
	parsedAny := false
	for _, spec := range []struct {
		path      string
		loopbacks map[string]bool
	}{
		{
			path:      "/proc/net/tcp",
			loopbacks: map[string]bool{"0100007F": true},
		},
		{
			path: "/proc/net/tcp6",
			loopbacks: map[string]bool{
				"00000000000000000000000001000000": true,
				"0000000000000000FFFF00000100007F": true,
			},
		},
	} {
		err := collectLoopbackListenInodes(spec.path, spec.loopbacks, port, inodes)
		if err == nil {
			parsedAny = true
		} else if firstErr == nil && !errors.Is(err, os.ErrNotExist) {
			firstErr = err
		}
	}
	if !parsedAny && firstErr != nil {
		return nil, firstErr
	}
	return inodes, nil
}

func collectLoopbackListenInodes(procNetPath string, loopbacks map[string]bool, port uint16, inodes map[string]bool) error {
	file, err := os.Open(procNetPath)
	if err != nil {
		return err
	}
	defer file.Close()
	wantPort := fmt.Sprintf("%04X", port)
	scanner := bufio.NewScanner(file)
	first := true
	for scanner.Scan() {
		if first {
			first = false
			continue
		}
		fields := strings.Fields(scanner.Text())
		// Fields: sl local_address rem_address st tx_queue:rx_queue tr:tm->when retrnsmt uid timeout inode ...
		if len(fields) < 10 {
			continue
		}
		// st == 0A is TCP_LISTEN.
		if !strings.EqualFold(fields[3], "0A") {
			continue
		}
		addrHex, portHex, ok := strings.Cut(fields[1], ":")
		if !ok || !strings.EqualFold(portHex, wantPort) {
			continue
		}
		if !loopbacks[strings.ToUpper(addrHex)] {
			continue
		}
		if fields[9] != "" && fields[9] != "0" {
			inodes[fields[9]] = true
		}
	}
	return scanner.Err()
}

// ValidateLoopbackAddress verifies that addr is host:port pointing strictly at
// localhost, 127.0.0.1, or ::1 with a valid non-zero port.
func ValidateLoopbackAddress(addr string) (string, uint16, error) {
	host, portStr, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		return "", 0, fmt.Errorf("invalid lsAddress %q: %w", addr, err)
	}
	lowerHost := strings.ToLower(strings.TrimSpace(host))
	if lowerHost != "localhost" && lowerHost != "127.0.0.1" && lowerHost != "::1" {
		return "", 0, fmt.Errorf("lsAddress %q is not loopback", addr)
	}
	portNum, err := strconv.Atoi(portStr)
	if err != nil || portNum <= 0 || portNum > 65535 {
		return "", 0, fmt.Errorf("invalid port in lsAddress %q", addr)
	}
	return lowerHost, uint16(portNum), nil
}

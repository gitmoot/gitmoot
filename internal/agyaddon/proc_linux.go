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

// PlatformSupported reports whether this OS supports agy process inspection
// and push notification delivery.
func PlatformSupported() error {
	return nil
}

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
// that pid currently holds a TCP LISTEN socket on that concrete loopback
// address and port. It returns the exact numeric host:port (`127.0.0.1:<port>`
// or `[::1]:<port>`) owned by pid so callers dial the verified endpoint rather
// than re-resolving "localhost".
func VerifyPIDOwnsLoopbackPort(pid int, lsAddress string) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("invalid pid %d", pid)
	}
	host, port, err := ValidateLoopbackAddress(lsAddress)
	if err != nil {
		return "", err
	}
	pidInodes, err := readPIDSockets(pid)
	if err != nil {
		return "", err
	}
	if len(pidInodes) == 0 {
		return "", fmt.Errorf("pid %d holds no sockets for %s", pid, lsAddress)
	}

	type candidate struct {
		concreteHost string
		procNetPath  string
		addrHexes    map[string]bool
	}
	var candidates []candidate
	switch host {
	case "127.0.0.1":
		candidates = []candidate{
			{concreteHost: "127.0.0.1", procNetPath: "/proc/net/tcp", addrHexes: map[string]bool{"0100007F": true}},
			{concreteHost: "127.0.0.1", procNetPath: "/proc/net/tcp6", addrHexes: map[string]bool{"0000000000000000FFFF00000100007F": true}},
		}
	case "::1":
		candidates = []candidate{
			{concreteHost: "::1", procNetPath: "/proc/net/tcp6", addrHexes: map[string]bool{"00000000000000000000000001000000": true}},
		}
	case "localhost":
		candidates = []candidate{
			{concreteHost: "127.0.0.1", procNetPath: "/proc/net/tcp", addrHexes: map[string]bool{"0100007F": true}},
			{concreteHost: "127.0.0.1", procNetPath: "/proc/net/tcp6", addrHexes: map[string]bool{"0000000000000000FFFF00000100007F": true}},
			{concreteHost: "::1", procNetPath: "/proc/net/tcp6", addrHexes: map[string]bool{"00000000000000000000000001000000": true}},
		}
	}

	var firstErr error
	parsedAny := false
	for _, cand := range candidates {
		inodes := make(map[string]bool)
		err := collectLoopbackListenInodes(cand.procNetPath, cand.addrHexes, port, inodes)
		if err != nil {
			if firstErr == nil && !errors.Is(err, os.ErrNotExist) {
				firstErr = err
			}
			continue
		}
		parsedAny = true
		for inode := range inodes {
			if pidInodes[inode] {
				return net.JoinHostPort(cand.concreteHost, strconv.Itoa(int(port))), nil
			}
		}
	}
	if !parsedAny && firstErr != nil {
		return "", firstErr
	}
	return "", fmt.Errorf("pid %d does not own loopback listener %s", pid, lsAddress)
}

func readPIDSockets(pid int) (map[string]bool, error) {
	fdDir := fmt.Sprintf("/proc/%d/fd", pid)
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		return nil, fmt.Errorf("inspect %s: %w", fdDir, err)
	}
	inodes := make(map[string]bool)
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join(fdDir, entry.Name()))
		if err != nil {
			continue
		}
		if !strings.HasPrefix(target, "socket:[") || !strings.HasSuffix(target, "]") {
			continue
		}
		inodes[target[len("socket:["):len(target)-1]] = true
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

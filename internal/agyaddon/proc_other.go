//go:build !linux

package agyaddon

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
)

var errUnsupportedPlatform = errors.New("agy runtime process inspection is only supported on Linux")

// ProcessAlive reports false on unsupported platforms.
func ProcessAlive(int) bool {
	return false
}

// ProcessStartTime returns an unsupported-platform error outside Linux.
func ProcessStartTime(int) (uint64, error) {
	return 0, errUnsupportedPlatform
}

// FindAncestorAgyPID returns an unsupported-platform error outside Linux.
func FindAncestorAgyPID(int) (int, uint64, error) {
	return 0, 0, errUnsupportedPlatform
}

// VerifyPIDOwnsLoopbackPort fails closed outside Linux after checking loopback syntax.
func VerifyPIDOwnsLoopbackPort(_ int, lsAddress string) error {
	if _, _, err := ValidateLoopbackAddress(lsAddress); err != nil {
		return err
	}
	return errUnsupportedPlatform
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

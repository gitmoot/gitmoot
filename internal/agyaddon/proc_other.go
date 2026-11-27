//go:build !linux

package agyaddon

import "errors"

var errUnsupportedPlatform = errors.New("agy process inspection and push delivery require Linux")

// PlatformSupported reports why agy push notifications are unavailable on non-Linux hosts.
func PlatformSupported() error {
	return errUnsupportedPlatform
}

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
func VerifyPIDOwnsLoopbackPort(_ int, lsAddress string) (string, error) {
	if _, _, err := ValidateLoopbackAddress(lsAddress); err != nil {
		return "", err
	}
	return "", errUnsupportedPlatform
}

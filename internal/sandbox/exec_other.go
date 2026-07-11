//go:build !linux

package sandbox

import (
	"errors"
)

const MinimumABI = 3

func Exec(_ []string, _ []string) error {
	return errors.New("Landlock sandboxing is supported only on Linux")
}

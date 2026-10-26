//go:build !linux

package sandbox

import (
	"errors"
)

const MinimumABI = 3

func ReadOnlyWorkdirSupported() bool {
	return false
}

func Exec(_ []string, _ []string, _ []string, _ []string) error {
	return errors.New("Landlock sandboxing is supported only on Linux")
}

func ExecReadOnlyWorkdir(_ []string, _ []string, _ []string, _ []string) error {
	return errors.New("Landlock sandboxing is supported only on Linux")
}

func ExecNoExecutionChild(_ []string, _ []string, _ []string, _ []string) error {
	return errors.New("Landlock sandboxing is supported only on Linux")
}

func NoExecutionSupported() error {
	return errors.New("no-execution sandbox is supported only on linux/amd64")
}

func SuperviseNoExecution(_ string, _ []string) (int, error) {
	return 1, errors.New("no-execution sandbox is supported only on Linux")
}

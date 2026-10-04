//go:build !linux

package ompaddon

import (
	"errors"
	"os"
	"os/exec"
)

func startInPTY(*exec.Cmd) (*os.File, error) {
	return nil, errors.New("the live check needs a Linux pseudo-terminal")
}

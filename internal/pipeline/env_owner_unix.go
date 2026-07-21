//go:build linux || darwin

package pipeline

import (
	"fmt"
	"os"
	"syscall"
)

var PipelineEnvCurrentUID = func() uint32 {
	return uint32(syscall.Geteuid())
}

func pipelineEnvOwnerUID(info os.FileInfo) (uint32, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("owner information unavailable")
	}
	return stat.Uid, nil
}

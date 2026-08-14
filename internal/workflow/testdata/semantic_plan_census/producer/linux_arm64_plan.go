//go:build linux && arm64

package producer

import "github.com/gitmoot/gitmoot/internal/runtime"

func LinuxARM64BuildContext() {
	_ = runtime.Job{Plan: true}
}

package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/doctor"
)

const remoteExecDoctorCheckName = "remote exec config"

func remoteExecDoctorCheck(paths config.Paths) (doctor.Check, bool) {
	cfg, err := config.LoadRemoteExecConfig(paths)
	if err == nil {
		return doctor.Check{
			Name:     remoteExecDoctorCheckName,
			OK:       true,
			Required: true,
			Detail:   fmt.Sprintf("[remote_exec] configuration valid (backend %s)", cfg.Backend),
		}, true
	}
	if errors.Is(err, os.ErrNotExist) {
		return doctor.Check{
			Name:     remoteExecDoctorCheckName,
			OK:       true,
			Required: true,
			Detail:   "[remote_exec] not configured; local backend defaults apply",
		}, true
	}
	return doctor.Check{
		Name:     remoteExecDoctorCheckName,
		Required: true,
		Detail:   fmt.Sprintf("invalid [remote_exec] configuration: %v", err),
	}, true
}

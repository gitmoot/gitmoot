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
		detail := fmt.Sprintf("[remote_exec] configuration valid (backend %s)", cfg.Backend)
		// Each declared sandboxd provider's OMP upload is checked here, so a
		// missing or wrong-architecture binary shows up before a review needs it.
		for _, provider := range config.RemoteExecSandboxdProviders() {
			if cfg.SandboxdProviders[provider] == nil {
				continue
			}
			view, err := cfg.ForProvider(provider)
			if err == nil {
				err = view.ValidateOMPExecutable()
			}
			if err != nil {
				return doctor.Check{
					Name:     remoteExecDoctorCheckName,
					Required: true,
					Detail:   fmt.Sprintf("invalid [remote_exec] configuration: %v", err),
				}, true
			}
			if view.OMPLinuxFile != "" {
				detail += fmt.Sprintf("; provider %s uploads linux/%s omp %s", provider, view.OMPGuestArch, view.OMPLinuxFile)
			} else {
				detail += fmt.Sprintf("; provider %s declared without an omp executable", provider)
			}
		}
		return doctor.Check{
			Name:     remoteExecDoctorCheckName,
			OK:       true,
			Required: true,
			Detail:   detail,
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

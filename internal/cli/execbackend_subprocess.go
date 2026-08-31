package cli

import (
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/execbackend"
	gitutil "github.com/gitmoot/gitmoot/internal/git"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/subprocess"
)

type hostJobSubprocessRunner struct {
	subprocess.ExecRunner
}

func (hostJobSubprocessRunner) grouped() subprocess.Runner {
	return subprocess.GroupRunner{}
}

type groupedJobSubprocessRunner interface {
	grouped() subprocess.Runner
}

func (w jobWorker) subprocessRunnerForJob(job db.Job) (subprocess.Runner, error) {
	payload, err := daemonJobPayload(job)
	if err != nil {
		return nil, err
	}
	name, present := payload.ExecBackendOverride()
	backend, _, err := daemonJobExecBackendFor(w, name, present)
	if err != nil {
		return nil, err
	}
	return jobSubprocessRunnerForBackend(backend)
}

// jobSubprocessRunnerForBackend is the single consumption seam for
// job-associated git, checkout, and verifier subprocesses. These operations are
// host-only for both backends; runtime delivery is the only instance-bound path.
func jobSubprocessRunnerForBackend(backend execbackend.Backend) (subprocess.Runner, error) {
	return execbackend.Consume(backend, func() (subprocess.Runner, error) {
		return hostJobSubprocessRunner{}, nil
	}, func() (subprocess.Runner, error) {
		return hostJobSubprocessRunner{}, nil
	})
}

func jobGroupedSubprocessRunner(runner subprocess.Runner) subprocess.Runner {
	if grouped, ok := runner.(groupedJobSubprocessRunner); ok {
		return grouped.grouped()
	}
	return runner
}

var _ subprocess.Runner = hostJobSubprocessRunner{}

func jobGitClient(dir string, runner subprocess.Runner) gitutil.Client {
	return gitutil.NewClient(dir, runner)
}

func jobGitHubClient(dir string, client github.Client, runner subprocess.Runner) github.Client {
	if gh, ok := client.(*github.GhClient); ok {
		return gh.WithRunner(dir, runner)
	}
	if client != nil {
		return client
	}
	return github.NewClientWithRunner(dir, runner)
}

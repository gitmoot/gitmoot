package remote

import (
	"context"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/execbackend"
)

// TestRemoteExecRefusesLaunchDir: a launch dir outside the workspace is the
// local no-execution seat's exception (#2563); no-execution never runs
// remotely (#2364), so a remote instance refuses it outright.
func TestRemoteExecRefusesLaunchDir(t *testing.T) {
	var backend Backend
	_, err := backend.Exec(context.Background(), &execbackend.Instance{ID: "remote"}, execbackend.Command{LaunchDir: "/tmp/launch", Name: "omp"})
	if err == nil || !strings.Contains(err.Error(), "does not support launch dir") {
		t.Fatalf("remote Exec with launch dir error = %v", err)
	}
}

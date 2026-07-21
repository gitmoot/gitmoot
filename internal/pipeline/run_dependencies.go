package pipeline

import (
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func pathsFromFlag(home string) (config.Paths, error) {
	if home != "" {
		return config.PathsForHome(home), nil
	}
	return config.DefaultPaths()
}

func keyConfigureCommand(name string) string {
	return fmt.Sprintf("gitmoot key configure %s --upstream <url> --auth bearer|header:<HeaderName>", name)
}

func writeLine(w io.Writer, format string, args ...any) {
	fmt.Fprintf(w, format+"\n", args...)
}

func pipelineRunnerAgentName(pipelineName string) string {
	return "pipeline-" + strings.TrimSpace(pipelineName) + "-runner"
}

func heartbeatJitter(jitter time.Duration) time.Duration {
	if jitter <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(int64(jitter) + 1))
}

func applyPipelineAutoMergePolicy(merger *workflow.PipelineAutoMerger, home string, repo string) {
	policy, ok := resolvedMergeGatePolicy(home, repo)
	if !ok {
		return
	}
	merger.RequireExternalCI = policy.RequireExternalCI
	merger.MinCIWait = policy.MinCIWait
	merger.MaxCIWait = policy.MaxCIWait
}

func resolvedMergeGatePolicy(home string, repo string) (config.MergeGatePolicy, bool) {
	cfg := resolveConfigFile(home)
	if cfg == "" {
		return config.MergeGatePolicy{}, false
	}
	loaded, err := config.LoadMergeGatePolicy(config.Paths{ConfigFile: cfg})
	if err != nil {
		return config.MergeGatePolicy{}, false
	}
	return loaded.For(repo), true
}

func resolveConfigFile(home string) string {
	home = strings.TrimSpace(home)
	if home == "" {
		return ""
	}
	cfg := filepath.Join(home, config.ConfigName)
	if _, err := os.Stat(cfg); err != nil {
		cfg = config.PathsForHome(home).ConfigFile
	}
	return cfg
}

func pipelineEventTime(value string) time.Time {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05"} {
		if parsed, err := time.Parse(layout, strings.TrimSpace(value)); err == nil {
			return parsed.UTC()
		}
	}
	return time.Time{}
}

func pipelineStageSourceBoundReviewRequest(request workflow.JobRequest) bool {
	return request.Sender == workflow.PipelineJobSender &&
		strings.TrimSpace(request.Action) == "review" &&
		request.PullRequest > 0
}

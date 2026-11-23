package remote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/gitmoot/gitmoot/internal/execbackend"
	"github.com/gitmoot/gitmoot/internal/execbackend/e2b"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// Only the instance stores repository objects. The host streams the bounded
// source identity files; it neither checks out nor bundles the repository.
const prepareReviewSourceScript = `set -eu
source_dir="$(pwd)/.gitmoot-source"
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL="$source_dir/gitconfig" GIT_TERMINAL_PROMPT=0
# git enables some traces when the variable is merely set (GIT_CURL_VERBOSE=0
# still traces curl headers), so unset them; stderr reaches the job error.
unset GIT_TRACE GIT_CURL_VERBOSE GIT_TRACE_CURL GIT_TRACE_CURL_NO_DATA GIT_TRACE_PACKET GIT_TRACE_PACKFILE GIT_TRACE_PERFORMANCE GIT_TRACE_SETUP GIT_TRACE_SHALLOW GIT_TRACE2 GIT_TRACE2_EVENT GIT_TRACE2_PERF GIT_TRACE_REDACT
step=setup
trap 'status=$?; rm -rf "$source_dir"; if [ "$status" -ne 0 ]; then printf "gitmoot-source: step %s failed (exit %s)\n" "$step" "$status" >&2; fi' EXIT
head=$1
target=$2
explicit=$3
merged=$4
rm -rf workspace
mkdir workspace
cd workspace
git init -q --template=
if [ -n "$explicit" ]; then
  step=fetch-head-and-diff-base
  git fetch --quiet --no-tags --no-recurse-submodules source "$head" "$explicit"
  step=diff-base-is-ancestor
  git merge-base --is-ancestor "$explicit" "$head"
  base=$explicit
else
  step=base-ref-format
  git check-ref-format "refs/heads/$target"
  step=fetch-head-and-base-ref
  git fetch --quiet --no-tags --no-recurse-submodules source "$head" "refs/heads/$target:refs/remotes/source/base"
  step=merge-base
  base=$(git merge-base refs/remotes/source/base "$head")
  if [ "$base" = "$head" ] && [ -n "$merged" ]; then
    step=fetch-merged
    git fetch --quiet --no-tags --no-recurse-submodules source "$merged"
    if git cat-file -e "$merged^2" 2>/dev/null; then
      base=$(git merge-base "$merged^1" "$head")
    fi
  fi
fi
step=verify-head
test "$(git rev-parse "$head^{commit}")" = "$head"
step=verify-base
git cat-file -e "$base^{commit}"
step=checkout
git checkout --quiet -B gitmoot-head "$head"
git update-ref refs/heads/gitmoot-review-base "$base"
git config branch.gitmoot-head.remote .
git config branch.gitmoot-head.merge refs/heads/gitmoot-review-base
step=verify-checkout
test "$(git rev-parse HEAD)" = "$head"
test "$(git rev-parse '@{upstream}')" = "$base"
printf '%s\n' "$head"
`

func reviewSourceCommit(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func (b *Backend) syncRemoteReview(ctx context.Context, instance *execbackend.Instance, state *sandboxState, source *execbackend.RemoteReviewSource) error {
	if !reviewSourceCommit(source.Head) || strings.TrimSpace(source.BaseRef) == "" || source.DiffBase != "" && !reviewSourceCommit(source.DiffBase) || source.MergedSHA != "" && !reviewSourceCommit(source.MergedSHA) {
		return errors.New("remote review source requires exact head and base commit identities")
	}
	access := source.Access
	if len(access.CACertificate) == 0 || len(access.ClientCertificate) == 0 || len(access.ClientPrivateKey) == 0 || len(access.ClientConfig) == 0 {
		return errors.New("remote review source requires its job-scoped mTLS identity")
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if _, err := runEnvd(ctx, state.envd, e2b.StartRequest{Name: "mkdir", Args: []string{"-p", ".gitmoot-source"}, Dir: "/home/user", MaxOutputBytes: 256}); err != nil {
		return err
	}
	if _, err := runEnvd(ctx, state.envd, e2b.StartRequest{Name: "chmod", Args: []string{"0700", ".gitmoot-source"}, Dir: "/home/user", MaxOutputBytes: 256}); err != nil {
		return err
	}
	files := []struct {
		name string
		data []byte
	}{
		{"ca.pem", access.CACertificate}, {"client.pem", access.ClientCertificate},
		{"client.key", access.ClientPrivateKey}, {"gitconfig", access.ClientConfig},
	}
	for _, file := range files {
		if err := state.envd.Upload(ctx, execbackend.ReviewSourceMaterialDir+"/"+file.name, bytes.NewReader(file.data)); err != nil {
			return fmt.Errorf("upload remote source identity %s: %w", file.name, err)
		}
	}
	if _, err := runEnvd(ctx, state.envd, e2b.StartRequest{Name: "sh", Args: []string{"-c", "chmod 0600 .gitmoot-source/*"}, Dir: "/home/user", MaxOutputBytes: 256}); err != nil {
		return err
	}
	result, err := runEnvd(ctx, state.envd, e2b.StartRequest{
		Name: "sh", Args: []string{"-c", prepareReviewSourceScript, "gitmoot-source", source.Head, source.BaseRef, source.DiffBase, source.MergedSHA},
		Dir: "/home/user", MaxOutputBytes: 4096,
	})
	if err != nil {
		// The script names its failing step on stderr (#2736); without it a bare
		// "exit status 128" can't tell a fetch failure from a missing commit.
		if tail := workflow.RedactedStderrTail(result.Stderr); tail != "" {
			return fmt.Errorf("prepare exact remote review source: %w: %s", err, tail)
		}
		return fmt.Errorf("prepare exact remote review source: %w", err)
	}
	if strings.TrimSpace(result.Stdout) != source.Head {
		return errors.New("remote review source did not establish the exact requested head")
	}
	state.hostBase = source.Head
	instance.BaseHEAD = source.Head
	return nil
}

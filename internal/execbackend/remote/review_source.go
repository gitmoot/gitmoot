package remote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/gitmoot/gitmoot/internal/execbackend"
	"github.com/gitmoot/gitmoot/internal/execbackend/e2b"
)

// Only the instance stores repository objects. The host streams the bounded
// source identity files; it neither checks out nor bundles the repository.
const prepareReviewSourceScript = `set -eu
source_dir="$(pwd)/.gitmoot-source"
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL="$source_dir/gitconfig" GIT_TERMINAL_PROMPT=0
export GIT_TRACE=0 GIT_CURL_VERBOSE=0
trap 'rm -rf "$source_dir"' EXIT
head=$1
target=$2
explicit=$3
merged=$4
rm -rf workspace
mkdir workspace
cd workspace
git init -q --template=
if [ -n "$explicit" ]; then
  git fetch --quiet --no-tags --no-recurse-submodules source "$head" "$explicit"
  git merge-base --is-ancestor "$explicit" "$head"
  base=$explicit
else
  git check-ref-format "refs/heads/$target"
  git fetch --quiet --no-tags --no-recurse-submodules source "$head" "refs/heads/$target:refs/remotes/source/base"
  base=$(git merge-base refs/remotes/source/base "$head")
  if [ "$base" = "$head" ] && [ -n "$merged" ]; then
    git fetch --quiet --no-tags --no-recurse-submodules source "$merged"
    if git cat-file -e "$merged^2" 2>/dev/null; then
      base=$(git merge-base "$merged^1" "$head")
    fi
  fi
fi
test "$(git rev-parse "$head^{commit}")" = "$head"
git cat-file -e "$base^{commit}"
git checkout --quiet -B gitmoot-head "$head"
git update-ref refs/heads/gitmoot-review-base "$base"
git config branch.gitmoot-head.remote .
git config branch.gitmoot-head.merge refs/heads/gitmoot-review-base
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
		return fmt.Errorf("prepare exact remote review source: %w", err)
	}
	if strings.TrimSpace(result.Stdout) != source.Head {
		return errors.New("remote review source did not establish the exact requested head")
	}
	state.hostBase = source.Head
	state.hostWorktree = "" // No host source exists; changeset import is not a review operation.
	instance.BaseHEAD = source.Head
	return nil
}
